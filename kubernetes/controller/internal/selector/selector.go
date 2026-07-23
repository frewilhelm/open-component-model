package selector

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/cel-go/cel"

	"ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
	ocmcel "ocm.software/open-component-model/kubernetes/controller/internal/cel"
)

// Matchable represents an element that can be matched against a Selector.
type Matchable struct {
	// Identity contains the element's identity attributes (name, version, extra identity).
	Identity map[string]string
	// Labels holds the element's labels keyed by name. Values are the label's
	// full JSON value (string, number, bool, object, array). MatchLabels only
	// looks at entries whose value is a string; the Expression path sees all.
	Labels map[string]any
}

// IsEmpty reports whether s has no predicates set (MatchIdentity, MatchLabels,
// and Expression are all zero). A nil selector is empty. Discovery treats an
// empty selector the same as an omitted field: the containing filter stage is
// skipped entirely, so an empty referenceSelector does not drop the root.
func IsEmpty(s *v1alpha1.Selector) bool {
	return s == nil || (len(s.MatchIdentity) == 0 && len(s.MatchLabels) == 0 && s.Expression == "")
}

// Matches evaluates whether the given element matches all requirements in the
// selector. MatchIdentity, MatchLabels, and Expression are ANDed. A nil
// selector matches everything.
func Matches(ctx context.Context, s *v1alpha1.Selector, element Matchable) (bool, error) {
	if s == nil {
		return true, nil
	}

	for key, want := range s.MatchIdentity {
		if got, exists := element.Identity[key]; !exists || got != want {
			return false, nil
		}
	}

	for name, want := range s.MatchLabels {
		got, ok := element.Labels[name].(string)
		if !ok || got != want {
			return false, nil
		}
	}

	if s.Expression != "" {
		return evalExpression(ctx, s.Expression, element)
	}

	return true, nil
}

// selectorEnv is the CEL environment used to compile Selector.Expression
// programs. It extends the shared env with the two variables Matchable exposes.
var (
	selectorEnvOnce sync.Once
	selectorEnv     *cel.Env
	selectorEnvErr  error
)

func env() (*cel.Env, error) {
	selectorEnvOnce.Do(func() {
		base, err := ocmcel.SharedEnv()
		if err != nil {
			selectorEnvErr = err
			return
		}
		selectorEnv, selectorEnvErr = base.Extend(
			cel.Variable("identity", cel.MapType(cel.StringType, cel.StringType)),
			cel.Variable("labels", cel.MapType(cel.StringType, cel.DynType)),
		)
	})
	return selectorEnv, selectorEnvErr
}

// programCache memoizes compiled CEL programs keyed by expression text. It is
// process-wide because Selector.Expression is typically stable across
// reconciles for a given Selector and reconciles a lot of elements.
var programCache sync.Map // map[string]cel.Program

func getProgram(expr string) (cel.Program, error) {
	if p, ok := programCache.Load(expr); ok {
		return p.(cel.Program), nil
	}
	e, err := env()
	if err != nil {
		return nil, fmt.Errorf("cel env: %w", err)
	}
	ast, issues := e.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("compile expression: %w", issues.Err())
	}
	prog, err := e.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("build program: %w", err)
	}
	// Store: last writer wins; identical expressions produce equivalent programs.
	actual, _ := programCache.LoadOrStore(expr, prog)
	return actual.(cel.Program), nil
}

func evalExpression(ctx context.Context, expr string, e Matchable) (bool, error) {
	prog, err := getProgram(expr)
	if err != nil {
		return false, err
	}
	val, _, err := prog.ContextEval(ctx, map[string]any{
		"identity": e.Identity,
		"labels":   e.Labels,
	})
	if err != nil {
		if ocmcel.IsMissingAttributeErr(err) {
			// An expression that references an attribute the element doesn't
			// carry evaluates to "no match" rather than surfacing an error.
			// Users who want strictness can use has(identity.foo) explicitly.
			return false, nil
		}
		return false, fmt.Errorf("evaluate expression: %w", err)
	}
	b, ok := val.Value().(bool)
	if !ok {
		return false, fmt.Errorf("expression must return bool, got %T", val.Value())
	}
	return b, nil
}
