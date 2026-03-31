package controller

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/ext"
	k8stypes "k8s.io/apimachinery/pkg/types"

	deliveryv1alpha1 "ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
)

// newDeploymentCELEnv creates a CEL environment with OCMDeployment-specific
// pipeline functions (repoByRef, componentByReference, resourceByIdentity, toOCI)
// and registers resource states as variables so cross-resource references like
// ${ocirepository.metadata.name} are evaluated as native CEL expressions.
//
// When a non-nil resourceIndex is provided (from the Phase 0 pre-scan), the
// shorthand resource() and component() functions are also registered.
func newDeploymentCELEnv(
	ctx context.Context,
	r *DeploymentReconciler,
	deployment *deliveryv1alpha1.OCMDeployment,
	resourceStates map[string]map[string]any,
	resourceIndex *ResourceIndex,
) (*cel.Env, error) {
	opts := []cel.EnvOption{
		ext.Strings(),
		ext.Math(),
		ext.Encoders(),

		// repoByRef(url) -> map   OR   repoByRef(url, options) -> map
		cel.Function("repoByRef",
			cel.Overload("repoByRef_string",
				[]*cel.Type{cel.StringType},
				cel.DynType,
				cel.UnaryBinding(bindRepoByRefUnary(ctx, r, deployment)),
			),
			cel.Overload("repoByRef_string_map",
				[]*cel.Type{cel.StringType, cel.MapType(cel.StringType, cel.StringType)},
				cel.DynType,
				cel.BinaryBinding(bindRepoByRefBinary(ctx, r, deployment)),
			),
		),

		// pipeline.componentByReference(name) -> map
		// pipeline.componentByReference(name, options) -> map
		cel.Function("componentByReference",
			cel.MemberOverload("componentByReference_map_string",
				[]*cel.Type{cel.DynType, cel.StringType},
				cel.DynType,
				cel.BinaryBinding(bindComponentByRefBinary(ctx, r, deployment)),
			),
			cel.MemberOverload("componentByReference_map_string_map",
				[]*cel.Type{cel.DynType, cel.StringType, cel.MapType(cel.StringType, cel.StringType)},
				cel.DynType,
				cel.FunctionBinding(bindComponentByRefTernary(ctx, r, deployment)),
			),
		),

		// pipeline.resourceByIdentity(identity) -> map
		cel.Function("resourceByIdentity",
			cel.MemberOverload("resourceByIdentity_map_map",
				[]*cel.Type{cel.DynType, cel.MapType(cel.StringType, cel.StringType)},
				cel.DynType,
				cel.BinaryBinding(bindResourceByIdentity(ctx, r, deployment)),
			),
		),

		// pipeline.toOCI() -> map<string, string>
		cel.Function("toOCI",
			cel.MemberOverload("toOCI_pipeline",
				[]*cel.Type{cel.DynType},
				cel.DynType,
			),
			cel.Overload("toOCI_pipeline_standalone",
				[]*cel.Type{cel.DynType},
				cel.DynType,
			),
			cel.SingletonUnaryBinding(bindToOCIPipeline(ctx, r, deployment)),
		),

		// pipeline.withVerificationFromSecret(signatureName, secretName) -> map
		cel.Function("withVerificationFromSecret",
			cel.MemberOverload("withVerificationFromSecret_map_string_string",
				[]*cel.Type{cel.DynType, cel.StringType, cel.StringType},
				cel.DynType,
				cel.FunctionBinding(bindWithVerificationFromSecret(ctx, r, deployment)),
			),
		),

		// pipeline.withVerification(signatureName, publicKeyPEM) -> map
		cel.Function("withVerification",
			cel.MemberOverload("withVerification_map_string_string",
				[]*cel.Type{cel.DynType, cel.StringType, cel.StringType},
				cel.DynType,
				cel.FunctionBinding(bindWithVerification(ctx, r, deployment)),
			),
		),

		// pipeline.componentReference("ref-name") -> map
		// pipeline.componentReference("ref-a", "ref-b") -> map
		cel.Function("componentReference",
			cel.MemberOverload("componentReference_map_string",
				[]*cel.Type{cel.DynType, cel.StringType},
				cel.DynType,
				cel.BinaryBinding(bindComponentReference(ctx, r, deployment)),
			),
			cel.MemberOverload("componentReference_map_string_string",
				[]*cel.Type{cel.DynType, cel.StringType, cel.StringType},
				cel.DynType,
				cel.FunctionBinding(bindComponentReferenceMulti(ctx, r, deployment)),
			),
		),
	}

	// Register resource() and component() shorthands when a resource index
	// is available (i.e., when Phase 0 found pipeline chains).
	if resourceIndex != nil {
		opts = append(opts,
			// resource("name") -> pipeline state  (auto-discovers component)
			// resource({"name": "x", ...}) -> pipeline state  (full identity)
			cel.Function("resource",
				cel.Overload("resource_string",
					[]*cel.Type{cel.StringType},
					cel.DynType,
					cel.UnaryBinding(bindResourceShorthand(ctx, r, deployment, resourceIndex)),
				),
				cel.Overload("resource_map",
					[]*cel.Type{cel.MapType(cel.StringType, cel.StringType)},
					cel.DynType,
					cel.UnaryBinding(bindResourceByIdentityShorthand(ctx, r, deployment, resourceIndex)),
				),
			),

			// component("name") -> component handle
			// component("name").resource("name") -> pipeline state
			cel.Function("component",
				cel.Overload("component_string",
					[]*cel.Type{cel.StringType},
					cel.DynType,
					cel.UnaryBinding(bindComponentHandle(resourceIndex)),
				),
			),

			// handle.resource("name") -> pipeline state (member on component handle)
			cel.Function("resource",
				cel.MemberOverload("component_resource_string",
					[]*cel.Type{cel.DynType, cel.StringType},
					cel.DynType,
					cel.BinaryBinding(bindComponentResourceMember(ctx, r, deployment, resourceIndex)),
				),
			),
		)
	}

	// Register each resolved resource state as a CEL variable.
	// This allows cross-resource references like ${ocirepository.metadata.name}
	// to be evaluated as native CEL field access on a map variable.
	for id := range resourceStates {
		opts = append(opts, cel.Variable(id, cel.DynType))
	}

	return cel.NewEnv(opts...)
}

// evaluateCELExpression compiles and evaluates a CEL expression string
// with the given variable bindings.
func evaluateCELExpression(env *cel.Env, expr string, vars map[string]any) (any, error) {
	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("CEL compile error: %w", issues.Err())
	}

	prog, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("CEL program error: %w", err)
	}

	val, _, err := prog.Eval(vars)
	if err != nil {
		return nil, fmt.Errorf("CEL eval error: %w", err)
	}

	return celToGo(val)
}

// --- CEL Function Bindings ---

// bindRepoByRefUnary handles repoByRef(url)
func bindRepoByRefUnary(
	ctx context.Context,
	r *DeploymentReconciler,
	deployment *deliveryv1alpha1.OCMDeployment,
) func(ref.Val) ref.Val {
	return func(urlVal ref.Val) ref.Val {
		url, ok := urlVal.Value().(string)
		if !ok {
			return types.NewErr("repoByRef: url must be a string, got %T", urlVal.Value())
		}
		return doRepoByRef(ctx, r, deployment, url, nil)
	}
}

// bindRepoByRefBinary handles repoByRef(url, options)
func bindRepoByRefBinary(
	ctx context.Context,
	r *DeploymentReconciler,
	deployment *deliveryv1alpha1.OCMDeployment,
) func(ref.Val, ref.Val) ref.Val {
	return func(urlVal, optsVal ref.Val) ref.Val {
		url, ok := urlVal.Value().(string)
		if !ok {
			return types.NewErr("repoByRef: url must be a string, got %T", urlVal.Value())
		}
		opts, err := celMapToStringMap(optsVal)
		if err != nil {
			return types.NewErr("repoByRef: options: %v", err)
		}
		return doRepoByRef(ctx, r, deployment, url, opts)
	}
}

// doRepoByRef creates/ensures a Repository CR and returns the pipeline state.
func doRepoByRef(
	ctx context.Context,
	r *DeploymentReconciler,
	deployment *deliveryv1alpha1.OCMDeployment,
	url string,
	opts map[string]string,
) ref.Val {
	pipeline := &OCMPipeline{
		RepoURL:      url,
		RepoType:     "OCIRegistry", // default
		RepoInterval: "10m",         // default
	}
	if opts != nil {
		if v, ok := opts["type"]; ok {
			pipeline.RepoType = v
		}
		if v, ok := opts["interval"]; ok {
			pipeline.RepoInterval = v
		}
	}

	if err := r.ensureRepository(ctx, deployment, pipeline); err != nil {
		return types.NewErr("repoByRef: failed to create repository: %v", err)
	}

	state := map[string]any{
		pipelineStateKey: true,
		"_repoURL":       pipeline.RepoURL,
		"_repoType":      pipeline.RepoType,
		"_repoInterval":  pipeline.RepoInterval,
		"_repoName":      pipeline.RepoName(deployment.Name),
		"_namespace":     deployment.Namespace,
		"_deployName":    deployment.Name,
	}

	return types.DefaultTypeAdapter.NativeToValue(state)
}

// bindComponentByRefBinary handles pipeline.componentByReference(name)
func bindComponentByRefBinary(
	ctx context.Context,
	r *DeploymentReconciler,
	deployment *deliveryv1alpha1.OCMDeployment,
) func(ref.Val, ref.Val) ref.Val {
	return func(selfVal, nameVal ref.Val) ref.Val {
		state, err := celValToGoMap(selfVal)
		if err != nil {
			return types.NewErr("componentByReference: invalid pipeline state: %v", err)
		}
		name, ok := nameVal.Value().(string)
		if !ok {
			return types.NewErr("componentByReference: name must be a string")
		}
		return doComponentByRef(ctx, r, deployment, state, name, nil)
	}
}

// bindComponentByRefTernary handles pipeline.componentByReference(name, options)
func bindComponentByRefTernary(
	ctx context.Context,
	r *DeploymentReconciler,
	deployment *deliveryv1alpha1.OCMDeployment,
) func(...ref.Val) ref.Val {
	return func(args ...ref.Val) ref.Val {
		if len(args) != 3 {
			return types.NewErr("componentByReference: expected 3 args (self, name, options), got %d", len(args))
		}
		state, err := celValToGoMap(args[0])
		if err != nil {
			return types.NewErr("componentByReference: invalid pipeline state: %v", err)
		}
		name, ok := args[1].Value().(string)
		if !ok {
			return types.NewErr("componentByReference: name must be a string")
		}
		opts, err := celMapToStringMap(args[2])
		if err != nil {
			return types.NewErr("componentByReference: options: %v", err)
		}
		return doComponentByRef(ctx, r, deployment, state, name, opts)
	}
}

// doComponentByRef creates/ensures a Component CR and returns the enriched pipeline state.
func doComponentByRef(
	ctx context.Context,
	r *DeploymentReconciler,
	deployment *deliveryv1alpha1.OCMDeployment,
	state map[string]any,
	name string,
	opts map[string]string,
) ref.Val {
	pipeline := pipelineFromState(state)
	pipeline.Component = name
	pipeline.Semver = ">=0.0.0" // default: latest
	pipeline.CompInterval = "10m"

	if opts != nil {
		if v, ok := opts["semver"]; ok {
			pipeline.Semver = v
		}
		if v, ok := opts["interval"]; ok {
			pipeline.CompInterval = v
		}
	}

	if err := r.ensureComponent(ctx, deployment, pipeline); err != nil {
		return types.NewErr("componentByReference: failed to create component: %v", err)
	}

	state["_component"] = pipeline.Component
	state["_semver"] = pipeline.Semver
	state["_compInterval"] = pipeline.CompInterval
	state["_compName"] = pipeline.CompName(deployment.Name)

	return types.DefaultTypeAdapter.NativeToValue(state)
}

// bindResourceByIdentity handles pipeline.resourceByIdentity(identity)
func bindResourceByIdentity(
	ctx context.Context,
	r *DeploymentReconciler,
	deployment *deliveryv1alpha1.OCMDeployment,
) func(ref.Val, ref.Val) ref.Val {
	return func(selfVal, identityVal ref.Val) ref.Val {
		state, err := celValToGoMap(selfVal)
		if err != nil {
			return types.NewErr("resourceByIdentity: invalid pipeline state: %v", err)
		}
		identity, err := celMapToStringMap(identityVal)
		if err != nil {
			return types.NewErr("resourceByIdentity: identity must be map<string,string>: %v", err)
		}

		pipeline := pipelineFromState(state)
		pipeline.ResourceID = identity

		if err := r.ensureResource(ctx, deployment, pipeline, allTOCIFields); err != nil {
			return types.NewErr("resourceByIdentity: failed to create resource: %v", err)
		}

		state["_resourceID"] = identity
		state["_resName"] = pipeline.ResName(deployment.Name)

		return types.DefaultTypeAdapter.NativeToValue(state)
	}
}

// bindToOCIPipeline handles pipeline.toOCI() for pipeline state maps.
// It reads the auto-created Resource's status.additional and returns it.
func bindToOCIPipeline(
	ctx context.Context,
	r *DeploymentReconciler,
	deployment *deliveryv1alpha1.OCMDeployment,
) func(ref.Val) ref.Val {
	return func(selfVal ref.Val) ref.Val {
		state, err := celValToGoMap(selfVal)
		if err != nil {
			return types.NewErr("toOCI: invalid input: %v", err)
		}

		if _, ok := state[pipelineStateKey]; !ok {
			return types.NewErr("toOCI: input is not a pipeline state (missing %s key)", pipelineStateKey)
		}

		resName, _ := state["_resName"].(string)
		if resName == "" {
			return types.NewErr("toOCI: pipeline has no resource — call resourceByIdentity() before toOCI()")
		}
		namespace, _ := state["_namespace"].(string)

		res := &deliveryv1alpha1.Resource{}
		if err := r.Get(ctx, k8stypes.NamespacedName{Namespace: namespace, Name: resName}, res); err != nil {
			return types.NewErr("toOCI: failed to read resource %q: %v", resName, err)
		}

		if res.Status.Additional == nil {
			return types.NewErr("toOCI: resource %q has no additional status yet (not ready)", resName)
		}

		var additional map[string]any
		if err := json.Unmarshal(res.Status.Additional.Raw, &additional); err != nil {
			return types.NewErr("toOCI: failed to parse resource %q additional status: %v", resName, err)
		}

		// Convert all values to strings for consistent map type.
		result := make(map[string]any, len(additional))
		for k, v := range additional {
			result[k] = fmt.Sprintf("%v", v)
		}

		return types.DefaultTypeAdapter.NativeToValue(result)
	}
}

// --- Helpers ---

// pipelineFromState reconstructs an OCMPipeline from a pipeline state map.
func pipelineFromState(state map[string]any) *OCMPipeline {
	p := &OCMPipeline{
		RepoInterval: "10m",
		CompInterval: "10m",
		Semver:       ">=0.0.0",
	}
	if v, ok := state["_repoURL"].(string); ok {
		p.RepoURL = v
	}
	if v, ok := state["_repoType"].(string); ok {
		p.RepoType = v
	}
	if v, ok := state["_repoInterval"].(string); ok {
		p.RepoInterval = v
	}
	if v, ok := state["_component"].(string); ok {
		p.Component = v
	}
	if v, ok := state["_semver"].(string); ok {
		p.Semver = v
	}
	if v, ok := state["_compInterval"].(string); ok {
		p.CompInterval = v
	}
	if v, ok := state["_resourceID"].(map[string]string); ok {
		p.ResourceID = v
	}
	if v, ok := state["_verify"].([]VerificationEntry); ok {
		p.Verify = v
	}
	if v, ok := state["_referencePath"].([]string); ok {
		p.ReferencePath = v
	}
	return p
}

// --- Verification Bindings ---

// bindWithVerificationFromSecret handles pipeline.withVerificationFromSecret(signatureName, secretName).
// It stores the verification config in the pipeline state and re-applies the Component CR
// with the verification entry.
func bindWithVerificationFromSecret(
	ctx context.Context,
	r *DeploymentReconciler,
	deployment *deliveryv1alpha1.OCMDeployment,
) func(...ref.Val) ref.Val {
	return func(args ...ref.Val) ref.Val {
		if len(args) != 3 {
			return types.NewErr("withVerificationFromSecret: expected 3 args (self, signatureName, secretName), got %d", len(args))
		}
		state, err := celValToGoMap(args[0])
		if err != nil {
			return types.NewErr("withVerificationFromSecret: invalid pipeline state: %v", err)
		}
		sigName, ok := args[1].Value().(string)
		if !ok {
			return types.NewErr("withVerificationFromSecret: signatureName must be a string")
		}
		secretName, ok := args[2].Value().(string)
		if !ok {
			return types.NewErr("withVerificationFromSecret: secretName must be a string")
		}

		entry := VerificationEntry{Signature: sigName, SecretRef: secretName}
		verify := appendVerify(state, entry)
		state["_verify"] = verify

		// Re-apply the Component CR with verification.
		pipeline := pipelineFromState(state)
		if err := r.ensureComponent(ctx, deployment, pipeline); err != nil {
			return types.NewErr("withVerificationFromSecret: failed to update component: %v", err)
		}

		return types.DefaultTypeAdapter.NativeToValue(state)
	}
}

// bindWithVerification handles pipeline.withVerification(signatureName, publicKeyPEM).
func bindWithVerification(
	ctx context.Context,
	r *DeploymentReconciler,
	deployment *deliveryv1alpha1.OCMDeployment,
) func(...ref.Val) ref.Val {
	return func(args ...ref.Val) ref.Val {
		if len(args) != 3 {
			return types.NewErr("withVerification: expected 3 args (self, signatureName, publicKey), got %d", len(args))
		}
		state, err := celValToGoMap(args[0])
		if err != nil {
			return types.NewErr("withVerification: invalid pipeline state: %v", err)
		}
		sigName, ok := args[1].Value().(string)
		if !ok {
			return types.NewErr("withVerification: signatureName must be a string")
		}
		publicKey, ok := args[2].Value().(string)
		if !ok {
			return types.NewErr("withVerification: publicKey must be a string")
		}

		entry := VerificationEntry{Signature: sigName, Value: publicKey}
		verify := appendVerify(state, entry)
		state["_verify"] = verify

		// Re-apply the Component CR with verification.
		pipeline := pipelineFromState(state)
		if err := r.ensureComponent(ctx, deployment, pipeline); err != nil {
			return types.NewErr("withVerification: failed to update component: %v", err)
		}

		return types.DefaultTypeAdapter.NativeToValue(state)
	}
}

// appendVerify extracts existing verify entries from state and appends a new one.
func appendVerify(state map[string]any, entry VerificationEntry) []VerificationEntry {
	existing, _ := state["_verify"].([]VerificationEntry)
	return append(existing, entry)
}

// --- Component Reference Bindings ---

// bindComponentReference handles pipeline.componentReference("ref-name") — single-level nesting.
func bindComponentReference(
	ctx context.Context,
	r *DeploymentReconciler,
	deployment *deliveryv1alpha1.OCMDeployment,
) func(ref.Val, ref.Val) ref.Val {
	return func(selfVal, refNameVal ref.Val) ref.Val {
		state, err := celValToGoMap(selfVal)
		if err != nil {
			return types.NewErr("componentReference: invalid pipeline state: %v", err)
		}
		refName, ok := refNameVal.Value().(string)
		if !ok {
			return types.NewErr("componentReference: reference name must be a string")
		}
		return doComponentReference(ctx, r, deployment, state, []string{refName})
	}
}

// bindComponentReferenceMulti handles pipeline.componentReference("ref-a", "ref-b") — multi-level.
func bindComponentReferenceMulti(
	ctx context.Context,
	r *DeploymentReconciler,
	deployment *deliveryv1alpha1.OCMDeployment,
) func(...ref.Val) ref.Val {
	return func(args ...ref.Val) ref.Val {
		if len(args) < 3 {
			return types.NewErr("componentReference: expected at least 3 args (self, ref1, ref2), got %d", len(args))
		}
		state, err := celValToGoMap(args[0])
		if err != nil {
			return types.NewErr("componentReference: invalid pipeline state: %v", err)
		}
		refs := make([]string, 0, len(args)-1)
		for _, a := range args[1:] {
			name, ok := a.Value().(string)
			if !ok {
				return types.NewErr("componentReference: all reference names must be strings")
			}
			refs = append(refs, name)
		}
		return doComponentReference(ctx, r, deployment, state, refs)
	}
}

// doComponentReference stores the reference path in the pipeline state.
// The reference path will be used by ensureResource to set the ReferencePath
// on the Resource CR, telling the Resource controller to resolve through
// the nested component references.
func doComponentReference(
	ctx context.Context,
	r *DeploymentReconciler,
	deployment *deliveryv1alpha1.OCMDeployment,
	state map[string]any,
	refs []string,
) ref.Val {
	// Append to any existing reference path.
	existing, _ := state["_referencePath"].([]string)
	state["_referencePath"] = append(existing, refs...)

	return types.DefaultTypeAdapter.NativeToValue(state)
}

// celValToGoMap converts a CEL ref.Val to a Go map[string]any.
func celValToGoMap(v ref.Val) (map[string]any, error) {
	native := v.Value()
	switch m := native.(type) {
	case map[string]any:
		return m, nil
	case map[ref.Val]ref.Val:
		result := make(map[string]any, len(m))
		for k, val := range m {
			ks, ok := k.Value().(string)
			if !ok {
				return nil, fmt.Errorf("non-string key: %v", k)
			}
			result[ks] = val.Value()
		}
		return result, nil
	default:
		return nil, fmt.Errorf("expected map, got %T", native)
	}
}

// celMapToStringMap converts a CEL map value to map[string]string.
func celMapToStringMap(v ref.Val) (map[string]string, error) {
	goMap, err := celValToGoMap(v)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(goMap))
	for k, val := range goMap {
		result[k] = fmt.Sprintf("%v", val)
	}
	return result, nil
}

// celToGo converts a CEL ref.Val to a native Go value.
func celToGo(v ref.Val) (any, error) {
	switch v.Type() {
	case types.StringType:
		return v.Value().(string), nil
	case types.IntType:
		return v.Value().(int64), nil
	case types.BoolType:
		return v.Value().(bool), nil
	case types.DoubleType:
		return v.Value().(float64), nil
	case types.MapType:
		return celValToGoMap(v)
	default:
		return v.Value(), nil
	}
}

// --- Resource Shorthand Bindings ---

// componentHandleKey is the marker key identifying a component handle map
// (produced by component("name") for use with .resource()).
const componentHandleKey = "_componentHandle"

// bindResourceShorthand handles resource("name") — looks up the resource
// by name in the pre-built index, ensures the Resource CR, and returns a
// pipeline state that supports .toOCI().
func bindResourceShorthand(
	ctx context.Context,
	r *DeploymentReconciler,
	deployment *deliveryv1alpha1.OCMDeployment,
	index *ResourceIndex,
) func(ref.Val) ref.Val {
	return func(nameVal ref.Val) ref.Val {
		name, ok := nameVal.Value().(string)
		if !ok {
			return types.NewErr("resource: name must be a string, got %T", nameVal.Value())
		}

		entry, err := index.Lookup(name)
		if err != nil {
			return types.NewErr("resource: %v", err)
		}

		return ensureAndReturnPipelineState(ctx, r, deployment, entry)
	}
}

// bindResourceByIdentityShorthand handles resource({"name": "x", ...}) —
// looks up by full identity map.
func bindResourceByIdentityShorthand(
	ctx context.Context,
	r *DeploymentReconciler,
	deployment *deliveryv1alpha1.OCMDeployment,
	index *ResourceIndex,
) func(ref.Val) ref.Val {
	return func(identityVal ref.Val) ref.Val {
		identity, err := celMapToStringMap(identityVal)
		if err != nil {
			return types.NewErr("resource: identity must be map<string,string>: %v", err)
		}
		name, ok := identity["name"]
		if !ok {
			return types.NewErr("resource: identity map must contain a 'name' key")
		}

		entry, err := index.Lookup(name)
		if err != nil {
			return types.NewErr("resource: %v", err)
		}

		return ensureAndReturnPipelineState(ctx, r, deployment, entry)
	}
}

// bindComponentHandle handles component("name") — returns a component
// handle map that the member .resource() function can consume.
func bindComponentHandle(index *ResourceIndex) func(ref.Val) ref.Val {
	return func(nameVal ref.Val) ref.Val {
		name, ok := nameVal.Value().(string)
		if !ok {
			return types.NewErr("component: name must be a string, got %T", nameVal.Value())
		}

		// Validate that this component exists in the index.
		found := false
		for _, entries := range index.byName {
			for _, e := range entries {
				if e.Component == name {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			return types.NewErr("component: %q not found in deployment", name)
		}

		handle := map[string]any{
			componentHandleKey: true,
			"_componentName":   name,
		}
		return types.DefaultTypeAdapter.NativeToValue(handle)
	}
}

// bindComponentResourceMember handles handle.resource("name") where handle
// is the map returned by component("compName").
func bindComponentResourceMember(
	ctx context.Context,
	r *DeploymentReconciler,
	deployment *deliveryv1alpha1.OCMDeployment,
	index *ResourceIndex,
) func(ref.Val, ref.Val) ref.Val {
	return func(selfVal, nameVal ref.Val) ref.Val {
		handle, err := celValToGoMap(selfVal)
		if err != nil {
			return types.NewErr("resource: invalid component handle: %v", err)
		}
		if _, ok := handle[componentHandleKey]; !ok {
			return types.NewErr("resource: receiver is not a component handle")
		}
		componentName, _ := handle["_componentName"].(string)

		name, ok := nameVal.Value().(string)
		if !ok {
			return types.NewErr("resource: name must be a string, got %T", nameVal.Value())
		}

		entry, err := index.LookupForComponent(componentName, name)
		if err != nil {
			return types.NewErr("resource: %v", err)
		}

		return ensureAndReturnPipelineState(ctx, r, deployment, entry)
	}
}

// ensureAndReturnPipelineState creates the Resource CR for the given index
// entry and returns a pipeline state map that supports .toOCI().
func ensureAndReturnPipelineState(
	ctx context.Context,
	r *DeploymentReconciler,
	deployment *deliveryv1alpha1.OCMDeployment,
	entry *ResolvedResourceEntry,
) ref.Val {
	pipeline := &OCMPipeline{
		RepoURL:       entry.Pipeline.RepoURL,
		RepoType:      entry.Pipeline.RepoType,
		RepoInterval:  entry.Pipeline.RepoInterval,
		Component:     entry.Pipeline.Component,
		Semver:        entry.Pipeline.Semver,
		CompInterval:  entry.Pipeline.CompInterval,
		ResourceID:    entry.ResourceID,
		Verify:        entry.Pipeline.Verify,
		ReferencePath: entry.Pipeline.ReferencePath,
	}

	if err := r.ensureResource(ctx, deployment, pipeline, allTOCIFields); err != nil {
		return types.NewErr("resource: failed to ensure resource CR: %v", err)
	}

	state := map[string]any{
		pipelineStateKey: true,
		"_repoURL":       pipeline.RepoURL,
		"_repoType":      pipeline.RepoType,
		"_repoInterval":  pipeline.RepoInterval,
		"_repoName":      pipeline.RepoName(deployment.Name),
		"_namespace":     deployment.Namespace,
		"_deployName":    deployment.Name,
		"_component":     pipeline.Component,
		"_semver":        pipeline.Semver,
		"_compInterval":  pipeline.CompInterval,
		"_compName":      pipeline.CompName(deployment.Name),
		"_resourceID":    pipeline.ResourceID,
		"_resName":       pipeline.ResName(deployment.Name),
	}

	return types.DefaultTypeAdapter.NativeToValue(state)
}
