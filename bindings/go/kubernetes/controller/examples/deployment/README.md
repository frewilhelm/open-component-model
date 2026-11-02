# OCMDeployment Example — four-variant deployment showcase

Demonstrates OCMDeployment (ADR 0032) with all four engine combinations:
FluxCD × Helm, FluxCD × Kustomize, Argo CD × Helm, Argo CD × Kustomize.
Each variant deploys [podinfo](https://github.com/stefanprodan/podinfo) from an
OCI registry that the e2e harness provisions, with image references rewritten by
signed localization labels.

## Variant matrix

| Variant | Engine | Deploy resource | Namespace | Config |
|---|---|---|---|---|
| `flux-helm/` | FluxCD | `podinfo-chart` (Helm) | `deployment-flux-helm` | central + local + inline |
| `argo-helm/` | Argo CD | `podinfo-chart` (Helm) | `deployment-argo-helm` | central + local + inline |
| `flux-kustomize/` | FluxCD | `podinfo-kustomize` (OCI layout) | `deployment-flux-kustomize` | central + local + inline (patches) |
| `argo-kustomize/` | Argo CD | `podinfo-kustomize` (OCI layout) | `deployment-argo-kustomize` | central + local + inline (patches) |

## Component structure

```
ocm.software/ocm-k8s-toolkit/examples/deployment @ 1.0.0
├── resources
│   ├── podinfo-chart        (helmChart, ociArtifact access)
│   ├── podinfo-kustomize    (ociArtifact, built from kustomize/ by harness)
│   ├── podinfo-image        (ociImage — carries localization label via --localize-hints)
│   ├── redis-image          (ociImage — localization label, referenced when redis.enabled=true)
│   ├── unused-image         (ociImage — labelled but not referenced in chart values; skipped)
│   ├── local-config         (yaml — replicaCount:2, redis.enabled:true, ui.color override — Helm)
│   └── local-patches        (yaml — {patches:[replicas=2]} — kustomize)
└── componentReferences
    └── central-config → ocm.software/.../deployment/central-config @ 1.0.0
        └── resources
            ├── central-config   (yaml — ui.message, ui.color — Helm)
            └── central-patches  (yaml — {patches:[add PODINFO_UI_MESSAGE env]} — kustomize)
```

## Localization behaviors demonstrated

| Resource | Chart value shape | Behavior |
|---|---|---|
| `podinfo-image` | `image: {repository: "ghcr.io/stefanprodan/podinfo", tag: "6.9.1"}` (map) | Registry+repo+tag substituted; `digestPinned: false` (tag-only label), warning Event emitted |
| `redis-image` | `redis: {image: {repository: "docker.io/library/redis"}}` (map) | Matched after `docker.io` normalization + `library/` prefix inference; substituted |
| `unused-image` | No match in `values.yaml` | Label present, no substitution; absent from `appliedLocalizations` |

## Config precedence chain

```
chart defaults < localization overrides < central-config < local-config < inline config
```

The Helm variants merge (lowest to highest):

1. `chart defaults` — podinfo's bundled `values.yaml`
2. `localization` — image references rewritten to the relocated registry
3. `central-config` — `ui.message: "Hello from the central config component"`, `ui.color: "#111111"`
4. `local-config` — `replicaCount: 2`, `redis.enabled: true`, `ui.color: "#222222"`
5. `inline config` (spec.config) — `ui.color: "#326ce5"` (final winner)

The kustomize variants localize via the native `images:` transformer, and apply `config`/
`configResources` as pass-through `patches` (Flux `Kustomization.spec.patches`, Argo
`Application.source.kustomize.patches`). Patch ordering mirrors Helm's layering — `configResources`
in spec order, then inline `config` last:

1. `localization` — `images:` transformer rewrites the podinfo image to the relocated registry
2. `central-patches` — adds the `PODINFO_UI_MESSAGE` env var to the podinfo container
3. `local-patches` — scales the Deployment to `replicas: 2`
4. `inline config` (spec.config) — adds the pod-template label `config.ocm.software/applied: inline`

## Argo CD prerequisite: apps-in-any-namespace

The Argo variants place the `Application` in the `default` namespace (same as the OCMDeployment).
This requires:

- `argocd-cmd-params-cm` contains `application.namespaces: "*"`
- The `default` AppProject has `sourceNamespaces: ["*"]`

The e2e `hacks/setup.sh` `install_argocd` function sets both; see the setup script for details.

## Building and transferring (manual)

```bash
# Build the kustomize OCI layout (normally done by the e2e harness)
ocm add cv --constructor component-constructor.yaml \
  --repository ./transport-archive \
  --localize-hints

# Transfer to the e2e registry
ocm transfer cv ./transport-archive \
  --recursive --copy-resources \
  http://ocm-e2e-image-registry:5000
```

## Running via the e2e suite

```bash
task bindings/go/kubernetes/controller:test/e2e -- --focus=OCMDeployment
```
