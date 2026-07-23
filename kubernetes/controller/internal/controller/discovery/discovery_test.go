package discovery

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	descruntime "ocm.software/open-component-model/bindings/go/descriptor/runtime"
	"ocm.software/open-component-model/bindings/go/runtime"
	"ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
)

// identityKey stringifies {name, version} the same way the DAG vertex keys
// are stringified in Reconcile. Kept here so the tests don't depend on a
// production helper that no longer exists.
func identityKey(name, version string) string {
	return runtime.Identity{
		descruntime.IdentityAttributeName:    name,
		descruntime.IdentityAttributeVersion: version,
	}.String()
}

func TestIdentityKey_Format(t *testing.T) {
	// The stringified identity is round-tripped by runtime.ParseIdentity in
	// resolverAndDiscoverer.Resolve, so guard the exact stringification.
	got := identityKey("ocm.software/foo", "1.2.3")
	want := "name=ocm.software/foo,version=1.2.3"
	if got != want {
		t.Fatalf("identityKey = %q, want %q", got, want)
	}
}

func TestUniqueTargetFromSelector(t *testing.T) {
	cases := []struct {
		name     string
		sel      *v1alpha1.Selector
		nameKey  string
		wantOK   bool
		wantName string
		wantVers string
	}{
		{
			name:   "nil selector",
			sel:    nil,
			wantOK: false,
		},
		{
			name: "identity name+version only, short-circuit",
			sel: &v1alpha1.Selector{
				MatchIdentity: map[string]string{
					IdentityAttributeComponentName: "ocm.software/foo",
					"version":                      "1.2.3",
				},
			},
			nameKey:  IdentityAttributeComponentName,
			wantOK:   true,
			wantName: "ocm.software/foo",
			wantVers: "1.2.3",
		},
		{
			name: "identity missing version, no short-circuit",
			sel: &v1alpha1.Selector{
				MatchIdentity: map[string]string{
					IdentityAttributeComponentName: "ocm.software/foo",
				},
			},
			nameKey: IdentityAttributeComponentName,
			wantOK:  false,
		},
		{
			// Regression: adding MatchLabels alongside a concrete identity
			// used to arm the short-circuit and drop matching references
			// reachable via cancelled sibling paths (multi-parent to same
			// target, per-edge labels differ).
			name: "identity + matchLabels, no short-circuit",
			sel: &v1alpha1.Selector{
				MatchIdentity: map[string]string{
					IdentityAttributeComponentName: "ocm.software/foo",
					"version":                      "1.2.3",
				},
				MatchLabels: map[string]string{"env": "prod"},
			},
			nameKey: IdentityAttributeComponentName,
			wantOK:  false,
		},
		{
			name: "identity + expression, no short-circuit",
			sel: &v1alpha1.Selector{
				MatchIdentity: map[string]string{
					IdentityAttributeComponentName: "ocm.software/foo",
					"version":                      "1.2.3",
				},
				Expression: `identity.version.startsWith("1.")`,
			},
			nameKey: IdentityAttributeComponentName,
			wantOK:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, vers, ok := uniqueTargetFromSelector(tc.sel, tc.nameKey)
			if ok != tc.wantOK || name != tc.wantName || vers != tc.wantVers {
				t.Fatalf("uniqueTargetFromSelector = (%q, %q, %v); want (%q, %q, %v)",
					name, vers, ok, tc.wantName, tc.wantVers, tc.wantOK)
			}
		})
	}
}

var _ = Describe("resolverAndDiscoverer.Discover", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("enumerates a component's references as child keys", func() {
		parent := &descruntime.Descriptor{
			Component: descruntime.Component{
				ComponentMeta: descruntime.ComponentMeta{
					ObjectMeta: descruntime.ObjectMeta{Name: "root", Version: "1.0.0"},
				},
				References: []descruntime.Reference{
					{
						ElementMeta: descruntime.ElementMeta{
							ObjectMeta: descruntime.ObjectMeta{Name: "ref-a", Version: "2.0.0"},
						},
						Component: "ocm.software/child-a",
					},
					{
						ElementMeta: descruntime.ElementMeta{
							ObjectMeta: descruntime.ObjectMeta{Name: "ref-b", Version: "3.0.0"},
						},
						Component: "ocm.software/child-b",
					},
				},
			},
		}
		r := &resolverAndDiscoverer{}
		children, err := r.Discover(ctx, parent)
		Expect(err).ToNot(HaveOccurred())
		Expect(children).To(ConsistOf(
			identityKey("ocm.software/child-a", "2.0.0"),
			identityKey("ocm.software/child-b", "3.0.0"),
		))
	})

	It("returns errShortCircuit when the parent key matches shortCircuitKey", func() {
		parent := &descruntime.Descriptor{
			Component: descruntime.Component{
				ComponentMeta: descruntime.ComponentMeta{
					ObjectMeta: descruntime.ObjectMeta{Name: "target", Version: "9.9.9"},
				},
				References: []descruntime.Reference{
					// This reference would otherwise become a child. The short-circuit
					// must fire before it is enumerated.
					{
						ElementMeta: descruntime.ElementMeta{
							ObjectMeta: descruntime.ObjectMeta{Name: "should-not-fire", Version: "1.0.0"},
						},
						Component: "ocm.software/unreachable",
					},
				},
			},
		}
		r := &resolverAndDiscoverer{
			shortCircuitKey: identityKey("target", "9.9.9"),
		}
		children, err := r.Discover(ctx, parent)
		Expect(children).To(BeNil())
		Expect(errors.Is(err, errShortCircuit)).To(BeTrue())
	})

	It("does not short-circuit when the key does not match", func() {
		parent := &descruntime.Descriptor{
			Component: descruntime.Component{
				ComponentMeta: descruntime.ComponentMeta{
					ObjectMeta: descruntime.ObjectMeta{Name: "not-target", Version: "1.0.0"},
				},
				References: []descruntime.Reference{
					{
						ElementMeta: descruntime.ElementMeta{
							ObjectMeta: descruntime.ObjectMeta{Name: "ref", Version: "2.0.0"},
						},
						Component: "ocm.software/child",
					},
				},
			},
		}
		r := &resolverAndDiscoverer{
			shortCircuitKey: identityKey("target", "9.9.9"),
		}
		children, err := r.Discover(ctx, parent)
		Expect(err).ToNot(HaveOccurred())
		Expect(children).To(HaveLen(1))
	})

	It("returns an empty child slice when there are no references", func() {
		parent := &descruntime.Descriptor{
			Component: descruntime.Component{
				ComponentMeta: descruntime.ComponentMeta{
					ObjectMeta: descruntime.ObjectMeta{Name: "leaf", Version: "1.0.0"},
				},
			},
		}
		r := &resolverAndDiscoverer{}
		children, err := r.Discover(ctx, parent)
		Expect(err).ToNot(HaveOccurred())
		Expect(children).To(BeEmpty())
	})
})
