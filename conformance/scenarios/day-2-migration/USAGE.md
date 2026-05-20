# Day-2 Migration Scenario - Usage Guide

## Quick Start

```bash
task run
```

This runs the full pipeline end-to-end: creates a kind cluster, builds and signs
components, transfers through an air-gap, deploys, and verifies the initial schema.

## Prerequisites

The `check` task verifies these are available:

- `docker` - Container runtime
- `kubectl` - Kubernetes CLI
- `kind` - Kubernetes in Docker
- `helm` - Helm package manager (for installing OCM toolkit and kro)
- `openssl` - Key generation for signing
- `jq` - JSON processing

Run `task check` to verify all prerequisites.

## Configuration Options

| Variable | Default | Description |
|----------|---------|-------------|
| `VERSION` | `1.0.0` | Component version to build |
| `CLI_IMAGE` | `ghcr.io/open-component-model/cli:0.0.0-main` | OCM CLI container image |
| `TOOLKIT_IMAGE` | `oci://ghcr.io/.../chart:0.0.0-main` | OCM K8s toolkit Helm chart |
| `PRELOAD_IMAGES` | (empty) | Space-separated image archives to preload |
| `DOCKER_NETWORK` | `kind` | Docker network for OCM CLI container |
| `KRO_VERSION` | `0.9.0` | kro Helm chart version |
| `KIND_NODE_IMAGE_VERSION` | `v1.34.0` | Kind node image version |

## End-to-End Flow

### 1. `check`

Verifies all prerequisites are installed.

### 2. `clean`

Removes temporary files, kind cluster, and local registry container.

### 3. `prepare`

Creates the `tmp/` working directory and extracts Docker credentials
for the containerized OCM CLI.

### 4. `cluster:setup`

Sets up the infrastructure:

- `cluster:registry` - Starts local Docker registry on 127.0.0.1:5001
- `cluster:create` - Creates kind cluster with containerd registry config
- `cluster:registry:configure` - Configures cluster nodes to reach registry
- `cluster:preload` - Pre-loads any provided image archives
- `cluster:install:controllers` - Installs OCM toolkit and kro (no FluxCD needed)

### 5. `build:product`

Builds the complete OCM component tree:

- `build:migrator` - Docker builds the alpine+sqlite3 migrator image, packages as OCI layout
- Adds `acme.org/day2/db-migrator` component to CTF archive
- Adds `acme.org/day2/product` meta-component (references db-migrator, includes RGD)
- `product:keys` - Generates RSA-4096 key pair
- `sign` - Signs the product component

### 6. `transfer:airgap`

Verifies the component signature, then transfers to an air-gap archive
with all resources copied locally (no external registry access needed).

### 7. `cluster:import`

Transfers the air-gap archive into the cluster's local registry at
`http://registry:5000`.

### 8. `cluster:bootstrap`

Applies Kubernetes manifests in order:

1. RBAC (kro.run permissions for OCM controller)
2. Namespace (`day2-migration`)
3. Secret (public signing key for verification)
4. Bootstrap CRs (Repository -> Component -> Resource -> Deployer)
5. Waits for Deployer and RGD to be ready
6. Applies `sample-product-1.0.0.yaml` (creates the Day2MigrationProduct CR)
7. Waits for migration Job `day2-migration-1-0-0` to complete

### 9. `verify:deployment`

Confirms v1.0.0 migration succeeded:

- Checks Job logs for `001_create_items_table.sql` application
- Runs a temporary pod to query the SQLite schema and confirm `items` table exists

## Upgrade Workflow

### What changes between v1.0.0 and v1.1.0

| Aspect | v1.0.0 | v1.1.0 |
|--------|--------|--------|
| Migrations included | `001_create_items_table.sql` | `001_create_items_table.sql` + `002_add_category_column.sql` |
| Schema result | `items(id, name, created_at)` | `items(id, name, created_at, category)` |
| Job name | `day2-migration-1-0-0` | `day2-migration-1-1-0` |

### Running the upgrade

```bash
task upgrade
```

This:

1. Clears existing CTF archives
2. Rebuilds components at v1.1.0 (migrator image now includes both .sql files)
3. Signs the new version
4. Transfers through air-gap
5. Imports to cluster registry
6. Applies `sample-product-1.1.0.yaml` (only `spec.version` changes)
7. Waits for new migration Job to complete
8. Runs `verify:upgrade` to confirm schema change

### What happens during upgrade

1. OCM Controller detects version change in Component CR
2. Resource CRs resolve updated migrator image (new digest)
3. kro re-evaluates RGD: Job name changes from `day2-migration-1-0-0` to `day2-migration-1-1-0`
4. Old Job is pruned by ApplySet, new Job is created
5. Migrator container executes:
   - Skips `001_create_items_table.sql` (already in `schema_migrations`)
   - Applies `002_add_category_column.sql` (ALTER TABLE)
6. Job reports success, Day2MigrationProduct CR becomes Ready

## Step-by-Step Testing

Run stages independently for debugging:

```bash
task prepare
task cluster:setup
task build:product
task transfer:airgap
task cluster:import
task cluster:bootstrap
task verify:deployment
task upgrade
task verify:upgrade
```

## Useful Commands

```bash
# Show all OCM resources
task status

# Check migration Job logs
kubectl logs job/day2-migration-1-0-0 -n day2-migration
kubectl logs job/day2-migration-1-1-0 -n day2-migration

# Inspect the Day2MigrationProduct instance
kubectl get day2migrationproduct -n day2-migration -oyaml

# Check RGD status
kubectl get resourcegraphdefinition day2-migration-product -oyaml

# Query SQLite directly (after migration completes)
kubectl run sqlite-shell --rm -it --restart=Never \
  -n day2-migration --image=alpine:3.23 \
  --overrides='{"spec":{"containers":[{"name":"sh","image":"alpine:3.23","command":["sh"],"stdin":true,"tty":true,"volumeMounts":[{"name":"data","mountPath":"/data"}]}],"volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"day2-migration-sqlite-data"}}]}}'
# Then: apk add sqlite && sqlite3 /data/app.db
```

## Cleanup

```bash
task clean
```

Removes the kind cluster, local registry, and all temporary files.

## Troubleshooting

**Job stuck in pending:**
```bash
kubectl describe job/day2-migration-1-0-0 -n day2-migration
kubectl get events -n day2-migration --sort-by=.lastTimestamp
```

**PVC not bound:**
```bash
kubectl get pvc -n day2-migration
kubectl get storageclass
```

**Component not resolving:**
```bash
kubectl get component -n day2-migration -oyaml
kubectl logs -n ocm-k8s-toolkit-system deploy/ocm-k8s-toolkit-controller-manager
```

**RGD not creating resources:**
```bash
kubectl get rgd day2-migration-product -oyaml
kubectl logs -n kro deploy/kro-controller-manager
```
