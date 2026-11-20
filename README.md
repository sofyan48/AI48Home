# AI48Home

English | [中文](README_CN.md)

> AI48Home is based on [router-for-me/CLIProxyAPIHome](https://github.com/router-for-me/CLIProxyAPIHome) and its panel [Home Management Center](https://github.com/router-for-me/Home-Management-Center). Both are MIT-licensed; the original copyright notice is kept in [LICENSE](LICENSE).

AI48Home is a **centralized control plane for clusters of AI gateways** powered by [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). It brings multiple CLIProxyAPI gateway nodes under one management system, so operators can control their deployment centrally and scale request processing across multiple nodes and clusters.

Instead of managing each gateway independently, AI48Home centralizes configuration, API credentials, credential scheduling and distribution, and usage reporting. CLIProxyAPI nodes connect to AI48Home to retrieve configuration and dispatchable credentials, then report usage and request logs back to Home.

### How it supports distributed traffic

- **Centralized control**: Manage gateway configuration, provider accounts, credentials, and usage from one control plane and Web interface.
- **Distributed request processing**: Run multiple CLIProxyAPI gateways across servers or clusters to share the AI request workload rather than relying on a single gateway.
- **Coordinated credential scheduling**: Home schedules and distributes credentials to connected gateways, keeping credential selection centrally managed as the deployment grows.
- **Shared control-plane state**: Use PostgreSQL when running multiple active Home instances; a single Home instance can use SQLite and still manage multiple CLIProxyAPI gateways.

AI48Home manages the control plane; CLIProxyAPI gateways handle AI API requests. Use an ingress, reverse proxy, or load balancer in front of the gateways to distribute incoming client traffic across nodes or clusters. Home's credential scheduling complements this traffic routing rather than replacing it.

## AI48Home and AI48HomeUI

AI48Home provides the backend service, and [AI48HomeUI](https://github.com/sofyan48/AI48HomeUI) (the AI48Home panel) provides the Web interface. **The two projects are maintained separately. AI48Home releases and images embed AI48HomeUI so they can be deployed and updated together.**

### Deploying an official release

AI48Home binaries and Docker images include the Web interface, so AI48HomeUI does not need to be deployed separately. After starting Home, open its address, such as `http://localhost:8327`. The administrator console is at `/admin`, and the user workspace is at `/app`.

Administrator access requires a management key; see the [authentication instructions](docs/management/api.md#authentication). Database setup and configuration import are covered under [Cluster Mode](#cluster-mode). Update the Home binary or Docker image to update the bundled Web interface.

### Install from GHCR

AI48Home publishes multi-arch (`linux/amd64`, `linux/arm64`) images with the [AI48HomeUI](https://github.com/sofyan48/AI48HomeUI) panel embedded:

| Tag | Source |
| --- | --- |
| `ghcr.io/sofyan48/ai48home:latest` | Latest `v*` release tag |
| `ghcr.io/sofyan48/ai48home:<version>` | A specific release, e.g. `v1.2.0` |
| `ghcr.io/sofyan48/ai48home:edge` | Latest `main` of AI48HomeUI (rebuilt on every panel push) |

**Single node with SQLite**

```bash
git clone --depth 1 https://github.com/sofyan48/AI48Home.git && cd AI48Home
cp cluster.sqlite.example.yaml cluster.yaml          # set node and database options
cp config.example.yaml config.yaml                   # set management.secret-key
docker compose -f docker-compose.sqlite.yml pull
docker compose -f docker-compose.sqlite.yml run --rm \
  -v "$PWD/config.yaml:/AI48Home/config.yaml:ro" home ./AI48Home -import
docker compose -f docker-compose.sqlite.yml up -d
```

Use `docker-compose.pgsql.yml` and `cluster.pgsql.example.yaml` for PostgreSQL. Pin a release with `AI48HOME_IMAGE=ghcr.io/sofyan48/ai48home:v1.2.0`.

**Full cluster (PostgreSQL, Home, three CPA workers, Nginx)**

```bash
curl -fsSLO https://raw.githubusercontent.com/sofyan48/AI48Home/main/install.sh
sudo bash install.sh deploy            # writes /opt/cliproxy-cluster and starts everything
sudo bash install.sh upgrade           # pull the configured Home and worker images
sudo bash install.sh upgrade edge      # switch to the newest panel build
```

`install.sh` stores the Home image in `/opt/cliproxy-cluster/.env` as `HOME_IMAGE` and the worker image as `AI48PROXY_IMAGE`. Both images are pulled from GHCR; no worker image is built locally. Pin them before `deploy`, for example:

```bash
HOME_IMAGE=ghcr.io/sofyan48/ai48home:v1.2.0 \
AI48PROXY_IMAGE=ghcr.io/sofyan48/ai48proxy:v0.1.0 \
sudo -E bash install.sh deploy
```

Open `http://<host>:8327/admin` for the admin console and `http://<host>:8327/app` for the user portal. Client API traffic goes through `http://<host>:8317`.

**Publishing images.** `.github/workflows/docker-image.yml` builds the panel from `sofyan48/AI48HomeUI`, embeds it, and pushes to GHCR using `GITHUB_TOKEN` (no extra secret). It runs on `v*` tags, on the `panel-updated` dispatch sent by AI48HomeUI, and manually from the Actions tab. After the first push, set the package visibility to **Public** under *Packages → ai48home → Package settings* so servers can pull without logging in.

### Install on Kubernetes with Helm

Every `v*` release publishes the Helm chart to GHCR, next to the Docker image:

| Reference | Source |
| --- | --- |
| `oci://ghcr.io/sofyan48/charts/ai48home --version <version>` | A specific release; tag `v1.2.0` is chart version `1.2.0` |
| `oci://ghcr.io/sofyan48/charts/ai48home:latest` | Latest `v*` release |
| `ai48home-<version>.tgz`, `ai48home-latest.tgz` | GitHub release assets |

The chart deploys one Home instance and two AI48Proxy worker replicas by default, each with its own enrollment identity/PVC, with optional HPA and internal PostgreSQL. Each chart version pins its Home image to the same release tag.

**1. Write a private values file.** Keep it out of version control (`chmod 600`). The management key and gateway API key are optional: when left empty, the chart generates them on first install.

```yaml
# private-values.yaml
global:
  storageClass: nfs-client        # omit to use the cluster default StorageClass

postgresql:
  enabled: true                   # internal PostgreSQL; set false for an external database
  auth:
    username: ai48home
    database: ai48home
    password: "REPLACE_WITH_RANDOM_DATABASE_PASSWORD"  # openssl rand -hex 32

secrets:
  managementKey: ""               # optional; generated when empty
  gatewayApiKey: ""               # optional; generated when empty

workers:
  autoscaling:
    enabled: true
    minReplicas: 2
    maxReplicas: 8
```

For an external database, set `postgresql.enabled: false` and fill `postgresql.external.host`, `port`, and `sslmode`.

**2. Install or upgrade.** Check the target cluster first:

```bash
kubectl config current-context

helm upgrade --install ai48home oci://ghcr.io/sofyan48/charts/ai48home \
  --version 1.2.0 \
  --namespace ai48home --create-namespace \
  -f private-values.yaml --wait --timeout 10m
```

Pin `--version` in production. To inspect the newest release without installing it:

```bash
helm show values oci://ghcr.io/sofyan48/charts/ai48home --version 1.2.0
helm pull oci://ghcr.io/sofyan48/charts/ai48home:latest
```

**3. Read the keys from the Secret.** These commands print secrets; do not paste their output into logs or chat:

```bash
# Management key: sign in at /admin/connect
kubectl -n ai48home get secret ai48home-config \
  -o jsonpath='{.data.management-key}' | base64 -d; echo

# Gateway API key: client access to the worker API
kubectl -n ai48home get secret ai48home-config \
  -o jsonpath='{.data.gateway-api-key}' | base64 -d; echo
```

`helm get notes ai48home -n ai48home` shows the same commands again.

**4. Check the deployment and open the console.**

```bash
kubectl -n ai48home get pods,svc,pvc
kubectl -n ai48home get hpa ai48home-workers

kubectl -n ai48home port-forward service/ai48home-home 8327:8327
# Admin: http://127.0.0.1:8327/admin/connect   User: http://127.0.0.1:8327/app
```

Services with the default `fullnameOverride: ai48home`:

| Service | Port | Purpose |
| --- | --- | --- |
| `ai48home-home` | 8327 | Home Management HTTP, Web interface, and RESP/mTLS |
| `ai48home-worker` | 8317 | Worker API traffic for clients |

Port-forwarding is for debugging only. For production traffic, set `workers.service.type: LoadBalancer`, enable the chart's `ingress` or `gateway` routing, or configure an external Gateway/Ingress to `ai48home-worker`. Keep Home private unless you deliberately expose it. The chart supports optional HTTP Ingress and Gateway API HTTPRoute resources; both are disabled by default.

**Upgrade and uninstall.**

```bash
helm upgrade ai48home oci://ghcr.io/sofyan48/charts/ai48home \
  --version <new-version> -n ai48home -f private-values.yaml --wait --timeout 10m
helm rollback ai48home -n ai48home          # return to the previous revision
helm uninstall ai48home -n ai48home
```

Generated keys are reused on upgrades. Uninstall keeps worker PVCs; back up PostgreSQL and the `ai48home-config` Secret before deleting anything. To install from a source checkout instead of GHCR, use `./helm/ai48home` as the chart reference.

See [helm/README.md](helm/README.md) for external PostgreSQL, StorageClass, autoscaling, migration from the old `cliproxy` chart, and troubleshooting.

### Development Docker Compose

For a full local Home + AI48Proxy stack, check out both repositories as
siblings and run from the `AI48Home` root. The development Compose builds both
images from source and uses separate development ports/state; it does not use
the production installer or GHCR worker image.

The Compose file expects the ignored local files `cluster.dev.yaml` and
`config.dev.yaml` at the repository root. They are local operator files and must
be created from the examples in this checkout or from your existing development
configuration.

Start PostgreSQL and build/start Home first:

```bash
docker compose -f docker-compose.dev.yml up -d --build postgres

docker compose -f docker-compose.dev.yml run --rm --no-deps \
  -v "$PWD/config.dev.yaml:/workspace/config.yaml:ro" \
  ai48home go run ./cmd/home -config /workspace/config.yaml -import

docker compose -f docker-compose.dev.yml up -d --build ai48home
```

After Home is ready, issue a one-shot worker token with the development key and
save it locally (the file is ignored by git):

```bash
curl -fsS -X POST \
  -H 'X-MANAGEMENT-KEY: dev-management-key' \
  -H 'Content-Type: application/json' \
  --data '{"node_name":"ai48proxy-dev"}' \
  http://127.0.0.1:8327/v8/management/certificates/clients \
  | jq -er '.home_jwt' > home.jwt
chmod 600 home.jwt
docker compose -f docker-compose.dev.yml up -d --build ai48proxy
```

After the initial image build, the existing `.air.toml` reloads Home source
changes, and `cargo watch` reloads AI48Proxy source changes. Rebuild only after
changing `Dockerfile.dev` or the development toolchain.

Home is available at `http://localhost:8327` and the single development worker
is available directly at `http://localhost:8317`. `cluster.dev.yaml`,
`config.dev.yaml`, `home.jwt`, `logs.dev/`, and `plugins.dev/` are intentionally
ignored by git. Set
`HOME_JWT_FILE` if the token is stored elsewhere.

### Developing both projects locally

For frontend development, clone both repositories into sibling directories and run AI48Home and AI48HomeUI in separate terminals. Environment requirements are listed in [go.mod](go.mod) and the [AI48HomeUI README](https://github.com/sofyan48/AI48HomeUI#readme).

For a new local setup without `cluster.yaml`, copy Home's `config.example.yaml` to `config.yaml` and set a non-empty `management.secret-key`. Import that configuration into the local SQLite database, then start Home in one terminal:

```bash
cd AI48Home
go run ./cmd/home -config config.yaml -import
go run ./cmd/home -addr 127.0.0.1:8327
```

An existing configured Home backend can be used directly.

Start AI48HomeUI in another terminal:

```bash
cd AI48HomeUI
bun install --frozen-lockfile
bun run dev
```

Open `http://localhost:3000/admin/connect`, enter `http://127.0.0.1:8327` as the management endpoint, and sign in with the management key. Frontend changes can be previewed without rebuilding Home.

### Building and releasing the bundled frontend

To build Home with the Web interface from source, first build AI48HomeUI, then copy the complete output into Home before compiling. Starting from the parent of the two sibling repositories:

```bash
cd AI48HomeUI
bun install --frozen-lockfile
bun run build:embedded
cp -R dist/. ../AI48Home/internal/managementasset/static/
cd ../AI48Home
go build -o AI48Home ./cmd/home
```

Home's source checkout does not include the generated frontend. Neither `go build` nor `docker build` builds AI48HomeUI automatically, so prepare the complete frontend output before building either a binary or a Docker image.

Official releases automate this process: a version tag triggers the frontend build, followed by Home packaging and publication of binaries or Docker images. See the [binary release workflow](.github/workflows/release.yaml) and [Docker workflow](.github/workflows/docker-image.yml) for details.

## Key Features

- **Scalable Node Management**: Supports connecting an unlimited number of CLIProxyAPI nodes for credential distribution, ideal for large-scale deployments.
- **Multi-Provider OAuth2 Support**: Manages OAuth2 credentials for various AI model providers, including OpenAI, Claude, Codex, Antigravity, Kimi, xAI, and more.
- **High Availability**: Supports multi-account management and round-robin load balancing to ensure optimal performance and reliability.
- **Secure Access**: Provides a streamlined CLI authentication flow to ensure secure access to APIs.
- **Plugin Support**: Supports installing and enabling CLIProxyAPI-compatible plugins through Home Management.
- **Flexible Upstream Integration**: Easily integrate upstream providers via configuration, including:
  - OpenAI-compatible providers (e.g., OpenRouter)
  - OpenAI Responses protocol providers
  - Anthropic Messages protocol providers
  - Google Gemini protocol providers

## Cluster Mode

Home now always runs from a database-backed runtime. When `cluster.yaml` is absent, Home runs the same scheduler, node discovery, leader election, event watcher, management, and RESP logic on a local SQLite database. The default SQLite path is `home.db`; use `-sqlite-path` to override it.

When `cluster.yaml` exists, Home enables cluster mode and uses the database backend declared in that file. Copy `cluster.sqlite.example.yaml` or `cluster.pgsql.example.yaml` to `cluster.yaml` as a starting point. PostgreSQL accepts either individual connection fields or a connection string in `pgsql.postgres_uri`. SQLite supports only one active Home process per database; deployments with multiple active Homes must use PostgreSQL. Multiple CPA nodes may connect to a single SQLite-backed Home. When using SQLite with `cluster.yaml`, configure `node.external-ip` so CPA clients can reach the Home.

Before using automatic CPA connection rebalancing, upgrade all Home nodes to a version that supports it. Automatic rebalancing is not guaranteed while old and new Home versions coexist.

Set `slow-query-threshold` under the selected `pgsql` or `sqlite` backend to control slow SQL logging. It accepts Go duration strings such as `"500ms"` or `"1s"` and defaults to `"200ms"`.

PostgreSQL pool limits are configured in `cluster.yaml`: `pgsql.max-open-conns` defaults to `0` (unlimited), and `pgsql.max-idle-conns` defaults to `2` (capped by a positive total limit). Explicit `max-idle-conns: 0` disables idle connection retention; an explicit idle limit must not exceed a positive total limit. Limits apply per Home process and require a restart.

`config.yaml` and `auth-dir` are no longer runtime storage. They are import/export exchange formats only. To migrate an existing local setup into the database, run:

```bash
./AI48Home -import
```

By default, import reads `./config.yaml` and then resolves credentials from the `auth-dir` value inside that config. Use `-config <path>` to select another config file, `-auth-dir <path>` to override credential discovery, and `-sqlite-path <path>` when importing into a non-default SQLite database. Import is idempotent, so rerunning it with the same files does not duplicate records.

To export config and credentials back to local files, run:

```bash
./AI48Home -export
```

Export writes `./config.yaml` and credential files under `~/.cli-proxy-api/` by default. Use `-export-dir <path>` to write `<path>/config.yaml` and credential files under `<path>/auths/`. It refuses to overwrite an existing `config.yaml` or a non-empty credential directory at the selected target.

`-export` / `-import` preserve the legacy exchange format. API keys are exported only as their actual key values, so Home database-only metadata such as API key display names, user ownership, and channel/model-group bindings is not a complete backup. Use `-db-export <snapshot.zip>` and `-db-import <snapshot.zip>` for full database backup and restore. Database snapshot format v4 includes API key display names and cannot be imported by older Home versions. Before upgrading, retain a database backup or an older-version-compatible snapshot for rollback.

In cluster mode, the Management API operates directly on database data. The default listen port comes from `node.port` in `cluster.yaml`; the startup `-addr` flag can override the listen address. Set `node.external-port` when a reverse proxy changes the port that clients must use; when omitted, the advertised cluster node port follows the final listen port.

RESP password authentication has been removed. Home RESP access only accepts mTLS-authenticated clients; CPA should connect with `-home-jwt` or configured TLS material. `allow-host` is only an IP allowlist and is not a password mechanism.

Docker Compose uses `docker-compose.sqlite.yml` or `docker-compose.pgsql.yml`. Copy the matching `cluster.sqlite.example.yaml` or `cluster.pgsql.example.yaml` to `cluster.yaml`, then use `/AI48Home/data/home.db` for SQLite or `postgres` as the PostgreSQL host. Select either database type when importing existing local state:

```bash
COMPOSE_FILE=docker-compose.sqlite.yml
# PostgreSQL: COMPOSE_FILE=docker-compose.pgsql.yml
docker compose -f "$COMPOSE_FILE" run --rm \
  -v "$PWD/config.yaml:/AI48Home/config.yaml:ro" \
  -v "$PWD/auths:/root/.cli-proxy-api" \
  home ./AI48Home -import
```

## Strict Credential Concurrency Limits

See the [Management API documentation](docs/management/api.md#credential-concurrency-policy-and-limiter-routes) for credential concurrency policy configuration and activation prerequisites.

## Contributing

Issues, feature requests, and pull requests are welcome. Choose the repository that maintains the affected component:

- **Frontend / Web UI**: Report page display and interaction issues in [AI48HomeUI Issues](https://github.com/sofyan48/AI48HomeUI/issues), and submit frontend changes to AI48HomeUI.
- **Backend**: Report service, scheduling, and data management issues in [AI48Home Issues](https://github.com/sofyan48/AI48Home/issues), and submit backend changes here.

For changes spanning both projects, link the related issues or pull requests. Update the relevant [Management API](docs/management/api.md) or [User API](docs/user/api.md) documentation when an interface changes.

1. Fork the repository.
2. Create your feature branch (`git checkout -b feature/amazing-feature`).
3. Commit your changes (`git commit -m 'Add some amazing feature'`).
4. Push to the branch (`git push origin feature/amazing-feature`).
5. Open a Pull Request.

## License

This project is licensed under the MIT License. See the [LICENSE](LICENSE) file for details.

## Credits and License

AI48Home is a further development of [router-for-me/CLIProxyAPIHome](https://github.com/router-for-me/CLIProxyAPIHome) and [router-for-me/Home-Management-Center](https://github.com/router-for-me/Home-Management-Center). Released under the [MIT License](LICENSE); the original Router-For.ME copyright notice is retained alongside the AI48Home notice.
