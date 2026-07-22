package selector

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"

	"ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
)

// Matchable represents an element that can be matched against a Selector.
type Matchable struct {
	// Identity contains the element's identity attributes (name, version, extra identity).
	Identity map[string]string
	// Labels contains the element's label names mapped to their string values.
	Labels map[string]string
}

// Matches evaluates whether the given element matches all requirements in the selector.
// A nil selector matches everything.
func Matches(s *v1alpha1.Selector, element Matchable) (bool, error) {
	if s == nil {
		return true, nil
	}

	// MatchIdentity: each entry is equality on identity attributes (ANDed).
	for key, value := range s.MatchIdentity {
		if actual, exists := element.Identity[key]; !exists || actual != value {
			return false, nil
		}
	}

	// MatchLabels: each entry is equality on element labels (ANDed).
	for name, value := range s.MatchLabels {
		if actual, exists := element.Labels[name]; !exists || actual != value {
			return false, nil
		}
	}

	// MatchExpressions: each requirement is ANDed.
	for _, req := range s.MatchExpressions {
		match, err := matchRequirement(req, element)
		if err != nil {
			return false, err
		}
		if !match {
			return false, nil
		}
	}

	return true, nil
}

func matchRequirement(r v1alpha1.SelectorRequirement, element Matchable) (bool, error) {
	if r.Key == "labels" {
		return matchLabels(r, element.Labels)
	}
	return matchIdentity(r, element.Identity)
}

func matchIdentity(r v1alpha1.SelectorRequirement, identity map[string]string) (bool, error) {
	value, exists := identity[r.Key]

	switch r.Operator {
	case v1alpha1.SelectorOpIn:
		if !exists {
			return false, nil
		}
		for _, v := range r.Values {
			if v == value {
				return true, nil
			}
		}
		return false, nil

	case v1alpha1.SelectorOpNotIn:
		if !exists {
			return true, nil
		}
		for _, v := range r.Values {
			if v == value {
				return false, nil
			}
		}
		return true, nil

	case v1alpha1.SelectorOpExists:
		return exists, nil

	case v1alpha1.SelectorOpDoesNotExist:
		return !exists, nil

	case v1alpha1.SelectorOpSemverRange:
		if !exists {
			return false, nil
		}
		if len(r.Values) != 1 {
			return false, fmt.Errorf("SemverRange operator requires exactly one value, got %d", len(r.Values))
		}
		constraint, err := semver.NewConstraint(r.Values[0])
		if err != nil {
			return false, fmt.Errorf("invalid semver constraint %q: %w", r.Values[0], err)
		}
		v, err := semver.NewVersion(value)
		if err != nil {
			return false, fmt.Errorf("invalid semver version %q: %w", value, err)
		}
		return constraint.Check(v), nil

	default:
		return false, fmt.Errorf("unknown operator %q", r.Operator)
	}
}

func matchLabels(r v1alpha1.SelectorRequirement, labels map[string]string) (bool, error) {
	switch r.Operator {
	case v1alpha1.SelectorOpExists:
		for _, name := range r.Values {
			if _, exists := labels[name]; !exists {
				return false, nil
			}
		}
		return true, nil

	case v1alpha1.SelectorOpDoesNotExist:
		for _, name := range r.Values {
			if _, exists := labels[name]; exists {
				return false, nil
			}
		}
		return true, nil

	case v1alpha1.SelectorOpIn:
		for _, entry := range r.Values {
			name, val, ok := parseLabelEntry(entry)
			if !ok {
				return false, fmt.Errorf("invalid label entry %q: expected format \"name=value\"", entry)
			}
			if labelVal, exists := labels[name]; exists && labelVal == val {
				return true, nil
			}
		}
		return false, nil

	case v1alpha1.SelectorOpNotIn:
		for _, entry := range r.Values {
			name, val, ok := parseLabelEntry(entry)
			if !ok {
				return false, fmt.Errorf("invalid label entry %q: expected format \"name=value\"", entry)
			}
			if labelVal, exists := labels[name]; exists && labelVal == val {
				return false, nil
			}
		}
		return true, nil

	case v1alpha1.SelectorOpSemverRange:
		return false, fmt.Errorf("SemverRange operator is not supported for labels")

	default:
		return false, fmt.Errorf("unknown operator %q", r.Operator)
	}
}

func parseLabelEntry(entry string) (name, value string, ok bool) {
	parts := strings.SplitN(entry, "=", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}
