package selector

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
)

// helper to build a Matchable with string-valued labels; keeps tests concise.
func matchable(id map[string]string, labels map[string]any) Matchable {
	if id == nil {
		id = map[string]string{}
	}
	if labels == nil {
		labels = map[string]any{}
	}
	return Matchable{Identity: id, Labels: labels}
}

func TestSelector_Nil_MatchesEverything(t *testing.T) {
	r := require.New(t)
	ok, err := Matches(context.Background(), nil, matchable(nil, nil))
	r.NoError(err)
	r.True(ok)
}

func TestIsEmpty(t *testing.T) {
	r := require.New(t)
	r.True(IsEmpty(nil), "nil selector is empty")
	r.True(IsEmpty(&v1alpha1.Selector{}), "zero-value selector is empty")
	r.False(IsEmpty(&v1alpha1.Selector{MatchIdentity: map[string]string{"name": "x"}}))
	r.False(IsEmpty(&v1alpha1.Selector{MatchLabels: map[string]string{"env": "prod"}}))
	r.False(IsEmpty(&v1alpha1.Selector{Expression: "true"}))
}

func TestSelector_MatchIdentity(t *testing.T) {
	cases := []struct {
		name    string
		sel     *v1alpha1.Selector
		element Matchable
		want    bool
	}{
		{
			name: "single key equality matches",
			sel: &v1alpha1.Selector{
				MatchIdentity: map[string]string{"name": "foo"},
			},
			element: matchable(map[string]string{"name": "foo", "version": "1.0"}, nil),
			want:    true,
		},
		{
			name: "single key mismatch",
			sel: &v1alpha1.Selector{
				MatchIdentity: map[string]string{"name": "foo"},
			},
			element: matchable(map[string]string{"name": "bar"}, nil),
			want:    false,
		},
		{
			name: "multiple keys ANDed",
			sel: &v1alpha1.Selector{
				MatchIdentity: map[string]string{"name": "foo", "version": "1.0"},
			},
			element: matchable(map[string]string{"name": "foo", "version": "1.0"}, nil),
			want:    true,
		},
		{
			name: "multiple keys, one mismatch",
			sel: &v1alpha1.Selector{
				MatchIdentity: map[string]string{"name": "foo", "version": "1.0"},
			},
			element: matchable(map[string]string{"name": "foo", "version": "2.0"}, nil),
			want:    false,
		},
		{
			name: "missing identity attribute",
			sel: &v1alpha1.Selector{
				MatchIdentity: map[string]string{"missing": "x"},
			},
			element: matchable(map[string]string{"name": "foo"}, nil),
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			got, err := Matches(context.Background(), tc.sel, tc.element)
			r.NoError(err)
			r.Equal(tc.want, got)
		})
	}
}

func TestSelector_MatchLabels(t *testing.T) {
	cases := []struct {
		name    string
		sel     *v1alpha1.Selector
		element Matchable
		want    bool
	}{
		{
			name: "string label equality matches",
			sel: &v1alpha1.Selector{
				MatchLabels: map[string]string{"env": "prod"},
			},
			element: matchable(nil, map[string]any{"env": "prod"}),
			want:    true,
		},
		{
			name: "string label mismatch",
			sel: &v1alpha1.Selector{
				MatchLabels: map[string]string{"env": "prod"},
			},
			element: matchable(nil, map[string]any{"env": "dev"}),
			want:    false,
		},
		{
			name: "non-string label treated as non-match",
			sel: &v1alpha1.Selector{
				MatchLabels: map[string]string{"retries": "3"},
			},
			// stored as JSON number, not string
			element: matchable(nil, map[string]any{"retries": float64(3)}),
			want:    false,
		},
		{
			name: "missing label",
			sel: &v1alpha1.Selector{
				MatchLabels: map[string]string{"env": "prod"},
			},
			element: matchable(nil, nil),
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			got, err := Matches(context.Background(), tc.sel, tc.element)
			r.NoError(err)
			r.Equal(tc.want, got)
		})
	}
}

func TestSelector_Expression(t *testing.T) {
	cases := []struct {
		name    string
		expr    string
		element Matchable
		want    bool
		wantErr bool
	}{
		{
			name:    "identity equality",
			expr:    `identity.name == "foo"`,
			element: matchable(map[string]string{"name": "foo"}, nil),
			want:    true,
		},
		{
			name:    "structured label numeric comparison",
			expr:    `labels.retries > 3`,
			element: matchable(nil, map[string]any{"retries": float64(5)}),
			want:    true,
		},
		{
			name:    "nested structured label",
			expr:    `labels.routing.region == "eu"`,
			element: matchable(nil, map[string]any{"routing": map[string]any{"region": "eu"}}),
			want:    true,
		},
		{
			name:    "semverCheck matches",
			expr:    `semverCheck(identity.version, ">=2.7.0, <2.10.0")`,
			element: matchable(map[string]string{"version": "2.8.1"}, nil),
			want:    true,
		},
		{
			name:    "semverCheck rejects out-of-range",
			expr:    `semverCheck(identity.version, ">=2.7.0, <2.10.0")`,
			element: matchable(map[string]string{"version": "2.10.0"}, nil),
			want:    false,
		},
		{
			name:    "semverCheck on non-semver is false",
			expr:    `semverCheck(identity.version, ">=1.0.0")`,
			element: matchable(map[string]string{"version": "not-semver"}, nil),
			want:    false,
		},
		{
			name:    "missing identity attribute yields no match, not an error",
			expr:    `identity.region == "eu"`,
			element: matchable(map[string]string{"name": "foo"}, nil),
			want:    false,
		},
		{
			name:    "missing label yields no match, not an error",
			expr:    `labels.env == "prod"`,
			element: matchable(nil, nil),
			want:    false,
		},
		{
			name:    "has() lets users be explicit about optional attributes",
			expr:    `has(identity.region) && identity.region == "eu"`,
			element: matchable(map[string]string{"name": "foo"}, nil),
			want:    false,
		},
		{
			name:    "invalid syntax surfaces compile error",
			expr:    `identity.name ==`,
			element: matchable(nil, nil),
			wantErr: true,
		},
		{
			name:    "non-bool return type surfaces runtime error",
			expr:    `identity.name`,
			element: matchable(map[string]string{"name": "foo"}, nil),
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			got, err := Matches(context.Background(), &v1alpha1.Selector{Expression: tc.expr}, tc.element)
			if tc.wantErr {
				r.Error(err)
				return
			}
			r.NoError(err)
			r.Equal(tc.want, got)
		})
	}
}

func TestSelector_AllClausesAreANDed(t *testing.T) {
	r := require.New(t)
	sel := &v1alpha1.Selector{
		MatchIdentity: map[string]string{"name": "foo"},
		MatchLabels:   map[string]string{"env": "prod"},
		Expression:    `semverCheck(identity.version, ">=1.0.0")`,
	}
	// all three satisfied
	got, err := Matches(context.Background(), sel, matchable(
		map[string]string{"name": "foo", "version": "1.2.3"},
		map[string]any{"env": "prod"},
	))
	r.NoError(err)
	r.True(got)

	// identity mismatch fails fast without touching CEL
	got, err = Matches(context.Background(), sel, matchable(
		map[string]string{"name": "bar", "version": "1.2.3"},
		map[string]any{"env": "prod"},
	))
	r.NoError(err)
	r.False(got)

	// labels mismatch
	got, err = Matches(context.Background(), sel, matchable(
		map[string]string{"name": "foo", "version": "1.2.3"},
		map[string]any{"env": "dev"},
	))
	r.NoError(err)
	r.False(got)

	// expression mismatch
	got, err = Matches(context.Background(), sel, matchable(
		map[string]string{"name": "foo", "version": "0.9.0"},
		map[string]any{"env": "prod"},
	))
	r.NoError(err)
	r.False(got)
}
