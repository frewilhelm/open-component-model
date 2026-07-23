package cel

import (
	"fmt"
	"strings"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"

	"ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
	ocmfunctions "ocm.software/open-component-model/kubernetes/controller/internal/cel/functions"
)

var sharedEnv = sync.OnceValues[*cel.Env, error](func() (*cel.Env, error) {
	return cel.NewEnv(
		ext.Lists(),
		ext.Sets(),
		ext.Strings(),
		ext.Math(),
		ext.Encoders(),
		ext.Bindings(),
		cel.OptionalTypes(),
		ocmfunctions.SemverCheck(),
	)
})

// SharedEnv returns the process-wide base CEL environment used by controllers
// that do not need any extension bound to a particular ComponentInfo. Call
// Extend on the result to declare variables specific to the caller.
func SharedEnv() (*cel.Env, error) {
	return sharedEnv()
}

// ComponentInfoEnv constructs a CEL environment with a v1alpha1.ComponentInfo as a dependency.
// Extensions like `toOCI` need v1alpha1.ComponentInfo to properly provide an ImageReference from a localBlob.
func ComponentInfoEnv(component *v1alpha1.ComponentInfo) (*cel.Env, error) {
	if component == nil {
		return nil, fmt.Errorf("component info is nil but required to create the CEL environment")
	}

	env, err := sharedEnv()
	if err != nil {
		return nil, fmt.Errorf("failed to load shared cel environment: %w", err)
	}

	ociEnv, err := env.Extend(ocmfunctions.ToOCI(component))
	if err != nil {
		return nil, fmt.Errorf("failed to extend shared cel environment: %w", err)
	}

	return ociEnv, nil
}

// IsMissingAttributeErr recognises CEL runtime errors caused by referencing a
// map key / attribute / field that the activation doesn't carry. cel-go
// surfaces these as unwrapped *types.Err values distinguishable only by their
// message. Callers that want "missing evaluates to no-value rather than
// raising" semantics use this to convert the error into a null / non-match.
func IsMissingAttributeErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no such key") ||
		strings.Contains(msg, "no such attribute") ||
		strings.Contains(msg, "no such field")
}
