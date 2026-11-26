# DeployMCP

**Zero-SSH, ultra-lightweight Docker deployment for AI coding agents.**
A self-hosted "Vercel" built for Claude Code, Cursor and Windsurf.

```
You: "Deploy this Next.js app to app.mydomain.com. If the build fails, read the logs and fix it."
AI:  deploy_app → build failed → get_logs → fixes Dockerfile → deploy_app → ✅ https://app.mydomain.com
```

- **Zero-SSH, no IP whitelist.** The AI talks HTTPS to a tiny agent using a **scoped token** (per-app, read/write). There is no "run arbitrary shell" endpoint.
- **Tiny.** One static Go binary (~12 MB, ~20 MB RAM) with embedded SQLite. No database, no panel.
- **Automatic HTTPS.** Caddy issues certificates for every domain you deploy to.
- **Built for the agentic loop.** Compact, de-noised logs and **auto-rollback**: if a new release crashes or fails its healthcheck, the old one keeps serving and the AI gets the last 50 error lines and exit code.
- **Secrets stay off the LLM.** The AI can set env vars and see their *names*, never their values.

## How it works

```
[ Your machine ]                                   [ Your VPS ]
Claude Code / Cursor                               Caddy (auto HTTPS, reverse proxy)
   │ stdio (MCP)                                      │
   ▼                                                  ▼
deploymcp (npx) ──── .tar.gz over HTTPS ────▶ deploymcp-agent (Go, SQLite)
 packs project,      + scoped token              build → healthcheck → switch / rollback
 honors .gitignore  ◀── structured logs ────────  │ Docker Engine
                                                  ▼
                                          your containers / compose projects
```

## Install (60 seconds)

**1. On the VPS** (Ubuntu/Debian, root). Point a DNS record for the agent (e.g. `deploy.example.com`) at the server first, and open ports 80/443.

```bash
curl -fsSL https://github.com/ZenithHawking/deploymcp/releases/latest/download/install.sh \
  | sh -s -- --domain deploy.example.com --email you@example.com
```

It installs Docker (if missing), Caddy and the agent as a systemd service, then prints:

```
✅ DeployMCP Agent is running!
👉 Run this command on your local machine to connect Claude Code:

claude mcp add deploymcp --scope user \
  -e DEPLOYMCP_URL=https://deploy.example.com \
  -e DEPLOYMCP_TOKEN=dmcp_live_9f8a7b6c... \
  -- npx -y deploymcp
```

plus the equivalent `mcp.json` block for Cursor / Windsurf.

**2. In your editor**, just ask:

> Deploy the current project as `my-api` on `api.example.com`. If it fails, read the logs and fix it.

## MCP tools

| Tool | What it does |
| --- | --- |
| `deploy_app` | Pack the project (honors `.gitignore`, skips `node_modules`, `.git`, `.env*`), upload, build, healthcheck, switch traffic with HTTPS. Auto-rollback on failure. Inputs: `app_name`, `project_path`, `domain?`, `port?`, `healthcheck_path?` |
| `get_logs` | Runtime or build logs, de-noised (ANSI, progress bars, repeats, dependency stack frames removed). Inputs: `app_name`, `type` (`runtime`/`build`), `lines`, `level?` (`error`) |
| `rollback_app` | Back to the latest previously-healthy release, or a given `release_id` (healthchecked first). |
| `list_apps` | Apps in the token's scope: state, URL, current release, last 5 releases. |
| `manage_env` | `list_keys` (names only) or `update` with `set` / `delete`. The app is recreated; if it fails to start, the change is reverted. |

## What your project needs

A `Dockerfile` **or** a `docker-compose.yml` in the project root.

- **Dockerfile mode:** the image is built as `deploymcp/<app>:<release>` and run as a fresh container next to the old one (blue-green). The port is `port` → first `EXPOSE` → `3000`. The app must listen on `0.0.0.0`.
- **Compose mode:** the project runs under a stable name `dmcp-<app>` so named volumes (e.g. databases) persist across releases. The service receiving traffic is the one exposing `port`, else `web`/`app`/`api`/`server`/`frontend`, else the first service with a `build:`. Built images are tagged per release so rollbacks don't rebuild. Compose releases are replaced in place (a short restart), and the previous release is re-started automatically if the new one fails.

Healthcheck: `GET http://127.0.0.1:<port><healthcheck_path>` every 2 s, up to 15 times; any status below 500 passes. A container that exits or restarts fails immediately.

Env vars set with `manage_env` are stored in `/var/lib/deploymcp/apps/<app>/.env` (mode 0600) and injected with `--env-file` / `env_file`. In compose mode they are also available for `${VAR}` interpolation.

## Security model

- Tokens look like `dmcp_live_<64 hex>`; only their SHA-256 is stored.
- Scopes: a list of apps (or `*`) plus `read` or `write`. Read covers `list_apps`, `get_logs`, `manage_env list_keys`.
- Bundles are extracted only under `/var/lib/deploymcp/apps/<app>/releases/<release_id>`. Absolute paths and `..` are rejected; symlinks and device files are skipped. Max 100 MB compressed / 1 GB extracted.
- Env values are never returned by any endpoint.
- Every write (deploy, rollback, env update) is written to an audit table.

The agent runs as root because it drives the Docker daemon, and Docker access is root-equivalent anyway. Give AI tools tokens scoped to the apps they work on.

```bash
deploymcp-agent token create --apps my-api,my-web --perm write --name cursor-laptop
deploymcp-agent token create --apps '*' --perm read --name dashboards
deploymcp-agent token list
deploymcp-agent token revoke 3
```

(Run with `DEPLOYMCP_DATA_DIR=/var/lib/deploymcp` or `--data-dir` if your shell doesn't load `/etc/deploymcp/agent.env`.)

## REST API

All endpoints except `/health` need `Authorization: Bearer <token>`.

| Method | Path | |
| --- | --- | --- |
| GET | `/health` | Liveness + version |
| GET | `/v1/apps` | Apps in scope with state and recent releases |
| POST | `/v1/apps/{name}/deploy` | multipart: `bundle` (tar.gz), `domain?`, `port?`, `healthcheck_path?`. `200` healthy, `422` failed (with logs) |
| GET | `/v1/apps/{name}/logs` | `?type=runtime\|build&lines=100&level=error&compact=true&release_id=` |
| POST | `/v1/apps/{name}/rollback` | `{"release_id": "..."}` (optional) |
| GET | `/v1/apps/{name}/env` | Keys only |
| PUT | `/v1/apps/{name}/env` | `{"set": {"K": "V"}, "delete": ["OLD"]}` |

## Behind Cloudflare Tunnel (homelab / dynamic IP)

No open ports needed. Cloudflare terminates TLS; Caddy serves plain HTTP on `:80` and routes by hostname.

```bash
curl -fsSL .../install.sh | sh -s -- --tunnel --domain deploy.example.com
```

In your tunnel (Zero Trust → Networks → Tunnels → Public hostnames), route to `http://localhost:80`:

| Hostname | Service |
| --- | --- |
| `deploy.example.com` | `http://localhost:80` |
| `*.example.com` | `http://localhost:80` |

For the wildcard, also add a DNS record `CNAME * → <tunnel-id>.cfargotunnel.com` (proxied). Then deploy apps with `domain: app.example.com`; no tunnel change is needed per app or per deploy.

## Agent configuration

`deploymcp-agent serve` flags (or env vars in `/etc/deploymcp/agent.env`):

| Flag | Env | Default |
| --- | --- | --- |
| `--data-dir` | `DEPLOYMCP_DATA_DIR` | `/var/lib/deploymcp` |
| `--listen` | `DEPLOYMCP_LISTEN` | `127.0.0.1:7070` |
| `--domain` | `DEPLOYMCP_DOMAIN` | (none) agent's own HTTPS domain |
| `--caddy-admin` | `DEPLOYMCP_CADDY_ADMIN` | `http://localhost:2019` (`off` to disable) |
| `--acme-email` | `DEPLOYMCP_ACME_EMAIL` | (none) |
| `--public-host` | `DEPLOYMCP_PUBLIC_HOST` | used in URLs of apps without a domain |
| `--tunnel` | `DEPLOYMCP_TUNNEL=1` | Caddy serves HTTP only (Cloudflare Tunnel does TLS) |

The agent owns Caddy's configuration: it rebuilds the full route table and loads it through the Admin API on every change and at startup. The installer runs Caddy with `--resume` so routes survive restarts.

## Development

```bash
# agent (Go 1.23+)
cd agent && go test ./... && go build ./cmd/agent

# MCP server (Node 20+)
cd mcp && npm install && npm test

# local run without Caddy
./agent init --data-dir ./data
./agent serve --data-dir ./data --listen 127.0.0.1:7070 --caddy-admin off
DEPLOYMCP_URL=http://127.0.0.1:7070 DEPLOYMCP_TOKEN=dmcp_live_... node mcp/dist/index.js
```

Releases: push a `v*` tag; GitHub Actions builds `linux/amd64` and `linux/arm64` binaries and attaches them with `install.sh` to the GitHub release. Publish the MCP package with `cd mcp && npm publish`.

## Roadmap (v0.2)

- Human-in-the-loop approval via Telegram/Discord for production deploys and deletes
- Git pull mode (deploy from a GitHub URL)
- Multiple servers (`staging`, `production`) from one MCP config

## License

MIT
