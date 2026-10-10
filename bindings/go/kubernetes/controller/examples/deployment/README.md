# deployment

Deploys podinfo from an OCM component by applying a single `Deployment` resource.
The `Deployment` controller creates the `Repository`, `Component`, and `Resource`
objects and the Flux or Argo deploy objects, localizes the container images, and
merges configuration into the deployer values. See
[ADR 0032](../../../../../docs/adr/0032_deployer_agnostic_deployment.md) for the
design.

## What it deploys

podinfo, in all four combinations:

| File | Deployer | Artifact |
|---|---|---|
| `deployment-flux-helm.yaml` | FluxCD | Helm chart (`podinfo-chart`) |
| `deployment-argo-helm.yaml` | ArgoCD | Helm chart (`podinfo-chart`) |
| `deployment-flux-kustomize.yaml` | FluxCD | kustomize (`podinfo-kustomize`) |
| `deployment-argo-kustomize.yaml` | ArgoCD | kustomize (`podinfo-kustomize`) |

## Localization

`ocm add cv --localize-hints` attaches a signed `ocm.software/localization`
label to each image resource, carrying the image's original coordinates. At
deploy time the controller resolves each resource's current (relocated) location
and substitutes it wherever the original coordinates appear:

- Helm: by matching the chart's rendered `values.yaml` image fields.
- Kustomize: via the native `images` transformer (match by name).

## Configuration

Config is merged into the deployer values, low to high precedence:

```
chart defaults  <  localization  <  central-config  <  local-config  <  inline config
```

- `central-config` is delivered by a **separate component**, pulled in through a
  component reference and addressed by `referencePath`. It sets `ui.message`
  (which survives and shows in the pod) and `ui.color: #111111`.
- `local-config` (in this component) sets `replicaCount: 2`, `redis.enabled: true`
  and overrides the color to `#222222`.
- the inline `config` on the Helm Deployments overrides the color to `#326ce5`.

Rendered result: `ui.message` from central, `ui.color` `#326ce5` (inline beats
local beats central), redis enabled. The kustomize variants are localization-only;
kustomize has no values field, so configuration there is expressed as patches.

## Behaviors shown

- **split field, registry in `repository`** - podinfo `image.repository:
  ghcr.io/stefanprodan/podinfo` + `image.tag`.
- **whole-string image** - the kustomize manifest's `image:
  ghcr.io/stefanprodan/podinfo:6.9.1`.
- **bare-name normalization** - redis `repository: redis` →
  `docker.io/library/redis`, a second image at a different value path, rendered
  because `local-config` enables redis.
- **labelled but unreferenced** - `unused-image` (busybox) is labelled but no
  artifact references it; localization skips it (it is absent from
  `status.appliedLocalizations`) and the deploy still succeeds.
- **config precedence** - the color chain proves inline > local > central, and the
  central `ui.message` survives.
- **tag-only images** - podinfo's image maps expose only `repository`/`tag` (no
  `digest` field), so substitution is tag-based and integrity is weaker than a
  digest pin (see ADR 0032). A chart that exposes a `digest:` field can be
  digest-pinned instead.

## How the kustomize artifact is built

`podinfo-kustomize` uses a `dir/v1` input pointing at `./oci-layout` with media
type `application/vnd.ocm.software.oci.layout.v1+tar`. `./oci-layout` is not
checked in. The e2e harness builds it from `./kustomize/` automatically (it runs
`buildKustomizeOCILayout` whenever a `kustomize/` dir is present); to build it by
hand (local runs) use `oras` before `ocm add cv`:

```bash
tar -czf kustomize.tar.gz -C ./kustomize .
oras push --oci-layout ./oci-layout:latest \
  --artifact-type application/vnd.cncf.flux.config.v1+json \
  kustomize.tar.gz:application/vnd.cncf.flux.content.v1.tar+gzip
```

## Flow

1. `ocm add cv --localize-hints` builds the component and attaches the
   localization labels.
2. `ocm transfer cv … <registry> --recursive --copy-resources` relocates both components (the top
   one and `central-config`) and copies the external images into the target registry; with the OCI
   uploader it also converts the kustomize layout to a native OCI artifact. Localization resolves to
   these relocated references.
3. Apply `serviceaccount.yaml` (the deploy-time ServiceAccount named by each Deployment's
   `serviceAccountName`), then apply one `deployment-*.yaml`. The `http://` scheme in
   `componentVersionRef` selects the insecure local registry (it becomes the derived `Repository`'s
   `baseUrl`).

## Editing the kustomize manifests

Edit `./kustomize/`, then rebuild `./oci-layout/`. Do not commit `./oci-layout/`.
