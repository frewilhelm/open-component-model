package selector

import (
	"testing"

	"github.com/stretchr/testify/require"

	"ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
)

func TestSelector_NilMatchesEverything(t *testing.T) {
	r := require.New(t)

	match, err := Matches(nil, Matchable{
		Identity: map[string]string{"name": "foo"},
	})
	r.NoError(err)
	r.True(match)
}

func TestSelector_EmptyMatchesEverything(t *testing.T) {
	r := require.New(t)

	match, err := Matches(&v1alpha1.Selector{}, Matchable{
		Identity: map[string]string{"name": "foo", "version": "v1.0.0"},
	})
	r.NoError(err)
	r.True(match)
}

func TestSelector_MatchIdentity(t *testing.T) {
	r := require.New(t)

	tests := []struct {
		name     string
		selector *v1alpha1.Selector
		element  Matchable
		expected bool
	}{
		{
			name: "single key matches",
			selector: &v1alpha1.Selector{
				MatchIdentity: map[string]string{"name": "flux"},
			},
			element:  Matchable{Identity: map[string]string{"name": "flux", "version": "v1.0.0"}},
			expected: true,
		},
		{
			name: "single key does not match",
			selector: &v1alpha1.Selector{
				MatchIdentity: map[string]string{"name": "helm"},
			},
			element:  Matchable{Identity: map[string]string{"name": "flux", "version": "v1.0.0"}},
			expected: false,
		},
		{
			name: "multiple keys all match",
			selector: &v1alpha1.Selector{
				MatchIdentity: map[string]string{"name": "flux", "version": "v1.0.0"},
			},
			element:  Matchable{Identity: map[string]string{"name": "flux", "version": "v1.0.0"}},
			expected: true,
		},
		{
			name: "one of multiple keys does not match",
			selector: &v1alpha1.Selector{
				MatchIdentity: map[string]string{"name": "flux", "version": "v2.0.0"},
			},
			element:  Matchable{Identity: map[string]string{"name": "flux", "version": "v1.0.0"}},
			expected: false,
		},
		{
			name: "key not present in identity",
			selector: &v1alpha1.Selector{
				MatchIdentity: map[string]string{"arch": "amd64"},
			},
			element:  Matchable{Identity: map[string]string{"name": "flux"}},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match, err := Matches(tt.selector, tt.element)
			r.NoError(err)
			r.Equal(tt.expected, match)
		})
	}
}

func TestSelector_MatchLabels(t *testing.T) {
	r := require.New(t)

	tests := []struct {
		name     string
		selector *v1alpha1.Selector
		element  Matchable
		expected bool
	}{
		{
			name: "label matches",
			selector: &v1alpha1.Selector{
				MatchLabels: map[string]string{"tier": "production"},
			},
			element: Matchable{
				Identity: map[string]string{"name": "flux"},
				Labels:   map[string]string{"tier": "production"},
			},
			expected: true,
		},
		{
			name: "label value does not match",
			selector: &v1alpha1.Selector{
				MatchLabels: map[string]string{"tier": "production"},
			},
			element: Matchable{
				Identity: map[string]string{"name": "flux"},
				Labels:   map[string]string{"tier": "staging"},
			},
			expected: false,
		},
		{
			name: "label key not present",
			selector: &v1alpha1.Selector{
				MatchLabels: map[string]string{"tier": "production"},
			},
			element: Matchable{
				Identity: map[string]string{"name": "flux"},
				Labels:   map[string]string{},
			},
			expected: false,
		},
		{
			name: "nil labels fails match",
			selector: &v1alpha1.Selector{
				MatchLabels: map[string]string{"tier": "production"},
			},
			element: Matchable{
				Identity: map[string]string{"name": "flux"},
				Labels:   nil,
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match, err := Matches(tt.selector, tt.element)
			r.NoError(err)
			r.Equal(tt.expected, match)
		})
	}
}

func TestSelector_MatchExpressions_In(t *testing.T) {
	r := require.New(t)

	tests := []struct {
		name     string
		selector *v1alpha1.Selector
		element  Matchable
		expected bool
	}{
		{
			name: "matches single value",
			selector: &v1alpha1.Selector{
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "name", Operator: v1alpha1.SelectorOpIn, Values: []string{"flux"}},
				},
			},
			element:  Matchable{Identity: map[string]string{"name": "flux", "version": "v1.0.0"}},
			expected: true,
		},
		{
			name: "matches one of multiple values",
			selector: &v1alpha1.Selector{
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "name", Operator: v1alpha1.SelectorOpIn, Values: []string{"flux", "helm"}},
				},
			},
			element:  Matchable{Identity: map[string]string{"name": "helm"}},
			expected: true,
		},
		{
			name: "does not match",
			selector: &v1alpha1.Selector{
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "name", Operator: v1alpha1.SelectorOpIn, Values: []string{"flux", "helm"}},
				},
			},
			element:  Matchable{Identity: map[string]string{"name": "kustomize"}},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match, err := Matches(tt.selector, tt.element)
			r.NoError(err)
			r.Equal(tt.expected, match)
		})
	}
}

func TestSelector_MatchExpressions_NotIn(t *testing.T) {
	r := require.New(t)

	tests := []struct {
		name     string
		selector *v1alpha1.Selector
		element  Matchable
		expected bool
	}{
		{
			name: "does not match excluded value",
			selector: &v1alpha1.Selector{
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "name", Operator: v1alpha1.SelectorOpNotIn, Values: []string{"test-helper"}},
				},
			},
			element:  Matchable{Identity: map[string]string{"name": "flux"}},
			expected: true,
		},
		{
			name: "matches excluded value",
			selector: &v1alpha1.Selector{
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "name", Operator: v1alpha1.SelectorOpNotIn, Values: []string{"test-helper"}},
				},
			},
			element:  Matchable{Identity: map[string]string{"name": "test-helper"}},
			expected: false,
		},
		{
			name: "key not present passes NotIn",
			selector: &v1alpha1.Selector{
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "arch", Operator: v1alpha1.SelectorOpNotIn, Values: []string{"arm64"}},
				},
			},
			element:  Matchable{Identity: map[string]string{"name": "flux"}},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match, err := Matches(tt.selector, tt.element)
			r.NoError(err)
			r.Equal(tt.expected, match)
		})
	}
}

func TestSelector_MatchExpressions_Exists(t *testing.T) {
	r := require.New(t)

	s := &v1alpha1.Selector{
		MatchExpressions: []v1alpha1.SelectorRequirement{
			{Key: "version", Operator: v1alpha1.SelectorOpExists},
		},
	}

	match, err := Matches(s, Matchable{Identity: map[string]string{"name": "flux", "version": "v1.0.0"}})
	r.NoError(err)
	r.True(match)

	match, err = Matches(s, Matchable{Identity: map[string]string{"name": "flux"}})
	r.NoError(err)
	r.False(match)
}

func TestSelector_MatchExpressions_DoesNotExist(t *testing.T) {
	r := require.New(t)

	s := &v1alpha1.Selector{
		MatchExpressions: []v1alpha1.SelectorRequirement{
			{Key: "arch", Operator: v1alpha1.SelectorOpDoesNotExist},
		},
	}

	match, err := Matches(s, Matchable{Identity: map[string]string{"name": "flux"}})
	r.NoError(err)
	r.True(match)

	match, err = Matches(s, Matchable{Identity: map[string]string{"name": "flux", "arch": "amd64"}})
	r.NoError(err)
	r.False(match)
}

func TestSelector_MatchExpressions_SemverRange(t *testing.T) {
	r := require.New(t)

	tests := []struct {
		name     string
		selector *v1alpha1.Selector
		element  Matchable
		expected bool
		wantErr  bool
	}{
		{
			name: "version in range",
			selector: &v1alpha1.Selector{
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "version", Operator: v1alpha1.SelectorOpSemverRange, Values: []string{">=2.2.0, <2.3.0"}},
				},
			},
			element:  Matchable{Identity: map[string]string{"name": "flux", "version": "v2.2.3"}},
			expected: true,
		},
		{
			name: "version below range",
			selector: &v1alpha1.Selector{
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "version", Operator: v1alpha1.SelectorOpSemverRange, Values: []string{">=2.2.0, <2.3.0"}},
				},
			},
			element:  Matchable{Identity: map[string]string{"name": "flux", "version": "v2.1.9"}},
			expected: false,
		},
		{
			name: "version above range",
			selector: &v1alpha1.Selector{
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "version", Operator: v1alpha1.SelectorOpSemverRange, Values: []string{">=2.2.0, <2.3.0"}},
				},
			},
			element:  Matchable{Identity: map[string]string{"name": "flux", "version": "v2.3.0"}},
			expected: false,
		},
		{
			name: "invalid constraint",
			selector: &v1alpha1.Selector{
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "version", Operator: v1alpha1.SelectorOpSemverRange, Values: []string{"not-valid"}},
				},
			},
			element:  Matchable{Identity: map[string]string{"version": "v1.0.0"}},
			expected: false,
			wantErr:  true,
		},
		{
			name: "invalid version value",
			selector: &v1alpha1.Selector{
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "version", Operator: v1alpha1.SelectorOpSemverRange, Values: []string{">=1.0.0"}},
				},
			},
			element:  Matchable{Identity: map[string]string{"version": "not-a-version"}},
			expected: false,
			wantErr:  true,
		},
		{
			name: "too many values",
			selector: &v1alpha1.Selector{
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "version", Operator: v1alpha1.SelectorOpSemverRange, Values: []string{">=1.0.0", ">=2.0.0"}},
				},
			},
			element:  Matchable{Identity: map[string]string{"version": "v1.5.0"}},
			expected: false,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match, err := Matches(tt.selector, tt.element)
			if tt.wantErr {
				r.Error(err)
			} else {
				r.NoError(err)
			}
			r.Equal(tt.expected, match)
		})
	}
}

func TestSelector_MatchExpressions_Labels_Exists(t *testing.T) {
	r := require.New(t)

	tests := []struct {
		name     string
		selector *v1alpha1.Selector
		element  Matchable
		expected bool
	}{
		{
			name: "label exists",
			selector: &v1alpha1.Selector{
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "labels", Operator: v1alpha1.SelectorOpExists, Values: []string{"verified"}},
				},
			},
			element: Matchable{
				Identity: map[string]string{"name": "image"},
				Labels:   map[string]string{"verified": "true"},
			},
			expected: true,
		},
		{
			name: "label does not exist",
			selector: &v1alpha1.Selector{
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "labels", Operator: v1alpha1.SelectorOpExists, Values: []string{"verified"}},
				},
			},
			element: Matchable{
				Identity: map[string]string{"name": "image"},
				Labels:   map[string]string{"other": "value"},
			},
			expected: false,
		},
		{
			name: "all labels must exist",
			selector: &v1alpha1.Selector{
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "labels", Operator: v1alpha1.SelectorOpExists, Values: []string{"a", "b"}},
				},
			},
			element: Matchable{
				Identity: map[string]string{"name": "image"},
				Labels:   map[string]string{"a": "x", "b": "y"},
			},
			expected: true,
		},
		{
			name: "nil labels",
			selector: &v1alpha1.Selector{
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "labels", Operator: v1alpha1.SelectorOpExists, Values: []string{"deprecated"}},
				},
			},
			element: Matchable{
				Identity: map[string]string{"name": "image"},
				Labels:   nil,
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match, err := Matches(tt.selector, tt.element)
			r.NoError(err)
			r.Equal(tt.expected, match)
		})
	}
}

func TestSelector_MatchExpressions_Labels_DoesNotExist(t *testing.T) {
	r := require.New(t)

	s := &v1alpha1.Selector{
		MatchExpressions: []v1alpha1.SelectorRequirement{
			{Key: "labels", Operator: v1alpha1.SelectorOpDoesNotExist, Values: []string{"deprecated"}},
		},
	}

	match, err := Matches(s, Matchable{
		Identity: map[string]string{"name": "image"},
		Labels:   map[string]string{"other": "value"},
	})
	r.NoError(err)
	r.True(match)

	match, err = Matches(s, Matchable{
		Identity: map[string]string{"name": "image"},
		Labels:   map[string]string{"deprecated": "true"},
	})
	r.NoError(err)
	r.False(match)

	match, err = Matches(s, Matchable{
		Identity: map[string]string{"name": "image"},
		Labels:   nil,
	})
	r.NoError(err)
	r.True(match)
}

func TestSelector_MatchExpressions_Labels_In(t *testing.T) {
	r := require.New(t)

	tests := []struct {
		name     string
		selector *v1alpha1.Selector
		element  Matchable
		expected bool
		wantErr  bool
	}{
		{
			name: "matches key=value pair",
			selector: &v1alpha1.Selector{
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "labels", Operator: v1alpha1.SelectorOpIn, Values: []string{"tier=production"}},
				},
			},
			element: Matchable{
				Identity: map[string]string{"name": "image"},
				Labels:   map[string]string{"tier": "production"},
			},
			expected: true,
		},
		{
			name: "matches one of multiple pairs",
			selector: &v1alpha1.Selector{
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "labels", Operator: v1alpha1.SelectorOpIn, Values: []string{"tier=production", "tier=staging"}},
				},
			},
			element: Matchable{
				Identity: map[string]string{"name": "image"},
				Labels:   map[string]string{"tier": "staging"},
			},
			expected: true,
		},
		{
			name: "does not match any pair",
			selector: &v1alpha1.Selector{
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "labels", Operator: v1alpha1.SelectorOpIn, Values: []string{"tier=production"}},
				},
			},
			element: Matchable{
				Identity: map[string]string{"name": "image"},
				Labels:   map[string]string{"tier": "development"},
			},
			expected: false,
		},
		{
			name: "invalid value format",
			selector: &v1alpha1.Selector{
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "labels", Operator: v1alpha1.SelectorOpIn, Values: []string{"no-equals"}},
				},
			},
			element: Matchable{
				Identity: map[string]string{"name": "image"},
				Labels:   map[string]string{},
			},
			expected: false,
			wantErr:  true,
		},
		{
			name: "value containing equals sign",
			selector: &v1alpha1.Selector{
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "labels", Operator: v1alpha1.SelectorOpIn, Values: []string{"config=key=value"}},
				},
			},
			element: Matchable{
				Identity: map[string]string{"name": "image"},
				Labels:   map[string]string{"config": "key=value"},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match, err := Matches(tt.selector, tt.element)
			if tt.wantErr {
				r.Error(err)
			} else {
				r.NoError(err)
			}
			r.Equal(tt.expected, match)
		})
	}
}

func TestSelector_MatchExpressions_Labels_NotIn(t *testing.T) {
	r := require.New(t)

	s := &v1alpha1.Selector{
		MatchExpressions: []v1alpha1.SelectorRequirement{
			{Key: "labels", Operator: v1alpha1.SelectorOpNotIn, Values: []string{"tier=deprecated"}},
		},
	}

	match, err := Matches(s, Matchable{
		Identity: map[string]string{"name": "image"},
		Labels:   map[string]string{"tier": "production"},
	})
	r.NoError(err)
	r.True(match)

	match, err = Matches(s, Matchable{
		Identity: map[string]string{"name": "image"},
		Labels:   map[string]string{"tier": "deprecated"},
	})
	r.NoError(err)
	r.False(match)
}

func TestSelector_CombinedFields_AND(t *testing.T) {
	r := require.New(t)

	tests := []struct {
		name     string
		selector *v1alpha1.Selector
		element  Matchable
		expected bool
	}{
		{
			name: "all fields match",
			selector: &v1alpha1.Selector{
				MatchIdentity: map[string]string{"name": "flux"},
				MatchLabels:   map[string]string{"tier": "production"},
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "version", Operator: v1alpha1.SelectorOpSemverRange, Values: []string{">=2.0.0"}},
				},
			},
			element: Matchable{
				Identity: map[string]string{"name": "flux", "version": "v2.2.0"},
				Labels:   map[string]string{"tier": "production"},
			},
			expected: true,
		},
		{
			name: "identity matches but labels do not",
			selector: &v1alpha1.Selector{
				MatchIdentity: map[string]string{"name": "flux"},
				MatchLabels:   map[string]string{"tier": "production"},
			},
			element: Matchable{
				Identity: map[string]string{"name": "flux"},
				Labels:   map[string]string{"tier": "staging"},
			},
			expected: false,
		},
		{
			name: "labels match but identity does not",
			selector: &v1alpha1.Selector{
				MatchIdentity: map[string]string{"name": "flux"},
				MatchLabels:   map[string]string{"tier": "production"},
			},
			element: Matchable{
				Identity: map[string]string{"name": "helm"},
				Labels:   map[string]string{"tier": "production"},
			},
			expected: false,
		},
		{
			name: "identity and labels match but expression does not",
			selector: &v1alpha1.Selector{
				MatchIdentity: map[string]string{"name": "flux"},
				MatchLabels:   map[string]string{"tier": "production"},
				MatchExpressions: []v1alpha1.SelectorRequirement{
					{Key: "labels", Operator: v1alpha1.SelectorOpDoesNotExist, Values: []string{"deprecated"}},
				},
			},
			element: Matchable{
				Identity: map[string]string{"name": "flux"},
				Labels:   map[string]string{"tier": "production", "deprecated": "true"},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match, err := Matches(tt.selector, tt.element)
			r.NoError(err)
			r.Equal(tt.expected, match)
		})
	}
}

func TestSelector_UnknownOperator(t *testing.T) {
	r := require.New(t)

	s := &v1alpha1.Selector{
		MatchExpressions: []v1alpha1.SelectorRequirement{
			{Key: "name", Operator: "Unknown", Values: []string{"foo"}},
		},
	}
	match, err := Matches(s, Matchable{Identity: map[string]string{"name": "foo"}})
	r.Error(err)
	r.False(match)
}

func TestSelector_SemverRangeOnLabels(t *testing.T) {
	r := require.New(t)

	s := &v1alpha1.Selector{
		MatchExpressions: []v1alpha1.SelectorRequirement{
			{Key: "labels", Operator: v1alpha1.SelectorOpSemverRange, Values: []string{">=1.0.0"}},
		},
	}
	match, err := Matches(s, Matchable{
		Identity: map[string]string{"name": "foo"},
		Labels:   map[string]string{"version": "v1.0.0"},
	})
	r.Error(err)
	r.False(match)
}
