# Deployment: deployer-agnostic single-resource deployment with construction-derived localization

* **Status**: proposed
* **Date**: 2026-10-08

Technical Story: OCM needs a simplified, deployer-agnostic entry point for delivery. This ADR defines
the `Deployment` resource and the contract by which it localizes and configures a Helm chart or
kustomization delivered in a component version. The status quo it improves on is described below.

## Context and Problem Statement

Deploying a Helm chart or kustomization that lives in an OCM component today means hand-authoring a
Kro `ResourceGraphDefinition` that wires a `Resource`, an `OCIRepository`, and a `HelmRelease` or
`Kustomization` (or an Argo `Application`), and encodes localization and configuration by hand (see
`bindings/go/kubernetes/controller/examples/helm-configuration-localization/rgd.yaml`, around 130
lines for one chart plus one image). The deployer technology (Flux vs Argo) leaks into what the
component author bundles, and too many cluster resources are needed to deploy what is essentially a
chart plus its images.

A single applied resource should name a component version, the resource to deploy, a deployer engine,
and a config, and have the controller produce the low-level deploy objects with images localized and
values configured, without KRO.

`Deployment` is an addition, not a replacement. It sits beside the existing `Deployer` CR (which
applies a KRO `ResourceGraphDefinition`) and reuses its apply/prune machinery. Its primary purpose is
to migrate v1 users to v2 and lower the adoption bar for the common case. We accept consciously that
this is neither perfectly clean nor fully deployer-agnostic.

## Decision Drivers

* Single resource for the user to apply and maintain (the controller owns the generated objects).
* Lower the v1 to v2 migration and adoption bar for the common case.
* The component author must not need to know Flux vs Argo.
* Images are localized automatically; the user never edits image references.
* Localization data must survive transfer and be tamper-evident where the chart permits it.
* Do not re-abstract the full Flux/Argo surface; model only a small, deployer-agnostic set of fields (values/patches,
target namespace, release name, interval) and default the rest.
* Configuration can be shipped with the component and layered per deployment.

## Decision Outcome

The `Deployment` is a namespaced wrapper (orchestrating) custom resource
(`delivery.ocm.software/v1alpha1`, kind `Deployment`): it is the only resource the user applies, and
its controller creates and owns the lower-level resources the user would otherwise author by hand. It
is namespaced and applies with a ServiceAccount's permissions, following the `NamespacedDeployer`
direction (the cluster-scoped `Deployer` is being deprecated). The controller stays thin: it creates
the children once and then reacts to their status, instead of re-resolving on every reconcile.

Cleanup has two distinct layers, both same-namespace so owner references stay valid. The child OCM
CRs (`Repository`, `Component`, `Resource`) are created in the `Deployment`'s namespace with the
`Deployment` as owner reference and are garbage-collected with it. The generated deploy objects
(`HelmRelease`/`Kustomization`/Argo `Application`) also live in the `Deployment`'s namespace and are
applied and pruned via the ApplySet machinery (with its finalizer) that the `(Namespaced)Deployer`
uses; `TargetNamespace` only selects where the engine places the rendered workloads, not where the
deploy object itself lives, so there is no cross-namespace owner reference. Deploying to a different
cluster (Flux `spec.kubeConfig`, Argo `destination.server`, both supported) follows the
`NamespacedDeployer` model and is deferred.

Localization is derived at construction and carried as signed per-resource labels; configuration and
localization are applied through the deployer's native mechanism (Helm values, or kustomize
patches/`images`).

### Deployment resource

```go
type DeploymentSpec struct {
    // ComponentVersionRef is an OCM component reference, host/path//component:version,
    // The controller splits off the version segment and resolves the
    // host/path//component portion and creates the child Repository and Component.
    // The version segment is passed verbatim to the child Component's semver field,
    // so it may be an exact version or a semver range.
    ComponentVersionRef string `json:"componentVersionRef"`
    // DeployResource identifies the OCM resource to deploy (helmChart or kustomize).
    // The artifact kind (Helm vs kustomize) is derived from the OCM resource type.
    DeployResource ResourceID `json:"deployResource"`
    // Deployer selects the deployment engine.
    // +kubebuilder:validation:Enum:=FluxCD;ArgoCD
    Deployer DeployerEngine `json:"deployer"`
    // TargetNamespace is the namespace the workload is deployed into
    // Defaults to the Deployment's own namespace.
    // +optional
    TargetNamespace string `json:"targetNamespace,omitempty"`
    // CreateNamespace creates TargetNamespace if it does not exist.
    // +kubebuilder:default:=true
    // +optional
    CreateNamespace bool `json:"createNamespace,omitempty"`
    // ReleaseName overrides the Helm release name (defaults to the Deployment
    // name). Ignored for kustomize.
    // +optional
    ReleaseName string `json:"releaseName,omitempty"`
    // ServiceAccountName is the ServiceAccount whose permissions the controller
    // uses to apply the generated deploy objects. Empty means the controller's
    // own identity (discouraged, matching the deprecated cluster-scoped Deployer).
    // +optional
    ServiceAccountName string `json:"serviceAccountName,omitempty"`
    // ConfigResources references OCM resources holding yaml used as values/config.
    // +optional
    ConfigResources []ResourceID `json:"configResources,omitempty"`
    // Config is inline, schemaless configuration merged over ConfigResources.
    // +kubebuilder:validation:Type=object
    // +kubebuilder:validation:XPreserveUnknownFields
    // +optional
    Config *apiextensionsv1.JSON `json:"config,omitempty"`
    // OCMConfig references secrets/config maps/ocm api objects for ocm config
    // and credentials, like every other CR.
    // +optional
    OCMConfig []OCMConfiguration `json:"ocmConfig,omitempty"`
    // Interval at which the Deployment is reconciled. Required because it sits
    // on top of the Repository/Component polling cadence.
    Interval metav1.Duration `json:"interval"`
    // +optional
    Suspend bool `json:"suspend,omitempty"`
}

type DeploymentStatus struct {
    ObservedGeneration int64                     `json:"observedGeneration,omitempty"`
    Conditions         []metav1.Condition        `json:"conditions,omitempty"`
    EffectiveOCMConfig []OCMConfiguration        `json:"effectiveOCMConfig,omitempty"`
    // Deployed lists references (apiVersion/kind/name/namespace/uid, not status)
    // to every object the controller created and owns: the child
    // Repository/Component/Resource objects and the deploy objects.
    // DeployedObjectReference is the existing type reused from the Deployer API.
    Deployed           []DeployedObjectReference `json:"deployed,omitempty"`
    // DeployedComponentVersion is the resolved component version currently live.
    DeployedComponentVersion string `json:"deployedComponentVersion,omitempty"`
    // DeployedResourceDigest is the digest of the deploy resource currently live.
    DeployedResourceDigest string `json:"deployedResourceDigest,omitempty"`
    // AppliedLocalizations records each image localization that was applied: the
    // original reference (match key) and the relocated reference substituted in.
    AppliedLocalizations []AppliedLocalization `json:"appliedLocalizations,omitempty"`
}

type AppliedLocalization struct {
    Original string `json:"original"`
    Applied  string `json:"applied"`
}
```

`ComponentVersionRef` is split by the controller into the repository (parsed into a typed repository
spec) and component name, which create the child `Repository` and `Component`, and a version segment
passed verbatim to `Component.semver`, so it may be an exact version or a range. `ConfigResources`
is `[]ResourceID` (OCM resources inside the component), distinct from `OCMConfig`
(`[]OCMConfiguration`), which carries ocm config and credentials. They are not merged. The component
reference may carry a scheme (for example `http://` for an insecure local registry); it becomes the
derived `Repository`'s `baseUrl`, and the controller sets the generated engine source insecure
accordingly. `ocmConfig` carries any registry credentials.

Status rolls the deploy object's own readiness up into the `Deployment`'s `Ready` condition (Flux
`HelmRelease`/`Kustomization` `Ready`, or Argo `Application` health plus sync), and records the live
component version and deploy-resource digest and the applied localizations, so the single `Deployment`
reflects the outcome without the user inspecting the children.

### Managed resources

From one `Deployment` the controller creates and owns:

* **`Repository` and `Component`**, built from the parsed `ComponentVersionRef`. The `Component`
  resolves the version against the repository once and tracks drift, so later reconciles reuse the
  cached resolution instead of re-fetching the descriptor.
* **A `Resource` for the deploy resource**. Its status provides the verified OCI coordinates used to
  build the `OCIRepository`/source. The `Deployment` controller then downloads the chart (as its OCI
  artifact) and reads its default `values.yaml`, or the kustomization manifests, for localization
  matching. Subchart/dependency images and aliased subchart values are out of scope for the initial
  design. The `Resource` verifies the digest; the download itself is done by the `Deployment`
  controller, because the `Resource` controller only resolves and verifies coordinates, it does not
  download content.
* **A `Resource` for each config resource**. The `Resource` verifies the digest; the `Deployment`
  controller downloads the blob content (expected to be a single yaml document) before merging it
  (Helm) or patching it (kustomize) into the deployer values. Locating a file inside an archived
  resource is out of scope.
* **A `Resource` for each localizable image**, for digest verification. We cannot guarantee the
  deploy ends up digest-pinned (it depends on whether the chart's image field accepts a digest), so
  the runtime cannot always enforce image integrity at pull time. The per-image `Resource` verifies,
  at resolve time, that the image at its current location still matches the signed digest (an OCI
  manifest-digest check, not a content re-hash), which is the only integrity check available on the
  tag-only path. It also yields the cached current coordinates used for substitution.
* **The deploy objects**, the Flux or Argo objects that perform the deployment (see the matrix
  below).

Creating these as first-class `Resource` objects, rather than resolving everything inline in the
`Deployment` controller, means resolution and verification happen once, are cached and drift-tracked
by the existing controllers, and are visible to the user for debugging. A controller instantiating
`Repository`/`Component`/`Resource` objects is a new pattern for this project; today those are
authored by the user or by KRO. Child objects are named after the `Deployment`, so several
Deployments of the same component version do not collide.

### Localization

Localization knowledge is split so the signed component never names a chart values path.

A signed label is attached to each localizable image resource at construction. Presence is the opt-in
marker; the value is the match key. The resource identity is implicit in which resource carries it.

```yaml
labels:
- name: ocm.software/localization
  version: v1
  signing: true
  value:
    type: ociImage
    original:
      registry: ghcr.io
      repository: stefanprodan/podinfo
      tag: 6.9.1
      digest: sha256:262578...   # present when add cv computed a digest
```

The `original` field exists because it is the only way to find the image in the deploy artifact. The
deploy artifact (chart values, kustomize manifests) still references the image by its *original*
coordinates. After transfer the resource `access` has been rewritten to the new location
(`transfer/internal/uploader.go`, `repositoryupload/upload.go`), and OCM deliberately excludes
`access` from the signature (`descriptor/normalisation/json/v4alpha1/normalisation.go`), so the
original coordinates survive nowhere else. The signed label is the only channel that preserves them
across transfer, which is why they are snapshotted into it. Without `original` there is no key to
match against the artifact.

At deploy time the controller reads the child `Resource` object's status to get the current resolved
location (registry, repository, tag, digest), then applies the match key against the deploy target.

* **Helm**: fetch the chart (OCI artifact) before deploy, load its default `values.yaml`, and locate
  image references deterministically (see Matching below). Where a value matches a label's `original`
  coordinates, replace it in place with the current location.
* **Kustomize**: use kustomize's native `images` transformer, a top-level `images:` list that
  rewrites every container image matching a given name across all rendered manifests. One entry
  (`name` = original reference, `newName`/`newTag`/`digest` = current location) localizes the image
  without targeting a manifest path, because kustomize matches by image name. The equivalent
  lower-level form is a JSON6902 patch on the container `image` field, as used in
  `examples/kustomize-configuration-localization/`; the transformer is preferred here precisely
  because it matches by name instead of a hardcoded path.

#### Matching (Helm)

Matching is exact, not best-effort. The controller walks the loaded values tree and recognizes two
value shapes:

1. a scalar string that parses as an OCI reference (`image: ghcr.io/org/app:1.2.3`), and
2. a map carrying the conventional image sub-keys (`repository`, or `image` as its alias, plus
   optional `registry`, `tag`, `digest`) that reconstruct into a reference.

The walk keys off the value shape, not the field name, so the holder field being called `image` or
anything else does not matter.

Both sides are normalized (default `docker.io` registry, implicit `library/` namespace, canonical
host form) and the reconstructed reference is compared to the label's `original` for exact equality on
registry+repository (plus tag/digest when present). On a match the value is rewritten in place: a
scalar becomes the full new reference; a map has its `repository`/`registry`/`tag` (and `digest` when
the shape carries one) set to the current location.

A label that matches nothing is skipped, not an error: construction labels every localizable image,
and a given deployment's chart references only some of them, so unused labels are expected. A single
label matching several values localizes all of them (the same image used in several places). The one
error is a genuine conflict: a single value matched by two labels that resolve to different locations.
(Charts that never expose an image as a literal value, for example one hardcoded in a template, are
out of scope for this design. A later mechanism could handle them, for example an optional
construction label naming the chart values path, but that recouples the signed component to the
chart's values structure, so it is deferred until a real chart requires it.)

Integrity comes from the digest, not the label. The resource `digest` is in the signed surface
(normalization excludes `access`/`srcRefs`/unsigned labels, not `digest`), and `ocm add cv` computes
it by default unless `--skip-reference-digest-processing` is set. The emitted override therefore
**pins the signed digest when the chart's image field accepts a digest, and substitutes
registry+repository+tag otherwise**. The label is the match key that survives transfer; the digest is
the tamper-evidence. Consequence of tag-only charts: Helm resolves the image by tag at pull time, so
there is a resolve-time-vs-pull-time gap and weaker integrity than a digest pin. This is inherent to
tag-only charts and is documented, not solved.

The labels are derived during `ocm add cv --localize-hints`, before transfer, so they capture the
original coordinates. The flag labels every resource with an OCI access type (not only those a
particular chart references, since the chart binding is a deploy-time concern) and copies each
resource's coordinates plus its digest into the signed label above. The digest is available because
`add cv` computes it by default; with `--skip-reference-digest-processing` the digest is absent and
the label carries coordinates only (localization then falls back to tag substitution). The labels may
also be authored directly in the component-constructor, for manual control or before the flag exists.

### Configuration and merge order

For Helm, configuration and localization are one deep-merge into the single `values`/`valuesObject`
field, in precedence order:

```
chart defaults            (applied by Helm)
  + localization overrides (beats chart defaults, its only job)
  + config resources       (explicit, beats localization)
  + inline config          (most specific, wins)
```

Merge rules:

* Maps deep-merge by key; later layers win on overlapping leaves.
* An image block (the subtree a localization override wrote) is treated **atomically**: config
  either leaves it untouched or replaces the whole block. A deliberate whole-block image override in
  config therefore wins (the escape hatch when localization matched wrong); a partial override
  (config sets only `tag` while localization set `repository`+`tag`+`digest`) is not applied, because
  it would produce a split, invalid reference. A whole-block override from inline `config` can replace
  a digest-pinned image and bypass the signed-digest integrity below; that is accepted as an explicit,
  operator-authored override, not a silent one.
* Arrays are replaced, not merged.

For kustomize there is no `values` field; configuration and localization are applied as structured
JSON6902 / strategic-merge patches against the rendered manifests, which Flux `Kustomization.patches`
and Argo `source.kustomize.patches` both support (see
`examples/kustomize-configuration-localization/`). Localization patches the container image (or uses
the `images` transformer); configuration patches manifest fields (for example adding an env var).
ConfigResources are applied as patches rather than a values merge, so the merge-order rules above
apply only to Helm; for kustomize the layering is expressed as ordered patches. First implementation
cut: kustomize does localization via the `images` transformer only; configuration via patches is
deferred.

### Deployer engine and artifact matrix

| Artifact \ Engine | FluxCD | ArgoCD |
| --- | --- | --- |
| Helm | `OCIRepository` + `HelmRelease.values` | `Application.source.helm.valuesObject` |
| Kustomize | `OCIRepository` + `Kustomization` (`images`, `patches`) | `Application.source.kustomize` (`images`, `patches`) |

Localization input is identical across all four; only the output field differs. Apply and prune of
the generated deploy objects reuse the `(Namespaced)Deployer` ApplySet machinery.

### Positive Consequences

* One applied `Deployment` instead of a hand-authored RGD plus bootstrap objects.
* The component author stays deployer-agnostic; Flux vs Argo is a `Deployment` field.
* Localization is automatic; integrity is anchored on the signed digest when the chart allows it.
* Configuration can be shipped with the component (as `configResources`) and layered per
  deployment via inline `config`.
* No new template language is introduced.

### Negative Consequences

* Helm localization depends on the chart exposing images as literal values in a recognized shape
  (scalar OCI reference, or a map with the conventional `registry`/`repository`/`tag`/`digest` keys).
  Matching is exact over those shapes: a label matching nothing is skipped, and only a value matched
  by two conflicting labels is an error. Images never present as a literal value (only in templates)
  are out of scope. Normalization handles `docker.io` vs `index.docker.io`, bare names, implicit
  `library/`, and a `repository` that embeds the registry. Kustomize avoids the search entirely via
  its native `images` transformer.
* Tag-only charts cannot be digest-pinned, giving weaker integrity (see Localization).
* OCI-only first cut: the component repository and the chart/image/resource access are assumed to be
  OCI (or CTF); non-OCI component repositories and non-OCI access (Helm HTTP repo, git source) are
  out of scope.

## Links

* Refines epic [ocm-project#1339](https://github.com/open-component-model/ocm-project/issues/1339)
* Builds on the namespaced deployer: [open-component-model#3779](https://github.com/open-component-model/open-component-model/pull/3779)
