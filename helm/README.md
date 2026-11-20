# AI48Home Helm deployment

This chart follows the topology in `install.sh`: optional internal PostgreSQL, one AI48Home instance, and a configurable AI48Proxy worker pool. Workers run as a StatefulSet with optional HPA and automatic JWT enrollment. There is **no Nginx or headless Service**. The application Services expose Home on `8327` and workers on `8317`; enabling internal PostgreSQL adds its database Service.

## Requirements

- Helm 3+, Kubernetes, and a default StorageClass or configured `global.storageClass` / component `persistence.storageClass`.
- For HPA: Metrics Server and Kubernetes 1.30+ for stable container resource metrics.
- Images accessible to the cluster. Configure `imagePullSecrets` for private registries.
- PostgreSQL credentials. The management key and gateway API key are optional; the chart generates them when unset.

## Install

Start with [`values.yaml.example`](values.yaml.example), which documents the values supported by this chart. Copy it to a private file outside version control, restrict its permissions, and replace the database host, username, database, and password with your actual PostgreSQL connection settings. The example uses external PostgreSQL; set `postgresql.enabled: true` to deploy the internal database instead. Adjust all `persistence.storageClass` values to match your cluster.

From the repository root:

```bash
cp helm/values.yaml.example /path/to/private-values.yaml
chmod 600 /path/to/private-values.yaml
```

The following is only a secrets override, not a complete external database configuration:

```yaml
secrets:
  managementKey: ""   # optional; generated when empty
  gatewayApiKey: ""   # optional; generated when empty
postgresql:
  auth:
    password: "REPLACE_WITH_RANDOM_DATABASE_PASSWORD"
```

Generate supplied values independently, for example with `openssl rand -hex 32`.

### Management and gateway keys

When `secrets.managementKey` or `secrets.gatewayApiKey` is empty, the chart generates a random 48-character value on first install and stores it in the `<fullname>-config` Secret. Upgrades reuse the stored value through Helm `lookup`, so the keys do not rotate on every `helm upgrade`. An explicit value always takes precedence.

Read the keys from the Secret (with the default `fullnameOverride: ai48home`; replace the namespace with yours):

```bash
# Management key: sign in at /admin/connect and call the Management API
kubectl -n ai48home get secret ai48home-config \
  -o jsonpath='{.data.management-key}' | base64 -d; echo

# Gateway API key: client access to the worker API on port 8317
kubectl -n ai48home get secret ai48home-config \
  -o jsonpath='{.data.gateway-api-key}' | base64 -d; echo
```

The same commands are printed after `helm install`/`helm upgrade`, and can be shown again with `helm get notes ai48home -n ai48home`. They print secrets to the terminal; do not paste their output into logs or chat.

Caveats:

- `helm template` and `helm install --dry-run` cannot read cluster Secrets, so they render a new random value each time. Only real install/upgrade operations preserve the stored key.
- Key generation stores the value only in the Secret and Helm release metadata. If you delete the `<fullname>-config` Secret, the next upgrade generates a new key that will not match the key already imported into the database. Back up the Secret, or set explicit values, for production.
- GitOps tools that render with `helm template` (for example Argo CD) do not support `lookup` and would rotate the key on every sync. Set explicit values there.

### Install the released chart

Every `v*` release publishes the chart to GHCR as an OCI artifact and attaches the archive to the GitHub release. The chart version is the release tag without `v`, and its default Home image is pinned to the same release tag:

| Reference | Source |
| --- | --- |
| `oci://ghcr.io/sofyan48/charts/ai48home --version <version>` | A specific release, e.g. `1.2.0` for tag `v1.2.0` |
| `oci://ghcr.io/sofyan48/charts/ai48home:latest` | Latest `v*` release |
| `ai48home-<version>.tgz`, `ai48home-latest.tgz` | GitHub release assets |

```bash
helm upgrade --install ai48home oci://ghcr.io/sofyan48/charts/ai48home \
  --version 1.2.0 \
  --namespace ai48home --create-namespace \
  -f /path/to/private-values.yaml --wait --timeout 10m
```

Pin `--version` for production. To follow the newest release, pull the `latest` tag explicitly, because Helm resolves `--version` only against semantic versions:

```bash
helm pull oci://ghcr.io/sofyan48/charts/ai48home:latest
```

After the first publication, set the `charts/ai48home` package to **Public** under *Packages → Package settings* if clusters pull it without logging in.

### Install from source

From the repository root, verify the target cluster and install the source chart:

```bash
kubectl config current-context
chmod 600 /path/to/private-values.yaml
```

```bash
helm upgrade --install ai48home ./helm/ai48home \
  --namespace ai48home --create-namespace \
  -f /path/to/private-values.yaml --wait --timeout 10m
```

For Maxcloud, run `kube_maxcloud` first in your interactive shell. From the `helm/` directory, package and install the chart into namespace `dev`:

```bash
kube_maxcloud
helm lint ./ai48home --strict -f values.yml
helm package ./ai48home --destination .
helm upgrade --install ai48home ai48home-0.1.1.tgz \
  -n dev --create-namespace -f values.yml --wait --timeout 10m
```

`values.yml` is deployment-specific and may contain credentials. Do not commit or share it; use your own private values file for other environments. Repackage the chart after template changes: Helm does not read source templates when installing a `.tgz` archive.

Pin `home.image.tag` to a tested version/digest-compatible tag for production; its default follows the unpinned Home image in `install.sh`. The Home templates expect the AI48Home image layout (`/AI48Home/AI48Home`), as built by this repository's `Dockerfile`.

## Migrating from the `cliproxy` chart

The chart was renamed from `cliproxyapihome` (release `cliproxy`, `fullnameOverride: cliproxy`) to `ai48home`. This changes resource names, PVC names, and the `app.kubernetes.io/name` selector label, which is immutable on Deployments and StatefulSets. Do not `helm upgrade` an existing `cliproxy` release with this chart in place.

Install `ai48home` as a new release that points at the same external database, and copy any retained worker identity data if needed. Then verify it and uninstall the old `cliproxy` release. Back up PostgreSQL and PVCs first. Alternatively, keep an existing release on the old names by setting `fullnameOverride: cliproxy` and `nameOverride: cliproxyapihome` in its private values.

## Optional PostgreSQL

`postgresql.enabled: true` creates an internal PostgreSQL StatefulSet, Service, and persistent volume. Set it to `false` to use an external database:

```yaml
postgresql:
  enabled: false
  auth:
    username: ai48home
    database: ai48home
    password: "YOUR_EXTERNAL_DATABASE_PASSWORD"
  external:
    host: postgres.example.com
    port: 5432
    sslmode: require
```

The external database must already exist, and the user must have permissions to create and migrate tables. Pods must be able to reach its host and port. Disabling PostgreSQL does **not** migrate internal data. Back up and migrate the database before switching.

The chart derives PostgreSQL environment variables and Home's `cluster.yaml` from `postgresql.auth` and `postgresql.external`. Do not set connection details only through `home.environment`: Home reads concrete database settings from `cluster.yaml`. The password is stored in a Secret, not the environment ConfigMap. `sslmode: require` encrypts the connection but does not verify server identity like `verify-full`.

## Services and worker routing

With release name `ai48home` and the default `fullnameOverride: ai48home`:

| Service | Port | Purpose |
| --- | --- | --- |
| `ai48home-home` | 8327 | Home Management HTTP and RESP/mTLS |
| `ai48home-worker` | 8317 | Worker API traffic |

Without `fullnameOverride`, names are prefixed with `<release>-ai48home`, for example `ai48home-ai48home-home`.

The chart defaults to one Home replica and two AI48Proxy worker replicas. Each worker has its own PVC and enrollment identity; the regular worker Service balances HTTP connections across Ready workers. The worker image reads its persisted JWT from its own identity PVC. There is no headless Service or per-pod DNS discovery. StatefulSet names and per-worker PVC identities remain stable.

When upgrading from the chart with a headless Service, `serviceName` changes and Kubernetes requires recreating the StatefulSet. Preserve pods and PVCs using `kubectl -n dev delete statefulset ai48home-workers --cascade=orphan`, then run Helm upgrade. Adjust the namespace and StatefulSet name for your release. Helm removes the obsolete headless Service.

```yaml
home:
  service:
    type: ClusterIP
workers:
  service:
    type: LoadBalancer
```

Both support `ClusterIP`, `NodePort`, or `LoadBalancer`. Keep Home private unless deliberately exposing it. Home JWTs advertise the in-cluster Home DNS name; workers outside the cluster are not supported by these defaults. Use a TCP-capable endpoint for RESP/mTLS, not an HTTP-only Ingress.

For local access:

```bash
kubectl -n ai48home port-forward service/ai48home-home 8327:8327
kubectl -n ai48home port-forward service/ai48home-worker 8317:8317
```

Kubernetes Service balancing is connection-based, not Nginx `least_conn`. EndpointSlices track Ready workers as HPA changes replicas. New connections can reach available workers; existing keep-alive/streaming connections stay with their selected worker and cannot migrate when that pod terminates.

Local forwarding is for debugging only. Even `kubectl port-forward service/...` selects one backing pod and may stop when that pod is replaced or removed. Opening a Service through a desktop tool may also use local forwarding; inspect the actual transport rather than assuming a localhost URL uses ClusterIP balancing. For production, access the Service from inside the cluster or through a configured Ingress/LoadBalancer. The chart can create an optional HTTP `Ingress` or Gateway API `HTTPRoute`; both are disabled by default. A `LoadBalancer` Service still requires a load-balancer implementation in the cluster. The request path is Client → Ingress/Gateway/Service → AI48Proxy workers → upstream provider; workers use RESP/mTLS to reach the single AI48Home control plane, which persists state in PostgreSQL or SQLite.

## Ingress and Gateway API routing

The chart supports two optional HTTP routing resources. Both route client API traffic to the worker Service and can route Home management/UI traffic separately. Home remains private by default.

**Ingress:**

```yaml
ingress:
  enabled: true
  className: nginx
  hosts:
    - host: ai48.example.com
      home:
        path: /home
        pathType: Prefix
      worker:
        path: /
        pathType: Prefix
  tls:
    - secretName: ai48-tls
      hosts: [ai48.example.com]
```

The `/home` path is sent to `ai48home-home`; `/` is sent to `ai48home-worker`. Use a separate hostname or stronger authentication before exposing the Home path publicly. HTTP Ingress does not carry Home RESP/mTLS traffic; workers must reach the in-cluster Home Service for enrollment and RESP.

**Gateway API:**

```yaml
gateway:
  enabled: true
  parentRefs:
    - name: shared-gateway
      namespace: gateway-system
  hostnames:
    - api.example.com
  home:
    path: /home
  worker:
    path: /
```

This requires Gateway API CRDs and an existing Gateway listener accepting the hostname. The HTTPRoute backend ports are the Services' HTTP port 80; Home's RESP/mTLS port 8327 remains internal.

## Environment ConfigMaps and Secrets

The chart creates `<fullname>-home-env`, `<fullname>-worker-env`, and `<fullname>-postgresql-env`, consumed using `envFrom`. They hold timezone, database connection settings (excluding password), Home URL, worker startup delay, and extra non-sensitive environment values:

```yaml
timezone: Asia/Jakarta
home:
  environment:
    HOME_USER_EMAIL_SMTP_PASSWORD: ""
workers:
  environment:
    EXAMPLE_NON_SECRET_SETTING: "value"
postgresql:
  environment: {}
```

Do not put tokens, passwords, or API keys in `environment`. Required worker/database connection variables are chart-managed. Only use environment variables supported by the selected application image. Home does **not** expand environment variables inside `cluster.yaml`; the chart renders concrete connection settings into a Secret.

`<fullname>-config` stores the PostgreSQL password, management key, gateway API key, and secret-bearing `config.yaml` / `cluster.yaml`. Kubernetes Secrets are not automatically encrypted at rest; enable cluster encryption and restrict RBAC. Helm release metadata also contains supplied values: protect Helm storage access. Environment changes trigger pod rollouts via checksums.

## Bootstrap and persistence

- Home checks the database before startup and imports `config.yaml` only if `public.config` is absent or empty. Startup fails on database errors rather than blindly importing.
- The normal Home container mounts only `cluster.yaml`; runtime configuration remains DB-backed. After first import, changing the Helm management/API keys (supplied or generated) does not update existing DB config. Make changes through the Management API and keep chart values synchronized, particularly the management key used for worker enrollment.
- Each worker has its own PVC and creates a unique Home JWT through the Management API. The JWT and enrollment identity survive restarts. Startup is staggered by 25 seconds plus 2 seconds per ordinal to allow old membership to expire. Increase the delay if you increase heartbeat timeout.
- Home uses one replica with `Recreate`, avoiding overlapping instances with the same advertised identity. Do not point multiple chart releases at the same empty database during bootstrap.
- Internal PostgreSQL uses persistent storage. Changing `postgresql.auth.password` does not rotate an existing database password; rotate the database separately and synchronize values.
- Worker scaling is controlled by `workers.replicas` when autoscaling is disabled, or by HPA when enabled. Do not share worker PVCs or certificates. StatefulSet PVCs remain on scale-down/uninstall by default; back them up and delete explicitly only when intended. The standalone Home PVC is deleted by Helm uninstall, subject to the PV reclaim policy; back up first.

## StorageClass and Pending PVCs

Home and workers still need storage when `postgresql.enabled: false`. Maxcloud provides `nfs-client` without a default StorageClass, so configure:

```yaml
global:
  storageClass: nfs-client
```

A component's `persistence.storageClass` overrides this fallback. An explicit empty string opts out of dynamic provisioning and requires a manually provisioned PV; omitting both settings uses the cluster default.

```bash
kubectl get storageclass
kubectl -n dev get pvc
kubectl -n dev describe pvc identity-ai48home-workers-0
```

Changing the StatefulSet template does not update existing PVCs. If a PVC is Pending and `storageClassName` is unset (not explicitly empty), Kubernetes allows assigning a class:

```bash
kubectl -n dev patch pvc identity-ai48home-workers-0 \
  --type=merge -p '{"spec":{"storageClassName":"nfs-client"}}'
```

Inspect each PVC before patching; do not change or delete a Bound PVC to resolve scheduling. StatefulSet `volumeClaimTemplates` and `serviceName` are immutable. If an intentional upgrade changes these fields, recreate only the StatefulSet with `--cascade=orphan`, then upgrade Helm. This preserves pods and PVCs, but does not migrate or modify existing volumes.

## Worker autoscaling

Enable HPA for the worker StatefulSet (requires Metrics Server and Kubernetes 1.30+ for stable container resource metrics):

```yaml
workers:
  replicas: 2
  autoscaling:
    enabled: true
    minReplicas: 2
    maxReplicas: 10
    targetCPUUtilizationPercentage: 70
    scaleDownStabilizationSeconds: 300
  resources:
    requests:
      cpu: 250m
      memory: 256Mi
```

HPA targets the StatefulSet through its scale subresource; Deployment is not required. StatefulSet preserves per-ordinal PVC/JWT identities, HPA manages the replica count, and the worker Service manages routing to Ready endpoints. Manual `HOME_JWT` generation is not required.

HPA measures the `worker` container CPU against its request, ignoring bootstrap containers. With a 250m request, the 70% target is approximately 175m average CPU per worker. These resource requests are starting values, not measured capacity recommendations.

CPU-based scaling can underrepresent I/O-bound proxy traffic: low CPU does not mean there are no active requests or streams. Start with at least 2–3 minimum workers for redundancy and tune using realistic traffic. Active-request/concurrency custom metrics may be more suitable, but this chart does not currently implement them. HPA creates pods, not cluster nodes; scaling still requires node capacity and working storage provisioning.

When enabled, Helm omits StatefulSet `spec.replicas` so subsequent upgrades do not reset HPA scaling. Switching from Helm-managed replicas may temporarily reset replicas during the first upgrade before HPA reconciles. New ordinals automatically provision a PVC and enroll a unique JWT, without manual HOME_JWT generation. Retained PVCs reuse their identities on scale-up. Storage must support dynamic provisioning through a default or configured StorageClass.

Scale-up is limited to two pods per minute. Scale-down uses a five-minute stabilization window and removes at most one pod per minute. These policies do not guarantee completion of long-running streams: the current pod termination grace period is 30 seconds. HPA applies to the shared worker pool, not per-user isolation.

```bash
kubectl -n dev get hpa ai48home-workers
kubectl -n dev describe hpa ai48home-workers
kubectl -n dev top pod -l app.kubernetes.io/instance=ai48home --containers
kubectl -n dev get endpointslice -l kubernetes.io/service-name=ai48home-worker
```

During bootstrap or rolling updates, `FailedGetContainerResourceMetric` / `pods might be unready` can occur until workers are Ready and CPU samples are available. Check current HPA conditions rather than old events: `ScalingActive=True` with `ValidMetricFound` and a numeric CPU target means metrics are being read. `ScalingLimited=True` with `TooFewReplicas` means the minimum replica floor is preventing further scale-down, not a metrics failure.

For `socket hang up`, correlate the failing request with pod termination, rollout, and HPA events. Check the client-to-Service path, Ready endpoints, and worker logs. A successful HTTP 401 without a key proves HTTP connectivity only; it does not validate authenticated inference or streaming. Neither a healthy HPA nor a Ready TCP probe guarantees that an in-flight stream survives scale-down. CPU HPA is a capacity mechanism, not a zero-disconnect guarantee.

## Validation

```bash
helm lint ./helm/ai48home --strict -f /path/to/private-values.yaml
helm template ai48home ./helm/ai48home \
  --namespace ai48home -f /path/to/private-values.yaml
```

Rendered manifests contain secrets. Do not publish them or commit them to source control.
