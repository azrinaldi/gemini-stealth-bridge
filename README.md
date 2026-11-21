# gemini-stealth-bridge (`gsb`)

[![release](https://img.shields.io/github/v/release/azrinaldi/gemini-stealth-bridge?sort=semver)](https://github.com/azrinaldi/gemini-stealth-bridge/releases/latest)
[![release workflow](https://img.shields.io/github/actions/workflow/status/azrinaldi/gemini-stealth-bridge/release.yml)](https://github.com/azrinaldi/gemini-stealth-bridge/actions/workflows/release.yml)
[![go](https://img.shields.io/github/go-mod/go-version/azrinaldi/gemini-stealth-bridge)](go.mod)
[![license](https://img.shields.io/github/license/azrinaldi/gemini-stealth-bridge)](LICENSE)
[![ghcr](https://img.shields.io/badge/ghcr.io-gemini--stealth--bridge-blue)](https://github.com/azrinaldi/gemini-stealth-bridge/pkgs/container/gemini-stealth-bridge)

OpenAI-compatible HTTP proxy for [Gemini Web](https://gemini.google.com/app). Point OpenAI SDK clients, routers (such as `9router`), and coding agents at `gsb` to use Gemini Web via **Chat Completions**, **Responses**, **Image/Video Generation**, and a built-in **MCP Search server** — backed by multi-account pooling, 5h/weekly quota-aware failover, and an embedded Web Control Plane.

![gsb Web Control Plane](docs/assets/web-ui.png)

> **Unofficial.** `gsb` authenticates using Google session cookies rather than Gemini Developer API keys and does not run headless Chrome at runtime (it uses a Go uTLS/HTTP2 transport). Upstream protocol changes, sign-in checks, quota rules, and account limits can change without notice. Use only accounts you are authorized to access.

---

## What `gsb` Does

| Feature | Summary |
|---|---|
| **OpenAI Chat & Responses** | `POST /v1/chat/completions` and `POST /v1/responses` (JSON and SSE streaming), supporting thinking/reasoning effort (`low`/`medium`/`high`), image inputs, and continuation caching |
| **Function Calling** | Translates tool definitions into a strict prompt contract, validates arguments against JSON Schema, repairs common model formatting issues, and prevents mid-stream tool-call leaks |
| **Multi-Account Pool & Quotas** | `rotate`, `lru`, and `fill-first` pool routing across accounts with 5-hour & 7-day weekly quota tracking, group isolation (`BRIDGE_GROUP_TOKENS`), and per-account egress proxies |
| **Native Image & Video** | Imagen / Nano Banana images and Veo MP4 videos (with audio) via `/v1/images/generations`, `/v1/videos/generations`, Chat, or Coding Agent `generate_media` tool, served over `/v1/media/<id>` |
| **Web Search Grounding & MCP** | Extracts Google Search citations as `Sources:` and exposes `web_search` over Streamable HTTP MCP at `POST /mcp` (`/v1/mcp`) |
| **Web Control Plane** | Single-binary embedded dashboard at `GET /ui` for account onboarding, priority reordering, live quota bars, and runtime configuration (`config.env`) |

### Compatibility Notes

- **Sampling controls (`temperature`, `top_p`, `seed`)** are accepted but ignored because Gemini Web does not expose them.
- **Output controls** (`max_tokens` / `max_completion_tokens`, `stop`, `response_format` JSON unwrapping) are enforced locally by the bridge.
- **History**: Clients using `/v1/responses` must resend history in `input` (`previous_response_id` is not a server-side session store). Token counts are estimated locally.
- **Plain HTTP**: `gsb` does not terminate TLS; use loopback (`127.0.0.1:20133`), SSH tunneling, or a TLS reverse proxy for remote access.

---

## What's New in v0.4.5

- **Full Session Cookie & Isolated Login Fix**: Incorporates [PR #12](https://github.com/azrinaldi/gemini-stealth-bridge/pull/12) (fixes [Issue #11](https://github.com/azrinaldi/gemini-stealth-bridge/issues/11)) — huge thanks to [@PramaAditya](https://github.com/PramaAditya)! Preserves `OSID`, `__Secure-OSID`, `LSID`, `__Host-1PLSID`, `__Host-3PLSID`, `__Host-GAPS`, and `ACCOUNT_CHOOSER` with per-domain scoring; `--login` / `--login-helper` / `--desktop-ui` read browser-level CDP cookies (`Storage.getCookies`), wait for `OSID` to settle before closing the temporary browser, retry transient post-login propagation errors once, and commit atomically after `Bootstrap` succeeds.
- **Zero-Race Cookie Rotation & Cleanup**: Non-200 `/RotateCookies` responses no longer clear cookies or mark rotation complete; `reloadGen` is isolated from `cookieGen`; stale disk state cannot overwrite newer in-memory cookies.
- **Streaming Stability & Smart Stall Recovery**: `frameWatch` stays armed for the entire stream lifecycle with adaptive per-model no-frame deadlines (`30s` / `45s` / `75s`), evicts stalled continuation caches, adds cross-account stall rollback, and fails over cleanly on transport, upload, HTTP, parse, and incomplete upstream errors.
- **Tool-Calling Anti-Leak Hardening**: Prevents plain-text leakage when models append trailing text after closing code fences (e.g. ```` ```Groq ````) or glue ```` ```json ```` immediately after punctuation, deduplicates repeated blocks, and fixes `LiveToolCallStreamer` slicing and schema edge cases.
- **One-Port Desktop UI (`--desktop-ui`) & UI/UX Refresh**: Redesigned Web Control Plane with quick-connect Pool/MCP cards, drag/arrow priority ordering, runtime config source badges, and a single-port (`127.0.0.1:20133`) desktop proxy over a private SSH Unix socket for headless VPS servers.
- **Cross-Platform CI & Windows Fixes**: Fixed Windows home/mode unit tests and restored POSIX executable permissions on helper scripts.

> Older release notes are available on [GitHub Releases](https://github.com/azrinaldi/gemini-stealth-bridge/releases).

---

## Requirements

- **Runtime**: Linux, macOS, or Windows (`amd64` / `arm64`) — single static binary, zero runtime dependencies.
- **Build from source**: Go **1.26.0+** and Git.
- **Account**: A Google account with access to [Gemini Web](https://gemini.google.com/app).
- **Browser (optional)**: Chrome, Chromium, Brave, Edge, or Vivaldi for `--login`; Firefox/Arc profiles are also readable via `--import-chrome`.

---

## Quick Start

### 1. Install

**Prebuilt static binary (recommended):**

```sh
# Linux amd64 (other targets: linux_arm64, darwin_amd64, darwin_arm64, windows_amd64.exe, windows_arm64.exe)
curl -fL -o gsb https://github.com/azrinaldi/gemini-stealth-bridge/releases/latest/download/gsb_linux_amd64
chmod +x gsb
./gsb --version
```

Optional checksum verification:

```sh
curl -fLO https://github.com/azrinaldi/gemini-stealth-bridge/releases/latest/download/SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing
```

**Or build from source / `go install`:**

```sh
go install github.com/azrinaldi/gemini-stealth-bridge/cmd/gsb@latest
# or from a local clone:
go build -o gsb ./cmd/gsb
```

### 2. Sign In & Verify

```sh
./gsb --login acc1
./gsb --probe
```

`--login` opens a disposable browser profile with Device Bound Session Credentials (DBSC) disabled so the captured cookies remain renewable by `gsb` without a background browser competing for rotation. `--probe` verifies model discovery and 5h/weekly quota.

### 3. Start the Daemon

```sh
./start.sh
# or directly:
./gsb --web
```

- **API Base URL**: `http://127.0.0.1:20133/v1`
- **Web Control Plane**: `http://127.0.0.1:20133/ui`

### 4. Send a Test Request

```sh
# List live models
curl -sS http://127.0.0.1:20133/v1/models -H 'Authorization: Bearer public'

# Send a chat completion using the pool key (auto-failover across all accounts)
curl -sS http://127.0.0.1:20133/v1/chat/completions \
  -H 'Authorization: Bearer public' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gemini-4-hydrogen","messages":[{"role":"user","content":"Reply with OK."}]}'
```

Inspect the `X-GSB-Account` response header to see which account served the request.

---

## Account Management & Keys

### API Keys

| Key Type | Value | Behavior |
|---|---|---|
| **Pool key** (default) | `BRIDGE_API_KEY` (default `public`, or `gsb-pool-*` shown in UI) | Routes and fails over automatically across all eligible accounts. **Use this for agents and routers.** |
| **Group key** | `BRIDGE_GROUP_TOKENS` (`token:group`) | Routes and fails over only within the accounts assigned to that group (`BRIDGE_ACCOUNT_GROUP`). |
| **Account key** | `BRIDGE_ACCOUNT_TOKENS` (`token:account_id`), or `gsb-<name>` in `public` mode | Pins requests to one account with **no failover**. |

Accepted headers: `Authorization: Bearer <key>` (recommended), `x-api-key`, `x-goog-api-key`, or `?key=`.

### Remote VPS & Headless Onboarding

1. **Desktop UI + Local Browser over one SSH socket (`--desktop-ui`)** *(recommended)*:
   ```sh
   # On the VPS:
   gsb --web

   # On your laptop:
   install -d -m 700 "$XDG_RUNTIME_DIR/gsb-desktop"
   ssh -NT -o ExitOnForwardFailure=yes -o StreamLocalBindMask=0177 -o StreamLocalBindUnlink=yes \
     -L "$XDG_RUNTIME_DIR/gsb-desktop/bridge.sock:127.0.0.1:20133" user@vps
   gsb --desktop-ui
   ```
   Open `http://127.0.0.1:20133/ui` locally — clicking **+ Add Account → Launch** runs browser sign-in on your laptop and imports directly into the VPS daemon.
2. **Desktop Login Helper (`--login-helper`)**: Run `gsb --login-helper` on your laptop (port `20134`), tunnel `20133` with `ssh -L 20133:127.0.0.1:20133 user@vps`, and paste the helper token into the Web UI dialog.
3. **Incognito Cookie Export**: Sign in inside a new Incognito window, copy cookies via Cookie-Editor JSON or raw `Cookie` header, **close the Incognito window immediately without signing out**, and paste into **+ Add Account** or `gsb --import-cookie <name>`.

### Importing Existing Browser Profiles & Groups

```sh
./gsb --import-chrome acc2 --chrome-profile "Profile 2"
./gsb --import-cookie acc3 --cookie-file /path/to/cookies.json
```

Multi-user groups and per-account proxies:

```sh
BRIDGE_ACCOUNT_GROUP="acc1:team-a,acc2:team-b" \
BRIDGE_GROUP_TOKENS="alice-tok:team-a,bob-tok:team-b" \
BRIDGE_EGRESS_PER_ACCOUNT="acc1:socks5://127.0.0.1:1081" ./gsb --web
```

---

## Client & Router Integration

### Direct OpenAI SDK / Coding Agents

- **Base URL**: `http://127.0.0.1:20133/v1`
- **API Key**: Pool key (`public` or your `BRIDGE_API_KEY`)
- **Model**: Pick an ID from `GET /v1/models` (prefixes `gsb/`, `google/`, `models/`, `vx/`, `vertex/` and suffix `@think=low|medium|high` are supported).

For **OMP**, an optional wait-timer extension is included:

```sh
omp --extension ./scripts/omp-gsb-progress.ts
```

### `9router`

Configure **one** OpenAI-compatible provider pointing to `http://127.0.0.1:20133/v1` with the **pool key** (let `gsb` handle account rotation and session affinity instead of creating per-account router connections). Verify metadata forwarding:

```sh
./scripts/check-router-metadata.sh http://127.0.0.1:20133 public
./scripts/check-router-metadata.sh --compare http://127.0.0.1:20133 http://127.0.0.1:20130 - sk-router-key
```

### MCP Client (`web_search`)

```json
{
  "mcpServers": {
    "gemini-search": {
      "type": "http",
      "url": "http://127.0.0.1:20133/mcp",
      "headers": { "Authorization": "Bearer public" }
    }
  }
}
```

---

## API Reference

| Method | Path | Auth | Description |
|---|---|---|---|
| `GET` | `/ui`, `/dashboard` | UI gate | Embedded Web Control Plane |
| `GET` | `/health`, `/readyz` | None | Health check & per-account readiness (`?strict=1`) |
| `GET` | `/v1/models` | API key | Live model catalog with capabilities & `ETag` (`?fresh=1`) |
| `POST` | `/v1/chat/completions` | API key | Chat Completions (JSON or SSE, `tools`, `reasoning_effort`, images) |
| `POST` | `/v1/responses` | API key | Responses API (`input`, `instructions`, `tools`, named SSE events) |
| `POST` | `/v1/images/generations` | API key | Generate images (`url` or `b64_json`, `aspect_ratio`, `size`, `format`) |
| `POST` | `/v1/videos/generations` | API key | Generate Veo MP4 videos with audio (`aspect_ratio`, `duration`) |
| `GET` | `/v1/media/<id>` | None | Local proxy for generated images & videos (24h in-memory cache) |
| `POST` | `/mcp`, `/v1/mcp` | API key | MCP Streamable HTTP (`web_search` tool) |
| `GET` | `/v1/accounts`, `/v1/quota`, `/v1/sessions` | API key | Account health/status, 5h/7d quotas (`?live=1` / `?fresh=1`), H2/cache state |
| `POST` | `/v1/refresh` | API key | Reload credential files and refresh models (rejects browser `Origin`) |
| `*` | `/v1/admin/*` | Admin key | Web UI runtime config & account control API |

### Native Media Generation in Coding Agents

When a request includes `tools` (with `tool_choice` unset or `auto`) and recent user messages ask for an image, diagram, or video, `gsb` automatically injects `generate_media` and returns local `/v1/media/<id>` URLs plus a ready-to-run download command.

---

## Configuration

Settings load from environment variables first, then `$BRIDGE_HOME/config.env` (`0600`), then built-in defaults. Settings in the **Runtime** group can also be edited live in the Web UI.

| Variable | Default | Description |
|---|---|---|
| `BRIDGE_HOST` / `BRIDGE_PORT` | `127.0.0.1` / `20133` | Listen host and port (`HOST`/`PORT` also supported) |
| `BRIDGE_HOME` | `~/.gemini-stealth-bridge` | Directory for `accounts/*.json`, `config.env`, and `session-cache.json` |
| `BRIDGE_API_KEY` / `BRIDGE_ADMIN_KEY` | `public` / empty | Pool API key and admin key for remote `/ui` & `/v1/admin/*` |
| `BRIDGE_WEB_UI` | `false` (`1` in `start.sh`/Docker) | Enable `/ui` and `/v1/admin/*` |
| `BRIDGE_ACCOUNT_STRATEGY` | `rotate` | Pool routing: `rotate`, `lru`, or `fill-first` |
| `BRIDGE_ACCOUNT_PRIORITY` | empty | Comma-separated account order for `fill-first` & UI ranking |
| `BRIDGE_QUOTA_PREFLIGHT` | `strict` | `strict` skips spent accounts, `advisory` logs, `off` disables |
| `BRIDGE_DEFAULT_THINKING` | `low` | Default thinking effort: `low`, `medium`, `high` |
| `BRIDGE_MAX_RPM_PER_ACCOUNT` / `..._CONCURRENT_...` | `24` / `2` | Per-account rolling RPM and active stream caps (`0` disables) |
| `BRIDGE_REJECT_DOWNGRADE` / `..._LABEL_MISMATCH` | `true` / `false` | Failover/reject if upstream silently serves another model or label |
| `BRIDGE_LIVE_THOUGHTS` / `BRIDGE_LIVE_TOOL_CALLS` | `true` / `false` | Stream `reasoning_content`; validate complete tool calls vs deltas |
| `BRIDGE_HISTORY_OFFLOAD` / `BRIDGE_SESSION_CACHE` | `true` / `true` | Offload long history as attachments; persist continuation metadata |
| `BRIDGE_NO_FRAME_TIMEOUT_MS` / `BRIDGE_STALL_TIMEOUT_MS` | `75000` / `90000` | Liveness and inter-frame stream deadlines |
| `BRIDGE_EARLY_SSE_GRACE_MS` | `20000` | Flush SSE keepalive headers while waiting for slow reasoning turns |
| `BRIDGE_PROXY_SERVER` / `BRIDGE_EGRESS_PER_ACCOUNT` | empty | Global or per-account (`id:url`) HTTP/SOCKS5 outbound proxy |
| `BRIDGE_IMPERSONATE` | `chrome_150_psk` | uTLS fingerprint (`chrome_152_psk`, `chrome_150_psk`, `firefox_148`, …) |
| `BRIDGE_LOG_REQUESTS` | `false` | Log one redacted JSON telemetry line per request |

Run `./gsb --check-config` to verify your configuration. For all advanced variables, see [`AGENTS.md`](./AGENTS.md).

---

## Docker & Security

Docker image (~12 MB, non-root `uid 10001`, Web UI enabled):

```sh
# Recommended: set an admin key since Docker bridge NAT appears non-loopback
BRIDGE_ADMIN_KEY=$(openssl rand -hex 24) docker compose up -d
```

Security essentials:
- **Never expose `127.0.0.1:20133` with `BRIDGE_API_KEY=public` to an untrusted network.** Set both `BRIDGE_API_KEY` and `BRIDGE_ADMIN_KEY` and place `gsb` behind SSH tunneling or an authenticated TLS reverse proxy.
- **Keep `$BRIDGE_HOME` private (`0600`).** Never paste cookies, `accounts/*.json`, or `session-cache.json` into issues, chats, or commits.

---

## CLI & Troubleshooting

| Command | Purpose |
|---|---|
| `./gsb --login <name>` | Isolated browser login (recommended) |
| `./gsb --probe` | Verify saved sessions, models, and live quota |
| `./gsb --accounts --accounts-live` | Print account status table (`ok`, `exhausted`, `expired`, `quarantined`, `cooling`, `degraded`, `disabled`) |
| `./gsb --rotate` | Rotate rotating cookies offline (use Web UI while daemon is running) |
| `./gsb --unquarantine <id>` | Clear quarantine state in an account file |
| `./gsb --models-json` | Print canonical `/v1/models` payload |

Common fixes:
- **`401 upstream_auth_error` / `code 1016`**: Google session expired — run `./gsb --login <name>` and `./gsb --probe` (`--rotate` cannot restore an expired sign-in).
- **`403 account_banned` / `quarantined`**: Account restricted upstream (`TOS_VIOLATION` / region / terms check) — sign in manually on `gemini.google.com` or stop retrying that account.
- **`429 rate_limit_error` vs `session_busy`**: `rate_limit_error` is upstream quota (check `/v1/quota` or pool key usage); `session_busy` is local queue saturation (`BRIDGE_MAX_CONCURRENT_PER_ACCOUNT`).
- **`404 model_not_found`**: Query `GET /v1/models` with the same key; catalogues vary by account tier.

---

## Development

```sh
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
go build -o gsb ./cmd/gsb          # or -tags nowebui to exclude /ui
```

See [`AGENTS.md`](./AGENTS.md) for architecture details and agent operating conventions.

---

## Acknowledgments

Special thanks to **[@PramaAditya](https://github.com/PramaAditya)** for identifying and fixing the missing `OSID` / `__Secure-OSID` session cookies on multi-account Chrome profiles in [Issue #11](https://github.com/azrinaldi/gemini-stealth-bridge/issues/11) and [PR #12](https://github.com/azrinaldi/gemini-stealth-bridge/pull/12)! 🙏

---

## License

[MIT](./LICENSE)
