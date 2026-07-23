package functions

import (
	"github.com/Masterminds/semver/v3"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

const SemverCheckFunctionName = "semverCheck"

// SemverCheck registers the `semverCheck(version, constraint)` CEL function.
// It returns true when `version` satisfies the Masterminds/semver constraint
// syntax (e.g. ">=2.7.0, <2.10.0"). An unparseable version yields false so
// non-semver values just don't match, mirroring how the old SemverRange
// operator behaved.
func SemverCheck() cel.EnvOption {
	return cel.Function(
		SemverCheckFunctionName,
		cel.Overload(
			"semverCheck_string_string",
			[]*cel.Type{cel.StringType, cel.StringType},
			cel.BoolType,
			cel.BinaryBinding(bindingSemverCheck),
		),
	)
}

func bindingSemverCheck(v, c ref.Val) ref.Val {
	version, ok := v.Value().(string)
	if !ok {
		return types.NewErr("semverCheck: version must be string, got %T", v.Value())
	}
	constraint, ok := c.Value().(string)
	if !ok {
		return types.NewErr("semverCheck: constraint must be string, got %T", c.Value())
	}
	con, err := semver.NewConstraint(constraint)
	if err != nil {
		// Invalid constraint is a config bug; surface it.
		return types.NewErr("semverCheck: invalid constraint %q: %v", constraint, err)
	}
	ver, err := semver.NewVersion(version)
	if err != nil {
		// Non-semver version is a data condition, not a config bug.
		return types.Bool(false)
	}
	return types.Bool(con.Check(ver))
}
