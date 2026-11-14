# AI48Home

English | [中文](README_CN.md)

> AI48Home is based on [router-for-me/CLIProxyAPIHome](https://github.com/router-for-me/CLIProxyAPIHome) and its panel [Home Management Center](https://github.com/router-for-me/Home-Management-Center). Both are MIT-licensed; the original copyright notice is kept in [LICENSE](LICENSE).

AI48Home is a comprehensive credential **management, scheduling, and distribution** service designed for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI).

It serves as a centralized hub for managing and scheduling API credentials in large-scale CLIProxyAPI cluster deployments. CLIProxyAPI nodes communicate with Home to retrieve necessary API credentials and report usage statistics.

## AI48Home and AI48HomeUI

AI48Home provides the backend service, and [AI48HomeUI](https://github.com/sofyan48/AI48HomeUI) (the AI48Hub panel) provides the Web interface. **The two projects are maintained separately. AI48Home releases and images embed AI48HomeUI so they can be deployed and updated together.**

### Deploying an official release

AI48Home binaries and Docker images include the Web interface, so AI48HomeUI does not need to be deployed separately. After starting Home, open its address, such as `http://localhost:8327`. The administrator console is at `/management.html`, and the user workspace is at `/user.html`.

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
sudo bash install.sh upgrade           # pull the newest image and replace Home only
sudo bash install.sh upgrade edge      # switch to the newest panel build
```

`install.sh` stores the image in `/opt/cliproxy-cluster/.env` as `HOME_IMAGE`; override it before `deploy` with `HOME_IMAGE=ghcr.io/sofyan48/ai48home:v1.2.0 sudo -E bash install.sh deploy`.

Open `http://<host>:8327/management.html` for the admin console and `/user.html` for the user portal.

**Publishing images.** `.github/workflows/docker-image.yml` builds the panel from `sofyan48/AI48HomeUI`, embeds it, and pushes to GHCR using `GITHUB_TOKEN` (no extra secret). It runs on `v*` tags, on the `panel-updated` dispatch sent by AI48HomeUI, and manually from the Actions tab. After the first push, set the package visibility to **Public** under *Packages → ai48home → Package settings* so servers can pull without logging in.

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

Open `http://localhost:3000/management.html`, enter `http://127.0.0.1:8327` as the management endpoint, and sign in with the management key. Frontend changes can be previewed without rebuilding Home.

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
