package discovery

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/cel-go/cel"

	v2 "ocm.software/open-component-model/bindings/go/descriptor/v2"
	"ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
	ocmcel "ocm.software/open-component-model/kubernetes/controller/internal/cel"
	celconv "ocm.software/open-component-model/kubernetes/controller/internal/controller/resource/conversion"
)

// applyExtract projects the discovered graph according to spec.extract.
// It returns the value that should be written verbatim to status.discovery.
func applyExtract(ctx context.Context, descs []*v2.Descriptor, extract *v1alpha1.Extract) (any, error) {
	switch {
	case len(extract.ByResources) > 0:
		return extractPerResource(ctx, descs, extract.ByResources)
	case len(extract.ByComponents) > 0:
		return extractPerComponent(ctx, descs, extract.ByComponents)
	case extract.Expression != "":
		return extractExpression(ctx, descs, extract.Expression)
	default:
		return nil, fmt.Errorf("extract requires one of byResources, byComponents, expression")
	}
}

// extractPerResource evaluates each field expression once per resource of each
// component. Bindings: `resource` (current resource, v2 wire format) and
// `component` (enclosing component descriptor).
func extractPerResource(ctx context.Context, descs []*v2.Descriptor, fields map[string]string) ([]map[string]any, error) {
	baseEnv, err := ocmcel.SharedEnv()
	if err != nil {
		return nil, fmt.Errorf("cel env: %w", err)
	}
	env, err := baseEnv.Extend(
		cel.Variable("resource", cel.DynType),
		cel.Variable("component", cel.DynType),
	)
	if err != nil {
		return nil, fmt.Errorf("extend cel env: %w", err)
	}

	programs, err := compileFields(env, fields, "resource, component")
	if err != nil {
		return nil, err
	}

	var out []map[string]any
	for _, desc := range descs {
		compMap, err := toGenericMapViaJSON(desc.Component)
		if err != nil {
			return nil, fmt.Errorf("marshal component %s: %w", desc.Component.Name, err)
		}
		for _, res := range desc.Component.Resources {
			resMap, err := toGenericMapViaJSON(res)
			if err != nil {
				return nil, fmt.Errorf("marshal resource %s in %s: %w", res.Name, desc.Component.Name, err)
			}
			entry, err := evalFields(ctx, programs, map[string]any{
				"resource":  resMap,
				"component": compMap,
			})
			if err != nil {
				return nil, err
			}
			out = append(out, entry)
		}
	}
	return out, nil
}

// extractPerComponent evaluates each field expression once per component.
// Binding: `component` (current component descriptor, v2 wire format).
func extractPerComponent(ctx context.Context, descs []*v2.Descriptor, fields map[string]string) ([]map[string]any, error) {
	baseEnv, err := ocmcel.SharedEnv()
	if err != nil {
		return nil, fmt.Errorf("cel env: %w", err)
	}
	env, err := baseEnv.Extend(cel.Variable("component", cel.DynType))
	if err != nil {
		return nil, fmt.Errorf("extend cel env: %w", err)
	}

	programs, err := compileFields(env, fields, "component")
	if err != nil {
		return nil, err
	}

	out := make([]map[string]any, 0, len(descs))
	for _, desc := range descs {
		compMap, err := toGenericMapViaJSON(desc.Component)
		if err != nil {
			return nil, fmt.Errorf("marshal component %s: %w", desc.Component.Name, err)
		}
		entry, err := evalFields(ctx, programs, map[string]any{"component": compMap})
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

// extractExpression evaluates a single CEL expression with binding `components`
// (list of surviving component descriptors, v2 wire format, in sort order).
func extractExpression(ctx context.Context, descs []*v2.Descriptor, expr string) (any, error) {
	baseEnv, err := ocmcel.SharedEnv()
	if err != nil {
		return nil, fmt.Errorf("cel env: %w", err)
	}
	env, err := baseEnv.Extend(
		cel.Variable("components", cel.ListType(cel.DynType)),
	)
	if err != nil {
		return nil, fmt.Errorf("extend cel env: %w", err)
	}

	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("compile expression (available bindings: components): %w", issues.Err())
	}
	prog, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("build program: %w", err)
	}

	componentMaps := make([]any, 0, len(descs))
	for _, desc := range descs {
		m, err := toGenericMapViaJSON(desc.Component)
		if err != nil {
			return nil, fmt.Errorf("marshal component %s: %w", desc.Component.Name, err)
		}
		componentMaps = append(componentMaps, m)
	}

	val, _, err := prog.ContextEval(ctx, map[string]any{
		"components": componentMaps,
	})
	if err != nil {
		return nil, fmt.Errorf("evaluate expression: %w", err)
	}
	return celconv.GoNativeType(val)
}

// compileFields precompiles all field expressions once; each expression is
// evaluated N times where N is the iteration count. Any compile error is
// returned immediately with the offending field name for status surfacing.
// bindings names the CEL variables in scope for the mode, appended to any
// compile error so a typo tells the user which names are valid.
func compileFields(env *cel.Env, fields map[string]string, bindings string) (map[string]cel.Program, error) {
	programs := make(map[string]cel.Program, len(fields))
	for name, expr := range fields {
		ast, issues := env.Compile(expr)
		if issues != nil && issues.Err() != nil {
			return nil, fmt.Errorf("compile field %q (available bindings: %s): %w", name, bindings, issues.Err())
		}
		prog, err := env.Program(ast)
		if err != nil {
			return nil, fmt.Errorf("build program for field %q: %w", name, err)
		}
		programs[name] = prog
	}
	return programs, nil
}

// evalFields evaluates each precompiled field program with the given bindings.
// Fields that evaluate to null are omitted from the entry so a single map
// can span heterogeneous access types (e.g. some resources carry
// `access.imageReference`, others don't). Missing attribute / field / key
// references are treated the same as null: the field is dropped from the
// entry, mirroring selector tolerance and avoiding the need for CEL `?` on
// every path. To emit a placeholder instead of dropping, callers can wrap
// the access in `has(...) ? ... : <fallback>` in the field expression.
func evalFields(ctx context.Context, programs map[string]cel.Program, activation map[string]any) (map[string]any, error) {
	entry := make(map[string]any, len(programs))
	for name, prog := range programs {
		val, _, err := prog.ContextEval(ctx, activation)
		if err != nil {
			if ocmcel.IsMissingAttributeErr(err) {
				continue
			}
			return nil, fmt.Errorf("evaluate field %q: %w", name, err)
		}
		native, err := celconv.GoNativeType(val)
		if err != nil {
			return nil, fmt.Errorf("convert field %q result: %w", name, err)
		}
		if native == nil {
			continue
		}
		entry[name] = native
	}
	return entry, nil
}

// toGenericMapViaJSON marshals v to JSON and back into a map[string]any,
// yielding the same shape CEL sees when a resource / component is passed in.
func toGenericMapViaJSON(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return m, nil
}
