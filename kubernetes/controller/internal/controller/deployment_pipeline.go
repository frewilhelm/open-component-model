package controller

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	deliveryv1alpha1 "ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
)

// OCMPipeline holds the parameters accumulated by CEL pipeline functions.
// It is used for deterministic naming and deduplication of auto-created CRs.
type OCMPipeline struct {
	RepoURL      string
	RepoType     string
	RepoInterval string
	Component    string
	Semver       string
	CompInterval string
	ResourceID   map[string]string

	// Verify holds signature verification entries to be added to the Component CR.
	Verify []VerificationEntry

	// ReferencePath holds component reference names for navigating nested components.
	// Each entry is one hop in the reference chain.
	ReferencePath []string
}

// VerificationEntry represents a single signature verification configuration.
type VerificationEntry struct {
	Signature string
	SecretRef string // K8s Secret name (mutually exclusive with Value)
	Value     string // inline PEM-encoded public key
}

// RepoKey returns a deduplication key for the Repository portion.
func (p *OCMPipeline) RepoKey() string {
	return fmt.Sprintf("repo:%s:%s:%s", p.RepoURL, p.RepoType, p.RepoInterval)
}

// CompKey returns a deduplication key for Repository+Component.
func (p *OCMPipeline) CompKey() string {
	return fmt.Sprintf("%s|comp:%s:%s:%s", p.RepoKey(), p.Component, p.Semver, p.CompInterval)
}

// ResKey returns a deduplication key for the full pipeline.
func (p *OCMPipeline) ResKey() string {
	keys := make([]string, 0, len(p.ResourceID))
	for k := range p.ResourceID {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, "%s=%s,", k, p.ResourceID[k])
	}
	return fmt.Sprintf("%s|res:%s", p.CompKey(), sb.String())
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:4])
}

// RepoName returns the deterministic name for the auto-created Repository.
func (p *OCMPipeline) RepoName(deploymentName string) string {
	return truncateName(fmt.Sprintf("%s-%s-repo", deploymentName, shortHash(p.RepoKey())), 63)
}

// CompName returns the deterministic name for the auto-created Component.
func (p *OCMPipeline) CompName(deploymentName string) string {
	return truncateName(fmt.Sprintf("%s-%s-comp", deploymentName, shortHash(p.CompKey())), 63)
}

// ResName returns the deterministic name for the auto-created Resource.
func (p *OCMPipeline) ResName(deploymentName string) string {
	return truncateName(fmt.Sprintf("%s-%s-res", deploymentName, shortHash(p.ResKey())), 63)
}

func truncateName(name string, maxLen int) string {
	if len(name) <= maxLen {
		return name
	}
	return name[:maxLen]
}

// allTOCIFields lists every field that toOCI() can produce. We include all
// of them in additionalStatusFields so the Resource controller evaluates them
// regardless of which ones the Deployment templates actually reference.
var allTOCIFields = []string{"digest", "host", "reference", "registry", "repository", "tag"}

// buildAdditionalStatusFields constructs the additionalStatusFields JSON
// for a Resource CR given the list of toOCI fields needed.
func buildAdditionalStatusFields(fields []string) ([]byte, error) {
	m := make(map[string]string, len(fields))
	for _, f := range fields {
		m[f] = fmt.Sprintf("resource.access.toOCI().%s", f)
	}
	return json.Marshal(m)
}

// pipelineStateKey is the marker key in the pipeline state map to indicate
// it is an OCM pipeline state (not a regular map value).
const pipelineStateKey = "_pipeline"

// --- Resource Index ---

// ResolvedResourceEntry maps a discovered resource back to the pipeline
// (repo + component) that provides it.
type ResolvedResourceEntry struct {
	Pipeline   *OCMPipeline
	Component  string            // full OCM component name
	ResourceID map[string]string // resource identity from the descriptor
}

// ResourceIndex maps resource identities discovered via component descriptor
// introspection to the pipelines that provide them. Built during Phase 0
// (pre-scan) so that the resource() CEL shorthand can resolve resources
// without requiring the full repoByRef().componentByReference() chain.
type ResourceIndex struct {
	// byName maps a simple resource name to all entries that match.
	// A slice is used to detect ambiguity (same name in multiple components).
	byName map[string][]ResolvedResourceEntry
}

// NewResourceIndex creates an empty ResourceIndex.
func NewResourceIndex() *ResourceIndex {
	return &ResourceIndex{
		byName: make(map[string][]ResolvedResourceEntry),
	}
}

// Add registers a resource entry in the index.
// Duplicate entries (same component + resource name) are silently skipped.
func (idx *ResourceIndex) Add(entry ResolvedResourceEntry) {
	name := entry.ResourceID["name"]
	if name == "" {
		return
	}
	// Deduplicate: skip if this component already registered this resource name.
	for _, existing := range idx.byName[name] {
		if existing.Component == entry.Component {
			return
		}
	}
	idx.byName[name] = append(idx.byName[name], entry)
}

// Lookup finds a resource by name. Returns an error if not found or ambiguous.
func (idx *ResourceIndex) Lookup(name string) (*ResolvedResourceEntry, error) {
	entries, ok := idx.byName[name]
	if !ok || len(entries) == 0 {
		return nil, fmt.Errorf("resource %q not found in any declared component", name)
	}
	if len(entries) > 1 {
		components := make([]string, len(entries))
		for i, e := range entries {
			components[i] = e.Component
		}
		return nil, fmt.Errorf(
			"resource %q found in multiple components: %s — use component(%q).resource(%q) to disambiguate",
			name, strings.Join(components, ", "), components[0], name,
		)
	}
	return &entries[0], nil
}

// LookupForComponent finds a resource by name within a specific component.
func (idx *ResourceIndex) LookupForComponent(componentName, resourceName string) (*ResolvedResourceEntry, error) {
	entries, ok := idx.byName[resourceName]
	if !ok || len(entries) == 0 {
		return nil, fmt.Errorf("resource %q not found in any declared component", resourceName)
	}
	for i, e := range entries {
		if e.Component == componentName {
			return &entries[i], nil
		}
	}
	return nil, fmt.Errorf("resource %q not found in component %q", resourceName, componentName)
}

// --- Pre-scan Parsing ---

// preScanTemplates walks all resource templates, extracts ${} expressions,
// and parses full pipeline chains (repoByRef + componentByReference) to
// produce:
//   - a deduplicated list of OCMPipelines (keyed by CompKey) for creating Repository + Component CRs
//   - a list of all expression-parsed resource entries for populating the resource index
//
// The resource entries are needed because some resources live in nested
// components whose descriptors aren't directly accessible from the root.
func preScanTemplates(resources []deliveryv1alpha1.OCMDeploymentResource) ([]*OCMPipeline, []ResolvedResourceEntry, error) {
	seen := make(map[string]*OCMPipeline) // keyed by CompKey for dedup
	var expressionResources []ResolvedResourceEntry

	for _, res := range resources {
		templateMap, err := templateToMap(res.Template.Raw)
		if err != nil {
			continue // skip unparseable templates; they'll error later
		}

		walkStrings(templateMap, func(s string) {
			for _, expr := range extractExpressions(s) {
				pipeline, ok := parsePipelineChain(expr)
				if !ok {
					continue
				}
				key := pipeline.CompKey()
				if _, exists := seen[key]; !exists {
					seen[key] = pipeline
				}

				// If the expression also contains a resourceByIdentity call,
				// record it so we can register it in the resource index even
				// when the resource lives in a nested component.
				// We use the per-expression pipeline (not the shared one) because
				// each expression may have a different ReferencePath or Verify.
				if len(pipeline.ResourceID) > 0 {
					expressionResources = append(expressionResources, ResolvedResourceEntry{
						Pipeline:   pipeline,
						Component:  pipeline.Component,
						ResourceID: pipeline.ResourceID,
					})
				}
			}
		})
	}

	pipelines := make([]*OCMPipeline, 0, len(seen))
	for _, p := range seen {
		pipelines = append(pipelines, p)
	}
	return pipelines, expressionResources, nil
}

// parsePipelineChain attempts to extract repoByRef and componentByReference
// parameters from an expression string. Returns the pipeline and true if
// both function calls are found; false otherwise.
func parsePipelineChain(expr string) (*OCMPipeline, bool) {
	pipeline := &OCMPipeline{
		RepoType:     "OCIRegistry",
		RepoInterval: "10m",
		Semver:       ">=0.0.0",
		CompInterval: "10m",
	}

	// Extract repoByRef parameters.
	repoArgs, ok := extractFuncArgs(expr, "repoByRef")
	if !ok || len(repoArgs) == 0 {
		return nil, false
	}
	pipeline.RepoURL = unquote(repoArgs[0])
	if len(repoArgs) > 1 {
		if opts := parseMapLiteral(repoArgs[1]); opts != nil {
			if v, ok := opts["type"]; ok {
				pipeline.RepoType = v
			}
			if v, ok := opts["interval"]; ok {
				pipeline.RepoInterval = v
			}
		}
	}

	// Extract componentByReference parameters.
	compArgs, ok := extractFuncArgs(expr, "componentByReference")
	if !ok || len(compArgs) == 0 {
		return nil, false
	}
	pipeline.Component = unquote(compArgs[0])
	if len(compArgs) > 1 {
		if opts := parseMapLiteral(compArgs[1]); opts != nil {
			if v, ok := opts["semver"]; ok {
				pipeline.Semver = v
			}
			if v, ok := opts["interval"]; ok {
				pipeline.CompInterval = v
			}
		}
	}

	// Extract withVerificationFromSecret parameters.
	if vArgs, ok := extractFuncArgs(expr, "withVerificationFromSecret"); ok && len(vArgs) >= 2 {
		pipeline.Verify = append(pipeline.Verify, VerificationEntry{
			Signature: unquote(vArgs[0]),
			SecretRef: unquote(vArgs[1]),
		})
	}

	// Extract withVerification parameters (only if NOT already captured by withVerificationFromSecret).
	// We check by looking for "withVerification(" that is NOT preceded by "From" (i.e., not part of withVerificationFromSecret).
	if vArgs, ok := extractNonPrefixedFuncArgs(expr, "withVerification", "withVerificationFromSecret"); ok && len(vArgs) >= 2 {
		pipeline.Verify = append(pipeline.Verify, VerificationEntry{
			Signature: unquote(vArgs[0]),
			Value:     unquote(vArgs[1]),
		})
	}

	// Extract componentReference parameters.
	if refArgs, ok := extractFuncArgs(expr, "componentReference"); ok && len(refArgs) >= 1 {
		for _, arg := range refArgs {
			pipeline.ReferencePath = append(pipeline.ReferencePath, unquote(arg))
		}
	}

	// Extract resourceByIdentity parameters — used to populate the resource index
	// for expressions that reference resources in nested components (where the root
	// descriptor doesn't contain the resource directly).
	if resArgs, ok := extractFuncArgs(expr, "resourceByIdentity"); ok && len(resArgs) >= 1 {
		if m := parseMapLiteral(resArgs[0]); m != nil {
			pipeline.ResourceID = m
		}
	}

	return pipeline, true
}

// extractFuncArgs finds a function call by name in an expression string
// and returns the raw argument strings. Handles nested braces/parens.
func extractFuncArgs(expr, funcName string) ([]string, bool) {
	return extractFuncArgsAt(expr, funcName, 0)
}

// extractNonPrefixedFuncArgs finds a function call by name but only if it's
// not part of a longer function name (e.g., "withVerification" should not match
// "withVerificationFromSecret"). It does this by checking that the match is
// preceded by a non-alphanumeric character or is at the start of the string.
func extractNonPrefixedFuncArgs(expr, funcName, excludePrefix string) ([]string, bool) {
	// Search for all occurrences of funcName( and check they're not part of excludePrefix.
	searchFrom := 0
	for {
		idx := strings.Index(expr[searchFrom:], funcName+"(")
		if idx < 0 {
			return nil, false
		}
		absIdx := searchFrom + idx

		// Check if this is actually the excludePrefix function.
		excludeIdx := strings.Index(expr[searchFrom:], excludePrefix+"(")
		if excludeIdx >= 0 && searchFrom+excludeIdx == absIdx {
			// This match is the excluded prefix, skip past it.
			searchFrom = absIdx + len(excludePrefix)
			continue
		}

		return extractFuncArgsAt(expr, funcName, absIdx)
	}
}

// extractFuncArgsAt finds a function call starting at a specific position.
func extractFuncArgsAt(expr, funcName string, searchFrom int) ([]string, bool) {
	idx := strings.Index(expr[searchFrom:], funcName+"(")
	if idx < 0 {
		return nil, false
	}
	absIdx := searchFrom + idx

	// Find the opening paren.
	start := absIdx + len(funcName) + 1 // position after '('

	// Find matching closing paren, tracking depth.
	depth := 1
	end := start
	inString := false
	for end < len(expr) && depth > 0 {
		ch := expr[end]
		if inString {
			if ch == '\\' && end+1 < len(expr) {
				end++ // skip escaped char
			} else if ch == '"' {
				inString = false
			}
		} else {
			switch ch {
			case '"':
				inString = true
			case '(':
				depth++
			case ')':
				depth--
			case '{':
				depth++
			case '}':
				depth--
			}
		}
		if depth > 0 {
			end++
		}
	}

	if depth != 0 {
		return nil, false
	}

	argsStr := expr[start:end]
	return splitArgs(argsStr), true
}

// splitArgs splits a comma-separated argument string, respecting nested
// structures (strings, braces, parens).
func splitArgs(s string) []string {
	var args []string
	depth := 0
	inString := false
	start := 0

	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inString {
			if ch == '\\' && i+1 < len(s) {
				i++
			} else if ch == '"' {
				inString = false
			}
		} else {
			switch ch {
			case '"':
				inString = true
			case '(', '{', '[':
				depth++
			case ')', '}', ']':
				depth--
			case ',':
				if depth == 0 {
					arg := strings.TrimSpace(s[start:i])
					if arg != "" {
						args = append(args, arg)
					}
					start = i + 1
				}
			}
		}
	}

	// Last argument.
	arg := strings.TrimSpace(s[start:])
	if arg != "" {
		args = append(args, arg)
	}
	return args
}

// parseMapLiteral parses a simple CEL map literal like {"key": "value", "k2": "v2"}
// into a Go map. Only handles string keys and string values.
func parseMapLiteral(s string) map[string]string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil
	}
	inner := s[1 : len(s)-1]
	result := make(map[string]string)
	for _, pair := range splitArgs(inner) {
		parts := strings.SplitN(pair, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := unquote(strings.TrimSpace(parts[0]))
		val := unquote(strings.TrimSpace(parts[1]))
		result[key] = val
	}
	return result
}

// unquote removes surrounding double quotes from a string if present.
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
