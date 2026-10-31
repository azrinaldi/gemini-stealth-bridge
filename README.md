# gemini-stealth-bridge (`gsb`)

[![release](https://img.shields.io/github/v/release/azrinaldi/gemini-stealth-bridge?sort=semver)](https://github.com/azrinaldi/gemini-stealth-bridge/releases/latest)
[![release workflow](https://github.com/azrinaldi/gemini-stealth-bridge/actions/workflows/release.yml/badge.svg)](https://github.com/azrinaldi/gemini-stealth-bridge/actions/workflows/release.yml)
[![go](https://img.shields.io/github/go-mod/go-version/azrinaldi/gemini-stealth-bridge)](go.mod)
[![license](https://img.shields.io/github/license/azrinaldi/gemini-stealth-bridge)](LICENSE)
[![ghcr](https://img.shields.io/badge/ghcr.io-gemini--stealth--bridge-blue)](https://github.com/azrinaldi/gemini-stealth-bridge/pkgs/container/gemini-stealth-bridge)

OpenAI-compatible proxy for [Gemini Web](https://gemini.google.com/app). Point coding agents, routers, and OpenAI SDK clients at `gsb` and use your Gemini account through Chat Completions or Responses — with account pooling, quota-aware failover, native image/video generation, Google Search grounding, and a built-in MCP server.

![gsb Web Control Plane](docs/assets/web-ui.png)

`gsb` translates OpenAI-style messages, images, and tool definitions into Gemini Web requests, then converts the results back into JSON or Server-Sent Events (SSE). It authenticates with your Google session cookies, not a Gemini Developer API key. Model access and quotas depend on the connected account.

The daemon runs without headless Chrome or CDP. Browserless transport is an implementation detail; the API bridge is the interface your clients use.

> **Unofficial.** This is a bridge to the Gemini web app, not Google's supported Gemini Developer API or an OpenAI service. Changes to Google's web protocol, sign-in requirements, account restrictions, and quotas can interrupt operation. The transport does not guarantee protection from detection or account restrictions. Use only accounts you are authorized to access and review Google's applicable terms.

---

## Contents

1. [What `gsb` provides](#1-what-gsb-provides)
2. [Requirements](#2-requirements)
3. [Quick start](#3-quick-start)
4. [Core concepts](#4-core-concepts)
5. [Web Control Plane](#5-web-control-plane)
6. [Account onboarding](#6-account-onboarding)
7. [Client integration](#7-client-integration)
8. [API reference](#8-api-reference)
9. [Native image and video generation](#9-native-image-and-video-generation)
10. [Web search grounding](#10-web-search-grounding)
11. [Configuration reference](#11-configuration-reference)
12. [Data layout](#12-data-layout)
13. [Security](#13-security)
14. [Docker and deployment](#14-docker-and-deployment)
15. [Operations and troubleshooting](#15-operations-and-troubleshooting)
16. [How it works](#16-how-it-works)
17. [Development](#17-development)
18. [License](#18-license)

---

## 1. What `gsb` provides

### Capabilities

| Capability | Behavior |
|---|---|
| Chat Completions | `POST /v1/chat/completions`, JSON or SSE output |
| Responses | `POST /v1/responses`, JSON or SSE output, including `function_call`, `custom_tool_call`, `local_shell`, and namespaced tools |
| Function calling | Translates tool definitions into a prompt contract, validates returned arguments against the tool's JSON Schema, repairs common model mistakes, and emits OpenAI-style calls. The client executes the tools |
| Image input | Base64 data URLs, remote HTTPS images, and Anthropic-style `image` parts |
| Thinking effort | Maps `low`, `medium`, and `high` to Gemini Web thinking levels; accepts many client-specific field names |
| Conversation history | Reuses Gemini continuation tokens when matching history is available; offloads older turns into text attachments when the inline prompt budget is exceeded |
| Multiple accounts and quota | Pool key routes across accounts (`rotate`, `lru`, `fill-first`); per-account keys pin one account; group keys serve one user's accounts with failover inside the group. Exposes rolling **5-hour** and **7-day weekly** quota windows |
| Per-account proxies | Each account can egress through its own HTTP/SOCKS5 proxy, configured per account file, the Web UI, or `BRIDGE_EGRESS_PER_ACCOUNT` |
| Web search grounding | Extracts the Google Search citations Gemini Web returned and appends them to the answer as a numbered `Sources:` list |
| MCP server | Built-in Streamable HTTP MCP endpoint at `POST /mcp` (alias `/v1/mcp`) exposing grounding as a `web_search` tool for Claude Code, Cursor, OMP, and other MCP clients |
| Native image and video | Imagen / Nano Banana images and Veo MP4 video with audio through Chat, Coding Agent tool-calling (`generate_media`), `POST /v1/images/generations`, `POST /v1/videos/generations`, and the local `GET /v1/media/<id>` proxy |
| Web Control Plane | Embedded single-binary dashboard at `GET /ui` for account onboarding, quota bars, priority ordering, pause/quarantine actions, and runtime configuration |
| Session maintenance | Background cookie rotation, model/quota refresh, idle-session reaping, and persistence of updated credentials |
| Diagnostics | `X-GSB-*` response headers, request timing logs, redacted request dumps, and fingerprint comparison scripts |

### Compatibility boundaries

Compatibility covers the endpoints and fields documented here, not the entire OpenAI API:

- Sampling and output controls such as `temperature`, `top_p`, `max_tokens`, and `max_output_tokens` are **not forwarded** as generation controls. They may appear in model metadata for client compatibility, but they do not enforce output limits or change sampling.
- Responses clients must resend conversation history in `input`; `previous_response_id` is not a continuation mechanism. Response retrieval, deletion, and background jobs are not implemented.
- Function calling is prompt-based, not native Gemini Developer API function calling. Malformed or schema-invalid calls can fail with `tool_call_error`.
- Token usage is estimated locally, not reported by Google's billing/tokenizer API. The model registry advertises a 1,048,576-token context window and 65,536-token output limit as metadata, not a guarantee for every account or request.
- Audio input/output, embeddings, and the general OpenAI Files API are not exposed. Internal text-attachment offloading is not a general file-upload API.
- The daemon serves plain HTTP. It does not terminate TLS.

### What's new in v0.4.0

- **Web search grounding**: answers include a `Sources:` list; `BRIDGE_INCLUDE_GROUNDING=0` suppresses it.
- **Built-in MCP server**: `POST /mcp` exposes `web_search` with a per-call `timeout_seconds` deadline.
- **Per-account egress proxies**: `BRIDGE_EGRESS_PER_ACCOUNT` or the Web UI's *Outbound Proxy* field.
- **Group keys**: `BRIDGE_GROUP_TOKENS` + `BRIDGE_ACCOUNT_GROUP` for multi-user deployments.
- **Faster tool streaming**: the tool-envelope detector is a hand-rolled scan instead of a regexp over the whole accumulated answer.

Older release notes are on the [GitHub releases page](https://github.com/azrinaldi/gemini-stealth-bridge/releases).

---

## 2. Requirements

| Component | Requirement |
|---|---|
| Runtime | Linux, macOS, or Windows on `amd64` or `arm64`. Static binaries need no runtime dependencies |
| Build | **Go 1.26.0 or newer**, Git |
| Account | A Google account that can use [Gemini Web](https://gemini.google.com/app) |
| Browser (optional) | Chrome, Chromium, Brave, Edge, Vivaldi, or Arc for `--login` / `--import-chrome`; Firefox cookie databases are also readable |
| Docker (optional) | Any recent Docker Engine with Compose v2 |
| Scripts (optional) | `python3` for `scripts/check-router-metadata.sh` and `scripts/diff-fingerprint.sh`; POSIX `sh` |

Shell examples use a POSIX shell. On Windows, run the binary as `gsb.exe` and adapt environment-variable assignments to your shell.

---

## 3. Quick start

### 3.1 Install

Pick whichever path fits: a prebuilt binary, a Go install, or the container image.

**Prebuilt binary (recommended)** — every release publishes static binaries for Linux, macOS, and Windows on both `amd64` and `arm64`, plus `SHA256SUMS`:

```sh
# Linux x86_64; swap in darwin_arm64, linux_arm64, windows_amd64.exe, ...
curl -fL -o gsb https://github.com/azrinaldi/gemini-stealth-bridge/releases/latest/download/gsb_linux_amd64
chmod +x gsb
./gsb --version
```

Verify the download against the release checksums:

```sh
curl -fLO https://github.com/azrinaldi/gemini-stealth-bridge/releases/latest/download/SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing
```

**From source**:

```sh
git clone https://github.com/azrinaldi/gemini-stealth-bridge.git
cd gemini-stealth-bridge
go build -o gsb ./cmd/gsb
```

Or install into your Go binary directory:

```sh
go install github.com/azrinaldi/gemini-stealth-bridge/cmd/gsb@latest
```

**Container** — Docker is optional. If you only want the binary, you can use the image purely as an installer and never leave a container running:

```sh
cid=$(docker create ghcr.io/azrinaldi/gemini-stealth-bridge:latest)
docker cp "$cid:/usr/local/bin/gsb" ./gsb
docker rm "$cid"
```

See [Docker and deployment](#14-docker-and-deployment) for the full container setup.

### 3.2 Connect a Gemini account

Sign in through `gsb` so the session is captured in an isolated disposable browser profile that never conflicts with background cookie rotation:

```sh
./gsb --login acc1
./gsb --probe
```

`--login` opens a temporary Chrome/Chromium/Brave/Edge window with a disposable `--user-data-dir`, waits for Google sign-in, captures the cookies through CDP, verifies the session against Gemini Web, and deletes the temporary profile. No background browser process is left competing with `gsb` for `__Secure-1PSIDTS` rotation.

`--probe` prints the account's models and 5-hour/weekly quota. Treat a probe without an auth error as the setup success signal, not the mere existence of `accounts/<name>.json`.

### 3.3 Start the daemon

```sh
./start.sh
# or directly:
./gsb --web
```

The default address is `http://127.0.0.1:20133` and the Web Control Plane is at `http://127.0.0.1:20133/ui`. Keep this default local-only setup on loopback; see [Security](#13-security) before sharing access.

`start.sh` builds a missing or outdated binary when a Go toolchain is available, falls back to the existing binary when it is not, exports `BRIDGE_HOME` (default `~/.gemini-stealth-bridge`), enables the Web UI, and then `exec`s `gsb`.

### 3.4 Send a request

There are three kinds of key. Pick one:

| Key | Value | Behavior |
|---|---|---|
| **Pool key** | `BRIDGE_API_KEY` (default `public`) | Routes each request across all loaded accounts using `BRIDGE_ACCOUNT_STRATEGY`. Use this normally |
| **Group key** | `BRIDGE_GROUP_TOKENS` entry (`token:group`) | Serves only the accounts of its group and fails over inside that group. Use this in multi-user deployments |
| **Account key** | `gsb-<name>` or `<name>` | Pins the request to that account and never fails over. Use this to test or isolate one account |

List the models available to the pool:

```sh
curl -sS http://127.0.0.1:20133/v1/models \
  -H 'Authorization: Bearer public'
```

Set `MODEL` to an `id` returned by that endpoint. `gemini-flash` below is an example, not a promise of availability:

```sh
export MODEL=gemini-flash
curl -sS http://127.0.0.1:20133/v1/chat/completions \
  -H 'Authorization: Bearer public' \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with OK.\"}]}"
```

Read the answer from `choices[0].message.content`. `/health` checks that the daemon is running; only a successful model request verifies upstream generation.

`X-GSB-Account` in the response headers names the account that served the request, which is the quickest way to see the pool working. Replace `public` with `gsb-acc1` to pin the same request to one account.

---

## 4. Core concepts

### 4.1 Accounts

An account is one saved Google session under `$BRIDGE_HOME/accounts/<name>.json`. Account names accept 1–64 characters from `A-Z`, `a-z`, `0-9`, `.`, `_`, and `-`.

Each account has its own:

- cookie jar and rotating session cookies,
- live model list and quota state,
- transport session and optional egress proxy,
- failure/quarantine/disabled state,
- account group and priority position.

Accounts are deduplicated by email address. Multiple files for the same Google account do not create additional upstream quota; the freshest credential wins.

### 4.2 API keys

| | **Pool key** | **Group key** | **Account key** |
|---|---|---|---|
| Value | `BRIDGE_API_KEY` (default `public`) | A `BRIDGE_GROUP_TOKENS` entry (`token:group`) | `gsb-<name>` and `<name>` |
| Scope | All loaded accounts | The accounts assigned to one group via `BRIDGE_ACCOUNT_GROUP` | Exactly one account |
| Account selection | Automatic, per `BRIDGE_ACCOUNT_STRATEGY` | Automatic, inside the group only | None; the named account is always used |
| Failover on quota, ban, or failure | Yes | Yes, but only among the group's accounts | No; the error is returned to the caller |
| `/v1/accounts` and `/v1/quota` | All accounts | Only the group's accounts | Only the named account |
| Best for | Normal use, agents, routers | Multi-user deployments where each user has their own accounts | Testing one account, per-project quota isolation |

Notes:

- `BRIDGE_API_KEY=public` means the pool key is `public` and requests **without** a key are accepted. The Web UI also derives a stable `gsb-pool-<hash>` token from `BRIDGE_HOME` and accepts it as the pool key.
- When exactly one account is loaded, the pool key resolves to that account's scope rather than the admin/all-accounts scope.
- Account keys are registered automatically for every loaded account file, including the raw account name. They are predictable from the filename, so treat them as routing aliases, not secrets. Setting a pool key does not remove them.
- `BRIDGE_ACCOUNT_TOKENS` adds explicit `token:account_id` mappings. It does not remove the automatic aliases. When it is set, unknown tokens no longer trigger a hot reload of account files.
- `BRIDGE_GROUP_TOKENS` adds `token:group_name` mappings. A group key cannot reach accounts outside its group.
- `BRIDGE_ADMIN_KEY` grants the admin scope: all accounts, plus `/v1/admin/*`. A non-public `BRIDGE_API_KEY` also satisfies admin authentication when `BRIDGE_ADMIN_KEY` is empty.
- Accepted credential headers: `Authorization: Bearer <key>` (recommended), `x-api-key`, `x-goog-api-key`, and `?key=`. Query keys can leak into URL logs.

### 4.3 Pool routing strategies

`BRIDGE_ACCOUNT_STRATEGY` controls pool-key selection:

- **`rotate` (default)** — keep using one account until it is exhausted, banned, rate-limited, or failing, then advance to the next and wrap around. Best session continuity, and it leaves other accounts at full quota as standby. It does not jump back to an earlier account the moment that account recovers.
- **`lru`** — spread load to the least recently used healthy account. Ties prefer fewer recorded failures, then the oldest `LastUsed`.
- **`fill-first`** — drain a fixed order set by `BRIDGE_ACCOUNT_PRIORITY`, then load order. A recent failure demotes an account only for the 60-second cooldown, so a preferred account reclaims its rank as soon as it is healthy.

All strategies share the same eligibility rules. An account is skipped when it is:

- disabled (`Paused`) or quarantined,
- missing the requested model,
- known to have the requested model's quota exhausted (per `BRIDGE_QUOTA_PREFLIGHT`),
- in a synthetic model-substitution cooldown,
- at its per-account RPM or concurrency limit,
- in the 60-second failure cooldown.

An account that already holds the conversation's continuation cache is preferred, so a multi-turn conversation stays on one Gemini session even when the pool rotates.

Failover respects account groups. Once a request is being served by an account in a group, retries stay inside that group. A pool-key request that starts on an ungrouped account also stays on ungrouped accounts, because a grouped account is not a valid fallback for it.

### 4.4 Quota model

`gsb` queries two upstream sources:

- `batchexecute` RPC `qpEbW` (`CHECK_GEMINI_QUOTA`), and
- Gemini Web's GraphQL `GetUserQuotaService` pools `GXU_FIVE_HOURLY` and `GXU_WEEKLY`.

The resulting buckets are exposed in `/v1/accounts`, `/v1/quota`, `gsb --accounts`, and the Web UI:

| Window | Default credits | Meaning |
|---|---|---|
| `5h` | `2,400` | Rolling five-hour window |
| `weekly` | `48,384` | Seven-day weekly allowance |

The 5-hour window is nested under the weekly allowance, so `gsb` caps effective 5-hour remaining credits to the weekly remaining credits when the weekly pool runs lower.

When an account reaches `0` credits, non-lite models (`gemini-flash`, `gemini-pro`, `gemini-argon`) are blocked or downgraded upstream until `resetAt`, while `gemini-flash-lite` remains usable. `gsb` also records synthetic per-model cooldowns when upstream silently substitutes a model, escalating `1m → 5m → 15m → 1h → 6h → 24h` across repeated observations.

`quotaWindow` in `/v1/accounts` describes what the numbers cover, including `weeklyExposed`, `weeklyRemaining`, `weeklyTotal`, `nextResetAt`, and `nextResetInSeconds`.

### 4.5 Sessions and continuation cache

Gemini Web conversations are continued with internal metadata (`cid`, `rid`, `rcid`, context token). `gsb` hashes the conversation history, tools, and tool choice, then stores continuation metadata per `(account, model, history prefix)`:

- TTL: **30 minutes**
- Maximum entries: **256**
- Persisted to `$BRIDGE_HOME/session-cache.json`
- Disabled with `BRIDGE_SESSION_CACHE=0`
- Bypassed for `temporary: true` requests

Uploaded images and offloaded text attachments are cached separately for **25 minutes** (maximum 128 entries) so repeated turns do not re-upload the same bytes.

`BRIDGE_HISTORY_OFFLOAD=1` (default) uploads older conversation turns as text attachments when the inline prompt budget is exceeded. Setting it to `0` forces inline compaction instead.

### 4.6 Media cache

Generated images and videos are registered in memory and served through `GET /v1/media/<id>`:

- Maximum **96** entries, oldest evicted first
- Not persisted across restarts
- Served with `Cache-Control: public, max-age=86400` and `Access-Control-Allow-Origin: *`
- Unauthenticated, because upstream media URLs require Google cookies that browsers and `curl` do not have

---

## 5. Web Control Plane

Start `gsb` with `./start.sh`, `./gsb --web`, or `BRIDGE_WEB_UI=1` and open `http://127.0.0.1:20133/ui`.

The UI is a single embedded HTML/CSS/JS page compiled into the binary. It can be excluded at build time with `-tags nowebui`.

### 5.1 Access control

- **Loopback access with no admin key** is allowed.
- **Remote access** (any non-loopback peer, or a loopback peer with a non-loopback `X-Forwarded-For` / `X-Real-IP` / `CF-Connecting-IP` / `Forwarded` header) is locked with HTTP `403` while `BRIDGE_API_KEY` is `public` and `BRIDGE_ADMIN_KEY` is empty.
- When a key is configured, the page prompts for it. The key is kept in the tab's `sessionStorage` and sent as both `Authorization: Bearer` and `X-GSB-Admin-Key`.
- Five failed admin authentications from one IP within 60 seconds trigger HTTP `429`.
- Admin requests with `Sec-Fetch-Site: cross-site` are rejected.

This matters for Docker: bridge NAT makes every request look remote, so the control plane stays locked until `BRIDGE_ADMIN_KEY` or a non-public `BRIDGE_API_KEY` is set.

### 5.2 Accounts tab

- **Quick-connect cards**: Pool Base URL + pool key, per-account key chips (`gsb-<name>`), and the MCP endpoint URL/key. Every value is click-to-copy.
- **Pool telemetry**: current strategy, rotation cursor, active HTTP/2 sessions, and continuation cache hits/misses.
- **Filters**: All, Ready, Exhausted, Attention.
- **Per-account row**: status badge, email, 5-hour and weekly quota bars with reset countdowns, model list, RPM/stream telemetry, and a `Key:` chip.
- **Priority controls**: `↑` / `↓` move an account in `BRIDGE_ACCOUNT_PRIORITY`; the order is persisted to `config.env`.
- **`⋯` action menu**:
  - *Refresh Quota & Session* (`probe`)
  - *Pause Account in Pool* / *Resume Account in Pool* (`toggle`)
  - *Unquarantine Account* or *Reset Failure Counter* (`unquarantine`)
  - *Copy Key*
  - *Remove Account* (two-step confirmation)
- **Refresh All** queries upstream with `/v1/accounts?live=1`.

Status badges map the API statuses to UI labels: `ok` → Ready, `disabled` → Paused, `exhausted` → Exhausted, `cooling` → Cooling, `degraded` → Degraded, `quarantined` → Quarantined, `expired` → Expired, `unknown` → Unprobed.

### 5.3 Add Account

The **+ Add Account** dialog offers two isolated methods. Only the cookie box is visible by default; account name, proxy, and group live under **Optional settings**.

1. **Isolated browser window** (recommended): the daemon opens a disposable browser profile and auto-captures cookies on sign-in. On a headless VPS, the page falls back to the local helper at `http://127.0.0.1:20134` when the server reports no browser/GUI. Run `gsb --login-helper` on your laptop and forward both ports (`ssh -L 20133:127.0.0.1:20133 -L 20134:127.0.0.1:20134 user@vps`).
2. **Incognito session cookies** (works locally and over SSH):
   - Open a new Incognito window (`Ctrl+Shift+N`), sign in to `gemini.google.com/app`, export cookies with Cookie-Editor (or copy the `Cookie` header), then immediately close the Incognito window **without signing out**.
   - Paste the JSON array or raw header into the dialog.

Optional fields:

- **Account name** — leave blank to derive an ID from the Google email.
- **Outbound proxy** — per-account HTTP/SOCKS5 proxy; blank uses `BRIDGE_PROXY_SERVER`.
- **Account group** — only needed when you issue group keys; ignore it for a single pool key.

For an already-signed-in local browser profile, use the CLI: `gsb --import-chrome <name>`.

### 5.4 Runtime configuration tab

The UI can change these settings live and optionally persist them to `$BRIDGE_HOME/config.env` (mode `0600`):

| UI field | Environment variable | Allowed values |
|---|---|---|
| Account Strategy | `BRIDGE_ACCOUNT_STRATEGY` | `rotate`, `lru`, `fill-first` |
| Quota Preflight | `BRIDGE_QUOTA_PREFLIGHT` | `strict`, `advisory`, `off` |
| Account Priority | `BRIDGE_ACCOUNT_PRIORITY` | Comma-separated account IDs |
| Default Thinking | `BRIDGE_DEFAULT_THINKING` | `low`, `medium`, `high` |
| Max RPM per Account | `BRIDGE_MAX_RPM_PER_ACCOUNT` | `0`–`600` (`0` disables) |
| Max Concurrent Streams | `BRIDGE_MAX_CONCURRENT_PER_ACCOUNT` | `0`–`64` (`0` disables) |
| Reject Downgrade | `BRIDGE_REJECT_DOWNGRADE` | Boolean |
| Stream `reasoning_content` | `BRIDGE_LIVE_THOUGHTS` | Boolean |
| Stream `tool_calls` deltas | `BRIDGE_LIVE_TOOL_CALLS` | Boolean |
| Keep pre-tool preamble | `BRIDGE_TOOL_PREAMBLE` | Boolean |
| Offload long history | `BRIDGE_HISTORY_OFFLOAD` | Boolean |

**Apply In-Memory Only** changes the running daemon without writing a file. **Save to config.env** writes the values atomically. Values set in the process environment take precedence over `config.env` at the next start; the UI shows each field's source (`env`, `file`, `runtime`, or `default`).

Timeouts, proxy settings, keys, and transport fingerprint options are environment-only; they are not exposed in the UI.

### 5.5 Admin REST API

All admin routes require the admin key when one is configured, and are only mounted when the Web UI is enabled.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/admin/config` | Effective runtime config, field sources, pool key, rotation cursor, uptime, metrics |
| `PATCH` | `/v1/admin/config` | Update the 11 runtime fields above; `persist: true` writes `config.env` |
| `POST` | `/v1/admin/accounts/import` | Import an account from a cookie payload; body: `id`, `cookie`, `accountGroup`, `proxyUrl` |
| `POST` | `/v1/admin/accounts/browser-login` | Run an isolated browser login (4-minute timeout) and import the result |
| `DELETE` | `/v1/admin/accounts/<id>` | Remove the account file and unload it |
| `POST` | `/v1/admin/accounts/<id>/probe` | Bootstrap, refresh models/quota, clear failures on success |
| `POST` | `/v1/admin/accounts/<id>/rotate` | Rotate rotating cookies |
| `POST` | `/v1/admin/accounts/<id>/unquarantine` | Clear quarantine, failures, and synthetic cooldowns |
| `POST` | `/v1/admin/accounts/<id>/toggle` | Pause/resume the account in the pool |
| `POST` | `/v1/admin/accounts/<id>/move` | Reorder priority; body: `{"direction":"up"\|"down"}` |

---

## 6. Account onboarding

Each imported account is stored in `~/.gemini-stealth-bridge/accounts/<name>.json`, or under `$BRIDGE_HOME/accounts/` when configured. Cookie imports bootstrap the account against Gemini Web before reporting success. Treat both source cookie exports and saved account files as credentials.

### 6.1 Sign in with `--login` (recommended)

```sh
./gsb --login acc1
```

`--login` launches a temporary Chrome/Chromium/Brave/Edge window using a disposable `--user-data-dir`, opens `https://gemini.google.com/app`, and monitors cookies via CDP. As soon as Google sign-in completes and the `SAPISID` cookie is present, `gsb` captures the session, verifies it against Gemini Web, and deletes the temporary browser directory.

1. Run `./gsb --login <name>` and complete Google sign-in, including any 2FA or "verify it's you" prompt.
2. If operating on a headless server without a local browser, paste a `Cookie` header or Cookie-Editor JSON export from an **Incognito** window that you immediately close after copying.
3. Run `./gsb --probe` and confirm the account reports models and 5h/weekly quota without an auth error.

If an account later fails with an expired-session error, repeat `gsb --login <name>` instead of re-exporting stale cookies. `./gsb --rotate` only renews rotating cookies such as `__Secure-1PSIDTS`; it cannot restore a revoked or never-established sign-in.

### 6.2 Reuse a signed-in browser profile

Despite its name, `--import-chrome` also recognizes other Chromium browsers and Firefox cookie databases. No browser is launched during import. Use this only when that browser profile is already signed in to Gemini.

```sh
# Auto-detect a local cookie database.
./gsb --import-chrome acc1

# Choose a profile by name.
./gsb --import-chrome acc1 --chrome-profile "Profile 4"

# Choose a profile directory or cookie database explicitly.
./gsb --import-chrome acc1 --cookie-db "$HOME/.config/google-chrome/Default"
```

`--chrome-profile` also accepts a directory or cookie database path. `--cookie-db` takes precedence when both are supplied.

Supported browsers and typical paths:

| Browser | Linux | macOS | Windows |
|---|---|---|---|
| Chrome | `~/.config/google-chrome/Default` | `~/Library/Application Support/Google/Chrome/Default` | `%LOCALAPPDATA%\Google\Chrome\User Data\Default` |
| Chromium | `~/.config/chromium/Default` | `~/Library/Application Support/Chromium/Default` | `%LOCALAPPDATA%\Chromium\User Data\Default` |
| Brave | `~/.config/BraveSoftware/Brave-Browser/Default` | `~/Library/Application Support/BraveSoftware/Brave-Browser/Default` | `%LOCALAPPDATA%\BraveSoftware\Brave-Browser\User Data\Default` |
| Edge | `~/.config/microsoft-edge/Default` | `~/Library/Application Support/Microsoft Edge/Default` | `%LOCALAPPDATA%\Microsoft\Edge\User Data\Default` |
| Vivaldi | `~/.config/vivaldi/Default` | `~/Library/Application Support/Vivaldi/Default` | `%LOCALAPPDATA%\Vivaldi\User Data\Default` |
| Arc | — | `~/Library/Application Support/Arc/User Data` | MSIX package directory `%LOCALAPPDATA%\Packages\TheBrowserCompany.Arc_*\LocalCache\Local\Arc\User Data` |
| Firefox | `~/.mozilla/firefox/<profile>/cookies.sqlite` | `~/Library/Application Support/Firefox/Profiles/<profile>/cookies.sqlite` | `%APPDATA%\Mozilla\Firefox\Profiles\<profile>\cookies.sqlite` |

Arc is auto-detected on Windows through the MSIX glob. When no profile is given, auto-detection scans every known browser and prefers the first database that actually contains a `SAPISID` cookie, so an unsigned-in Chrome install does not shadow a signed-in Arc, Brave, or Edge profile.

```sh
# macOS Arc
./gsb --import-chrome acc1 --cookie-db "$HOME/Library/Application Support/Arc/User Data/Default"

# Or just name the browser and let gsb find the profile.
./gsb --import-chrome acc1 --chrome-profile arc
```

Decryption depends on the browser, OS, and access to the browser's encryption keys:

- **Linux**: keyring-backed cookies may require `secret-tool` and an unlocked desktop keyring.
- **macOS**: `gsb` queries the login Keychain for the matching `<Browser> Safe Storage` item (`Chrome Safe Storage`, `Arc Safe Storage`, `Brave Safe Storage`, …). macOS asks you to allow access the first time; `CHROME_PASS` overrides the lookup when Keychain access is unavailable.
- **Windows**: DPAPI decryption requires the same Windows user context; App-Bound (`v20`) encryption requires a manual export.

A detected database is not a guarantee that its cookies can be decrypted. `gsb` warns when the profile looks active or when long-lived session cookies are already expired or about to expire.

### 6.3 Cookie header or JSON export

Import either a raw `Cookie` header value or a Cookie-Editor JSON array:

```sh
./gsb --import-cookie acc1 --cookie-file /path/to/cookies.json
```

The same payload can be supplied on stdin:

```sh
./gsb --import-cookie acc1 < /path/to/cookies.json
```

`--cookie-value` accepts an inline payload, but puts credentials in shell history and potentially process listings. Prefer a protected file or stdin.

### 6.4 Remote VPS and headless servers

Recommended flow:

```sh
# On your laptop:
ssh -L 20133:127.0.0.1:20133 -L 20134:127.0.0.1:20134 user@vps

# On the VPS:
gsb --web
```

Then open `http://127.0.0.1:20133/ui` locally and use **+ Add Account**:

- **Incognito session cookies** (works everywhere), or
- **Isolated browser window → Launch** with `gsb --login-helper` running on the laptop.

`gsb --login-helper` listens on `127.0.0.1:20134` and exposes `POST /browser-login`. It allows CORS only from `http://localhost:*` and `http://127.0.0.1:*` origins.

### 6.5 Additional accounts

Repeat the login with another name:

```sh
./gsb --login acc2
```

Or, if that account is already signed in to a different browser profile:

```sh
./gsb --import-chrome acc2 --chrome-profile "Profile 2"
```

Each account has its own cookie state, model list, quota state, and transport session. Files belonging to the same Google account are deduplicated by email.

### 6.6 Account groups

Assign accounts to a group and map a key to that group:

```sh
BRIDGE_ACCOUNT_GROUP="acct-a:team-a,acct-b:team-a,acct-c:team-b" \
BRIDGE_GROUP_TOKENS="alice-key:team-a,bob-key:team-b" ./gsb
```

The group can also be set per account in the Web UI's *Account Group* field, or with `accountGroup` in the account file.

A group key:

- serves only that group's accounts,
- fails over only inside the group,
- sees only the group's accounts in `/v1/accounts` and `/v1/quota`,
- never consumes another group's quota.

### 6.7 Per-account egress proxies

```sh
BRIDGE_EGRESS_PER_ACCOUNT="acct-a:socks5://127.0.0.1:1081,acct-b:http://user:pass@proxy.example:3128" ./gsb
```

The same is available in the Web UI's *Outbound Proxy* field and as `proxyUrl` in the account file. The value applies to that account's transport session only; changing it rebuilds the session so no connection survives from the previous egress. Accounts without their own proxy use `BRIDGE_PROXY_SERVER`.

`BRIDGE_PROXY_SERVER` applies to Gemini transport, not to the separate remote-image downloader. It does not provide a process-wide proxy or privacy boundary.

---

## 7. Client integration

### 7.1 Direct connection

Configure any client that supports a custom OpenAI-compatible endpoint:

| Setting | Value |
|---|---|
| Base URL | `http://127.0.0.1:20133/v1` |
| API key | Pool key (`BRIDGE_API_KEY`, default `public`) for automatic rotation/failover, a group key, or `gsb-acc1` to pin `acc1` |
| API mode | Chat Completions or Responses, matching the client's adapter |
| Model | An ID from `GET /v1/models` using the same key |

Choose Chat Completions for clients that consume `choices` / `tool_calls`, or Responses for clients that consume `output` items and `response.*` events. Client support for reasoning fields varies. OpenAI compatibility does not imply that every coding agent or every client-specific feature has been tested.

### 7.2 Through `9router`

Add `gsb` as an OpenAI-compatible provider in `9router`:

| Provider setting | Value |
|---|---|
| Name | `Gemini Stealth Bridge` |
| Base URL | `http://127.0.0.1:20133/v1` |
| API type | Chat Completions (`chat`) or Responses (`responses`) |
| Prefix | `gsb` |
| Credential | Pool key (`BRIDGE_API_KEY`) to let `gsb` rotate accounts, or an account key (`gsb-acc1`) to pin one account |

Point the coding agent at the router's URL with the router's own API key; use the model name/prefix exposed by the router, for example `gsb/gemini-flash`. The bridge also accepts the `gsb/` model prefix directly, plus `google/`, `models/`, `vx/`, and `vertex/` prefixes and a `@think=<level>` suffix.

**Prefer one connection with the pool key.** Account rotation belongs to `gsb` because it is the only component that knows per-account quota, quarantine state, and conversation continuation affinity. A router that spreads requests across per-account connections cannot see any of that: it only learns about exhaustion from a `429`, and it will move the next turn to a different account, discarding the cached Gemini session. Use per-account connections only when you deliberately want to pin traffic to one account.

The base URL must be reachable **from the router process**. Inside a container, `127.0.0.1` refers to that container, not the host running `gsb`. Any non-loopback deployment needs the security measures in [Security](#13-security).

If your router caches or overrides model metadata, verify what survives:

```sh
# Check the bridge alone (all fields required)
./scripts/check-router-metadata.sh http://127.0.0.1:20133 public

# Compare the bridge against the router; reports exactly which fields the router dropped
./scripts/check-router-metadata.sh --compare \
  http://127.0.0.1:20133 http://127.0.0.1:20130 - sk-router-key
```

The compare form distinguishes a bridge problem from a router problem. A router that drops `capabilities.thinkingLevels` is why a harness cannot render a low/medium/high selector, and the script names that field rather than blaming the bridge.

### 7.3 MCP clients

`gsb` speaks MCP Streamable HTTP at `POST /mcp` (alias `/v1/mcp`) and exposes one tool, `web_search`.

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

The endpoint accepts the pool key, a group key, or an account key. See [MCP server](#811-mcp-server) for protocol details.

### 7.4 Coding agents

For agent workflows, point the agent at the Chat Completions or Responses base URL with the pool key. When the prompt asks for an image, diagram, or video, `gsb` injects the server-side `generate_media` tool so the agent can save generated media directly into the workspace. See [Native image and video generation](#9-native-image-and-video-generation).

---

## 8. API reference

### 8.1 Authentication

All `/v1/*` endpoints except `/v1/media/*` and `/v1/admin/*` use the same credential resolver. `/v1/admin/*` uses the admin key resolver. `/health`, `/readyz`, `/`, `/ui`, and `/media/*` have their own rules noted below; `/mcp` uses the API-key resolver.

Accepted forms:

```http
Authorization: Bearer <key>
x-api-key: <key>
x-goog-api-key: <key>
GET /v1/models?key=<key>
```

Admin routes additionally accept `X-GSB-Admin-Key`.

Failure shape for a bad API key:

```json
{
  "error": {
    "message": "invalid api key",
    "type": "authentication_error"
  }
}
```

Chat and Responses errors also include a `code` field. Simpler handlers such as `/v1/models`, `/v1/accounts`, `/v1/quota`, and the media proxy return only `message` and `type`.

### 8.2 Common response headers

Chat, Responses, and MCP responses can include:

| Header | Meaning |
|---|---|
| `X-GSB-Account` | Account ID that served the request |
| `X-GSB-Model` | Model ID used for the request |
| `X-GSB-Attempt` | 1-based attempt number |
| `X-GSB-Upstream-Status` | Last upstream HTTP status observed |
| `X-GSB-Retry-Reason` | Why the last retry/failover happened (`model_substitution`, `session_busy`, `rate_limit_error`, …) |
| `X-GSB-Duration-Ms` | MCP only: search duration |
| `X-GSB-Sources` | MCP only: number of citations |
| `Retry-After` | Seconds to wait when quota/rate limited |
| `X-Resets-At` | RFC 3339 quota reset timestamp, when known |

Chat and Responses stream headers flush as soon as the served model identity is verified. When downgrade rejection is disabled or `BRIDGE_STREAM_BUFFER=eager` is set, `BRIDGE_EARLY_SSE` and `BRIDGE_EARLY_SSE_GRACE_MS` can flush them earlier so long thinking turns do not hit proxy header timeouts.

### 8.3 Error format and codes

Non-streaming errors use HTTP status codes with the envelope above. Streaming requests can emit an error after HTTP 200; clients must inspect SSE events, not only the status line.

| Code | Typical HTTP | Meaning |
|---|---|---|
| `invalid_request_error` | 400 | Malformed body, tool schema, image, or model field |
| `authentication_error` | 401 | Invalid or missing API key |
| `auth_unavailable` | 503 | No accounts configured or loaded |
| `account_banned` | 403 | Upstream restricted the account (`TOS_VIOLATION` / `PERMISSION_DENIED`); account is quarantined |
| `upstream_auth_error` | 401 | Saved Google session expired or rejected |
| `auth_redirect` | 401 | Upstream redirected to Google sign-in |
| `model_not_found` | 404 | Account cannot serve the requested model |
| `model_substitution` | 429 | Upstream served a different model and downgrade rejection is enabled |
| `model_identity_unknown` | 502 | Upstream did not identify the served model |
| `model_identity_conflict` | 502 | Upstream reported conflicting served models |
| `rate_limit_error` | 429 | Quota, RPM, concurrency, or session-busy limit |
| `session_busy` | 429 | Transport session queue or wait timeout |
| `tool_call_error` | 502 | Tool call missing, malformed, or schema-invalid |
| `empty_upstream_response` | 502 | Upstream completed without an answer |
| `incomplete_upstream_response` | 502 | Upstream ended before the candidate completed |
| `upstream_error_reply` | 502 | Gemini returned a canned refusal/error text |
| `upstream_revision` | 502 | Upstream changed an answer or tool call after part of it was already streamed |
| `upstream_http_error` | 502+ | Upstream returned a non-2xx status |
| `upstream_error` | 502 | Generic upstream failure type, also used by the media proxy |
| `upstream_parse_error` | 502 | Malformed upstream frame |
| `upstream_callback_error` | 502 | A frame callback failed while processing the stream |
| `upstream_transport_error` | 502 | TLS/HTTP transport failure |
| `model_discovery_error` | 502 | Upstream model registry was malformed or unavailable |
| `upstream_connect_timeout` | 504 | First-byte timeout |
| `upstream_timeout` | 504 | Overall upstream timeout |
| `upstream_stalled` | 504 | No generation frame within `BRIDGE_STALL_TIMEOUT_MS` |
| `upstream_upload_error` | 502 | Attachment upload failed |
| `search_timeout` | 504 | MCP search deadline exceeded |
| `media_generation_failed` | 502 | No media returned by upstream |
| `not_found_error` | 404 | Unknown media ID or route |
| `client_cancelled` | 499 | The caller disconnected or cancelled the request |
| `server_error` | 500 | Local filesystem/state failure |
| `remote_web_ui_locked` | 403 | Remote control-plane access while no admin key is configured |
| `admin_auth_required` | 401 | Admin key required |
| `admin_auth_rate_limited` | 429 | Too many failed admin auth attempts |
| `cross_site_forbidden` | 403 | Cross-site admin request |

For a pool-key request, a `429` may say that no pool account can serve the requested model and summarize unavailable accounts by reason (`exhausted`, `substitution_cooldown`, `cooling`, `rate_limited`, `busy`, `model_unavailable`, `quarantined`). The message reports counts rather than emails or credentials; use `/v1/accounts?live=1` for per-account detail. Account-key requests keep the precise account-specific error.

### 8.4 Health and readiness

| Method | Path | Auth | Description |
|---|---|---|---|
| `GET` | `/health` | None | `{status, service, transport, profile, accounts, models, upstream}`; proves the daemon is running, not that upstream generation works |
| `GET` | `/` | None | `/health` when the Web UI is disabled; the UI when enabled |
| `GET` | `/readyz` | None | Per-account `models_fresh`, `quota_fresh`, `auth_ok`, `quarantined`; `?strict=1` returns `503` when no healthy account is available |

`/readyz` reports `degraded` when there are no accounts, no healthy account, any quarantined account, or fewer healthy accounts than loaded accounts. A model list is considered fresh for twice `BRIDGE_MODEL_REFRESH_MS`; quota is fresh for 30 minutes.

### 8.5 Models

```http
GET /v1/models
GET /v1/models?fresh=1
```

Returns an OpenAI-style list plus OpenRouter-style metadata:

- `id`, `display_name`, `description`, `aliases`
- `context_length`, `context_window`, `max_output_tokens`, `max_completion_tokens`
- `reasoning`, `supported_parameters`
- `architecture`, `top_provider`, `pricing`
- `thinking` (`mode`, `default`, `defaultLevel`, `efforts`, `requiresEffort`)
- `capabilities` (`vision`, `search`, `tools`, `reasoning`, `thinkingLevels`, `defaultThinkingLevel`, `limitsSource`, …)

Responses carry an `ETag`; send `If-None-Match` to get `304`. The HTTP response is cacheable for 60 seconds (`Cache-Control: max-age=60`), while the per-account upstream model registry is cached for 10 minutes and refreshed in the background every `BRIDGE_MODEL_REFRESH_MS`. `?fresh=1` forces live discovery.

Model IDs are whatever Gemini Web currently advertises, for example `gemini-flash`, `gemini-flash-lite`, `gemini-pro`, or `gemini-argon`. Aliases and display names are matched too. Prefixes `gsb/`, `google/`, `models/`, `vx/`, and `vertex/` are stripped before matching; a `@think=<level>` suffix sets thinking effort for that request.

`gsb --models-json` prints the same payload without starting the daemon. With no loaded accounts it prints a static fallback list.

### 8.6 Accounts, quota, sessions, refresh

| Method | Path | Description |
|---|---|---|
| `GET` | `/v1/accounts` | Account IDs, emails, computed `status`/`statusReason`, failures, timestamps, provenance, quarantine/disabled state, models, quota buckets, `quotaWindow`, load telemetry. `?live=1` or `?fresh=1` refreshes upstream first |
| `GET` | `/v1/quota` | Cached quota buckets; `?fresh=1` queries upstream. A cold cache triggers a refresh automatically |
| `GET` | `/v1/sessions` | Transport session status (`ready`, `busy`, `queued`, `active`, `stale`, `idleSec`) plus continuation cache hits/misses |
| `POST` | `/v1/refresh` | Reloads credential files and refreshes the model registry. Rejects requests carrying an `Origin` header |

Account objects include:

```json
{
  "id": "acc1",
  "email": "user@example.com",
  "status": "ok",
  "statusReason": "",
  "capacity": 2,
  "fails": 0,
  "lastError": "",
  "lastFailAt": "",
  "models": ["gemini-flash", "gemini-pro"],
  "quota": [{"id": "5h", "label": "5h Window", "total": 2400, "remaining": 2310, "resetAt": "…"}],
  "quotaWindow": {"scope": "rolling", "label": "5h", "nestedUnderWeekly": true, "weeklyExposed": true},
  "capturedAt": "…",
  "rotatedAt": "…",
  "modelsAt": "…",
  "quotaAt": "…",
  "lastUsedAt": "…",
  "quarantined": false,
  "quarantineReason": "",
  "disabled": false,
  "accountGroup": "team-a",
  "importedFrom": "web-ui",
  "profileId": "cookie-export",
  "proxyConfigured": true,
  "activeStreams": 0,
  "recentRpm": 0,
  "peakRpm": 1,
  "totalRequests": 12,
  "offloadRequests": 0,
  "surfaces": {"geminiWeb": "ok"}
}
```

### 8.7 Chat Completions

```http
POST /v1/chat/completions
Content-Type: application/json
```

Minimal request:

```json
{
  "model": "gemini-flash",
  "messages": [{ "role": "user", "content": "Reply with OK." }]
}
```

Accepted fields:

| Field | Notes |
|---|---|
| `model` | Optional; empty uses the first model on the selected account. Supports router prefixes and `@think=` |
| `messages` | Required, non-empty array. Roles: `system`, `developer`, `user`, `assistant`, `tool`, `function` |
| `stream` | Boolean; SSE when true |
| `tools` | Array of `{"type":"function","function":{name,description,parameters}}`. Tool names match `^[A-Za-z0-9_.:-]{1,128}$`; duplicates are rejected |
| `tool_choice` | `auto`, `none`, `required`, or `{"type":"function","function":{"name":"…"}}` |
| `temporary` | Boolean; requests a temporary Gemini conversation and skips continuation-cache reuse |
| `reasoning_effort` | `low`, `medium`, `high` (also numeric aliases) |
| `thinking_level`, `thinkingLevel` | Aliases for the effort |
| `reasoning` | Object: `enabled`, `effort`, `level` |
| `thinking` | Object: `enabled`, `type`, `effort`, `level`, `thinkingLevel`, `budget_tokens` |
| `thinking_budget` | Token budget mapped to `low` ≤4096, `medium` ≤16384, `high` above |
| `thinkingConfig`, `generationConfig.thinkingConfig` | `thinkingLevel` or `thinkingBudget` |
| `output_config` | `effort` |

Sampling parameters such as `temperature`, `top_p`, `max_tokens`, and `stop` are accepted but ignored.

Message content can be a string or an array of parts. Supported image shapes:

```json
{ "type": "image_url", "image_url": { "url": "data:image/png;base64,..." } }
{ "type": "input_image", "image_url": "https://example.com/image.png" }
{ "type": "image", "data": "<base64>", "mimeType": "image/png" }
```

Remote images are limited to 20 MiB, follow at most 5 redirects, and reject private/loopback destinations unless `BRIDGE_ALLOW_PRIVATE_IMAGE_URLS=1`.

The total request body is limited to 32 MiB.

#### Streaming Chat

Use `curl -N` to avoid buffering:

```sh
curl -N -sS http://127.0.0.1:20133/v1/chat/completions \
  -H 'Authorization: Bearer gsb-acc1' \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"stream\":true,\"reasoning_effort\":\"high\",\"messages\":[{\"role\":\"user\",\"content\":\"Explain how this bridge works.\"}]}"
```

Wire format:

- `data: {"object":"chat.completion.chunk", ...}` events
- a first role-only delta (`{"role":"assistant","content":""}`)
- text in `choices[0].delta.content`
- reasoning in `choices[0].delta.reasoning_content` when extracted and enabled
- tool calls in `choices[0].delta.tool_calls`
- a final chunk with `finish_reason` (`stop` or `tool_calls`) and `usage`
- `data: [DONE]`
- `: ping` comments every 15 seconds

`usage` is included on the final chunk regardless of `stream_options`.

Streaming behavior:

- The first client-visible delta is held until the served model identity is verified when downgrade rejection is enabled; `BRIDGE_STREAM_BUFFER=eager` or `BRIDGE_REJECT_DOWNGRADE=0` relaxes that.
- With tools enabled, prose answers longer than 240 bytes stream live once identity is verified. Mid-answer tool envelopes and fenced look-alikes stay out of the streamed prefix until parsing commits the turn; quoted envelope examples pass through as text.
- `BRIDGE_LIVE_TOOL_CALLS=false` (default) buffers and validates the complete tool call before emitting it; `true` emits incremental deltas and cannot repair a late upstream revision.
- `BRIDGE_LIVE_THOUGHTS=false` suppresses `reasoning_content` without changing the upstream thinking level.
- Errors can arrive after HTTP 200 as `data: {"error":{...}}`; inspect the stream, not just the status code.

#### Non-streaming Chat

```json
{
  "id": "chatcmpl-…",
  "object": "chat.completion",
  "created": 1760000000,
  "model": "gemini-flash",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "OK",
        "reasoning_content": "…",
        "tool_calls": []
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 12,
    "completion_tokens": 3,
    "total_tokens": 15,
    "prompt_tokens_details": { "cached_tokens": 0 }
  }
}
```

If the served model differs from the requested one, the payload also includes `requested_model` and `model_served`.

#### Function calling behavior

Function calling is prompt-based, not native Gemini function calling. `gsb` renders the tool definitions into the prompt, asks the model for a JSON tool-call envelope, parses it, validates it against the tool's JSON Schema, and emits OpenAI-style calls. The client executes the tools.

Validation covers `type` (including unions), `enum`, `const`, local `$ref`, string length/pattern, numeric bounds and `multipleOf`, array bounds and `uniqueItems`, object `required`/`additionalProperties`, and `allOf`/`anyOf`/`oneOf`/`not`/`nullable`.

Before validation, `gsb` coerces common model mistakes into the declared types (`"42"` → `42`, `"true"` → `true`, JSON-encoded objects/arrays in strings, scalar → single-element array). It also strips stray closing braces/brackets, removes trailing commas, quotes bare identifier keys, repairs string escapes, and retries truncated envelopes or invented refusals.

A schema-invalid call returns `tool_call_error`. To force a function, use `tool_choice: {"type":"function","function":{"name":"get_status"}}` for Chat or `{"type":"function","name":"get_status"}` for Responses. The bridge returns calls; it never executes your functions. Append the assistant call and matching tool result to the next request's history.

### 8.8 Responses

```http
POST /v1/responses
Content-Type: application/json
```

```json
{
  "model": "gemini-flash",
  "instructions": "Keep answers concise.",
  "input": "Explain how this bridge works."
}
```

`input` accepts:

- a string,
- an array of items (`message`, `function_call`, `function_call_output`, `custom_tool_call`, `custom_tool_call_output`, `reasoning`, `input_text`, `input_image`, `image`),
- or `messages` as a fallback.

`instructions` becomes a system message. `tools` accepts flat Responses definitions (`{type:"function", name, description, parameters}`), nested Chat-style definitions, `custom` tools, `local_shell`, and `namespace` wrappers. `tool_choice` accepts `auto`, `none`, `required`, or `{type:"function", name}`.

For multi-turn calls, resend the conversation in `input`, including assistant output and any `function_call` / `function_call_output` items. The bridge's internal continuation cache is separate from OpenAI's `previous_response_id` feature.

Output items:

- `reasoning` with a `summary` array
- `message` with `output_text` content
- `function_call` with `call_id`, `name`, `arguments`
- `custom_tool_call` with `call_id`, `name`, `input`

#### Streaming Responses

Set `stream: true`. The wire format is named SSE events:

```
event: response.created
data: {"type":"response.created","response":{...},"sequence_number":1}
```

Events include:

- `response.created`
- `response.in_progress`
- `response.output_item.added`
- `response.content_part.added`
- `response.output_text.delta`
- `response.output_text.done`
- `response.content_part.done`
- `response.reasoning_summary_part.added`
- `response.reasoning_summary_text.delta`
- `response.reasoning_summary_text.done`
- `response.function_call_arguments.delta`
- `response.function_call_arguments.done`
- `response.custom_tool_call_input.delta`
- `response.custom_tool_call_input.done`
- `response.output_item.done`
- `response.completed`

The stream ends with `response.completed`; there is no `[DONE]` marker. `: ping` comments are sent every 15 seconds.

### 8.9 Images and videos

#### `POST /v1/images/generations`

```sh
curl -sS http://127.0.0.1:20133/v1/images/generations \
  -H 'Authorization: Bearer public' \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Minimalist flat vector architecture diagram of an API bridge in light mode","aspect_ratio":"16:9","format":"png","response_format":"url"}'
```

Fields:

| Field | Notes |
|---|---|
| `prompt` | Required |
| `model` | Optional. Empty, `dall-e*`, `gpt-image*`, `imagen*`, and `veo*` map to `gemini-pro` |
| `response_format` | `url` (default) or `b64_json` |
| `size` | Parsed as `WxH` and snapped to the nearest supported ratio (e.g. `1280x720` → `16:9`); `auto` is accepted and ignored |
| `aspect_ratio` | `1:1`, `16:9`, `9:16`, `4:3`, `3:4` (also `square`, `landscape`, `portrait`, or any `W:H`, snapped to the nearest supported ratio) |

`size` and `aspect_ratio` must agree when both are sent; a conflict or a malformed value returns `400 invalid_request_error`. The generator decides the final dimensions, so the response reports the *produced* `width`, `height`, `size`, and the ratio they resolve to rather than echoing the request.
| `format` / `output_format` | `png` (lossless `=s0-rp`), `jpeg` (original `=s0-d`), or `gif` |

Response:

```json
{
  "created": 1760000000,
  "data": [
    {
      "url": "http://127.0.0.1:20133/v1/media/<id>.png",
      "revised_prompt": "…",
      "mime_type": "image/png",
      "aspect_ratio": "16:9",
      "width": 1376,
      "height": 768,
      "size": "1376x768"
    }
  ]
}
```

`b64_json` is added when requested and the bytes are available.

Media URLs are built from the request's `Host`/`X-Forwarded-Host` and scheme/`X-Forwarded-Proto`, so a reverse-proxied client receives a reachable URL rather than a hard-coded loopback address.

#### `POST /v1/videos/generations`

```sh
curl -sS --max-time 240 http://127.0.0.1:20133/v1/videos/generations \
  -H 'Authorization: Bearer public' \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"A glowing neon paper airplane flying smoothly through a rainy cyberpunk alley at night","aspect_ratio":"16:9","duration":10}'
```

Returns Veo MP4 with audio plus `thumbnail_url`. Video generation is slow; give the client a generous timeout (measured runs are commonly 60–150 seconds).

### 8.10 Media proxy

```http
GET /v1/media/<id>
GET /media/<id>
```

Serves cached generated images (`.jpg`, `.png`, `.gif`) and videos (`.mp4`). This endpoint is **unauthenticated** and sends `Access-Control-Allow-Origin: *`, because the upstream URLs require Google cookies. IDs are derived from the account and upstream URL and are not guessable secrets.

Unknown or evicted IDs return `404`. Media is cached in memory only; restarting `gsb` invalidates old URLs.

### 8.11 MCP server

```http
POST /mcp
POST /v1/mcp
```

Protocol: MCP Streamable HTTP over JSON-RPC 2.0, protocol version `2025-06-18` (the server echoes the client's requested version on `initialize`).

Behavior:

- `POST` only; `GET` returns `405` because there is no server-initiated stream.
- `OPTIONS` returns CORS preflight headers.
- Notifications (messages without an `id`) return HTTP `202` with an empty body.
- Request body limit: 8 MiB.
- Methods: `initialize`, `ping`, `tools/list`, `tools/call`.
- `tools/list` returns one tool, `web_search`.
- Unknown methods return JSON-RPC `-32601`; invalid params `-32602`; parse errors `-32700`.
- JSON-RPC errors are returned with HTTP `200`; check the `error` object.

`web_search` arguments:

| Argument | Required | Notes |
|---|---|---|
| `query` | Yes | Search query or question |
| `model` | No | `gemini-flash`, `gemini-flash-lite`, `gemini-pro`, `gemini-argon` |
| `timeout_seconds` | No | 10–600 seconds; defaults to `BRIDGE_SEARCH_TIMEOUT_MS` (180 s) |

A search runs as a temporary chat on the same account pool, quota accounting, and failover as normal requests. It does not add threads to the user's Gemini history. The result text is the synthesized answer followed by a `Sources:` list; snippets are included when Gemini provided them.

A grounded answer is a full model turn, not a search-API lookup. Measured searches on a live pool took **14–110 seconds**, so give the client a generous timeout. Cutting a search short throws away the quota it already spent.

```sh
curl -sS --max-time 240 http://127.0.0.1:20133/mcp \
  -H 'Authorization: Bearer public' \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"latest Go release notes","timeout_seconds":180}}}'
```

### 8.12 Endpoint index

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/` | None | Health JSON, or the Web UI when enabled |
| `GET` | `/dashboard` | UI gate | Alias for `/ui` |
| `GET` | `/ui`, `/ui/` | UI gate | Embedded Web Control Plane |
| `GET` | `/health` | None | Liveness and counts |
| `GET` | `/readyz` | None | Per-account readiness; `?strict=1` |
| `GET` | `/v1/models`, `/models` | API key | Model catalog with metadata and ETag |
| `POST` | `/v1/chat/completions`, `/chat/completions` | API key | Chat Completions |
| `POST` | `/v1/responses`, `/responses` | API key | Responses |
| `POST` | `/v1/images/generations`, `/images/generations` | API key | Image generation |
| `POST` | `/v1/videos/generations`, `/videos/generations` | API key | Video generation |
| `GET`/`HEAD` | `/v1/media/<id>`, `/media/<id>` | None | Generated media proxy |
| `POST` | `/mcp`, `/v1/mcp` | API key | MCP Streamable HTTP |
| `GET` | `/v1/accounts`, `/accounts` | API key | Account state; `?live=1` |
| `GET` | `/v1/quota`, `/quota` | API key | Quota buckets; `?fresh=1` |
| `GET` | `/v1/sessions`, `/sessions` | API key | Transport and cache status |
| `POST` | `/v1/refresh`, `/refresh` | API key | Reload credentials and models |
| `*` | `/v1/admin/*` | Admin key | Control-plane API (UI enabled only) |

Unknown routes return `404` with `invalid_request_error`.

---

## 9. Native image and video generation

`gsb` exposes Gemini Web's native image generation (**Nano Banana / Imagen**) and video generation (**Veo MP4 with audio**) across three surfaces. Because upstream `lh3.googleusercontent.com` and `contribution.usercontent.google.com` media URLs require authenticated Google session cookies, `gsb` automatically registers and serves generated media through `GET /v1/media/<id>`.

### 9.1 Coding Agent Mode (`tools` enabled)

When a coding agent (`OpenCode`, `OMP`, `Cursor`, `Cline`, etc.) sends a Chat Completions or Responses request with `tools`, and the prompt asks for an image, diagram, illustration, or video, `gsb` injects a server-side tool named `generate_media`. Non-media coding turns are left untouched so `SessionCache` hits are preserved.

Injection happens only when:

- the request has at least one client tool and `tool_choice` is `auto` or unset, and
- the last three user messages contain explicit media intent, or the history already contains a `generate_media` call.

Supported `generate_media` parameters:

| Parameter | Type | Notes |
|---|---|---|
| `media_type` | `"image"` \| `"video"` | Required |
| `prompt` | string | Required; detailed visual description |
| `aspect_ratio` | `"1:1"`, `"16:9"`, `"9:16"`, `"4:3"`, `"3:4"` | Optional |
| `format` | `"png"`, `"jpeg"`, `"gif"` | `png` uses lossless `=s0-rp`, `jpeg` uses original `=s0-d` |
| `duration` | `10`, `15`, `30` | Video only |
| `output_path` | string | Optional relative filename in the workspace |

When the model invokes `generate_media` (or Gemini Web's native generator returns media directly on a tool-enabled turn):

1. `gsb` runs the generation on Gemini Web and downloads full-resolution media (`=s0-d` / `=s0-rp`), following both HTTP redirects and application-level `text/plain` URL redirects.
2. `gsb` caches and serves the file at `http://127.0.0.1:20133/v1/media/<id>.png|.jpg|.mp4`.
3. `gsb` feeds a tool result back into a continuation turn with `url`, `urls`, `revised_prompt`, `markdown`, `suggested_filename`, and a ready-to-run `local_download_cmd` (`curl`), so the coding agent can save the file into the user's workspace.

If generation fails on the selected account, a pool or group request retries on other eligible accounts.

### 9.2 Direct Chat / Responses (without tools)

Ask for an image or video in a normal chat request; `gsb` enables Gemini Web's media extensions and rewrites returned Markdown URLs to local `/v1/media/<id>` URLs. The rewritten URL opens or downloads without Google cookies.

### 9.3 Dedicated OpenAI media endpoints

Use `POST /v1/images/generations` and `POST /v1/videos/generations` as documented in [Images and videos](#89-images-and-videos). These endpoints run a dedicated generation turn, do not require client tools, and return OpenAI-style `data` arrays.

---

## 10. Web search grounding

When Gemini Web answers with Google Search grounding, `gsb` extracts the citations and appends them to the answer:

```
<answer text>

---
Sources:
1. Article title — https://example.com/article
2. Another page — https://example.com/other
```

- `BRIDGE_INCLUDE_GROUNDING=0` returns the answer without the citation list.
- The MCP `web_search` result uses the same `Sources:` block and additionally includes a snippet line for each source when Gemini provided one.
- Citations are merged across streaming frames and deduplicated.
- Grounding does not require a special request flag; it is whatever Gemini Web decided to do for that turn.

The built-in MCP server is documented in [MCP server](#811-mcp-server).

---

## 11. Configuration reference

### 11.1 Precedence and files

Configuration is read from:

1. process environment variables,
2. `$BRIDGE_HOME/config.env` (dotenv-style `KEY=VALUE`; `export ` prefixes and surrounding quotes are accepted),
3. built-in defaults.

`$BRIDGE_HOME/config.env` is written atomically with mode `0600` by the Web UI and admin API. `HOST`/`PORT` are accepted as fallbacks when `BRIDGE_HOST`/`BRIDGE_PORT` are empty.

Boolean values accept `1`, `true`, `yes`, `on` and `0`, `false`, `no`, `off`.

Duration values are milliseconds unless the variable name says otherwise.

`gsb --check-config` validates the environment and exits.

### 11.2 Listener, home, and identity

| Variable | Default | Meaning |
|---|---|---|
| `HOST` / `BRIDGE_HOST` | `127.0.0.1` | Listen address |
| `PORT` / `BRIDGE_PORT` | `20133` | Listen port |
| `BRIDGE_HOME` | `~/.gemini-stealth-bridge` | Root for account files, `config.env`, and continuation cache |
| `BRIDGE_API_KEY` | `public` | Pool key. `public` also permits unauthenticated requests |
| `BRIDGE_ADMIN_KEY` | Empty | Dedicated admin key for `/v1/admin/*` and remote `/ui`. When empty, a non-public `BRIDGE_API_KEY` is used as the admin key |
| `BRIDGE_ACCOUNT_TOKENS` | Empty | `token:account_id` pairs for custom account keys |
| `BRIDGE_GROUP_TOKENS` | Empty | `token:group_name` pairs for group keys |
| `BRIDGE_ACCOUNT_GROUP` | Empty | `account_id:group_name` assignments |
| `BRIDGE_WEB_UI` | `false` (`1` in `start.sh` and Docker) | Enable `/ui` and `/v1/admin/*` |
| `WEB_CRED_FILE` | `$BRIDGE_HOME/web-credentials.json` | Parsed for compatibility but not used by the credential loader. The loader always reads `$BRIDGE_HOME/web-credentials.json` and every `$BRIDGE_HOME/accounts/*.json` |

### 11.3 Credentials and onboarding

| Variable | Default | Meaning |
|---|---|---|
| `CHROME_COOKIES` | Empty | Browser cookie database/profile hint for import/login; also enables a live-cookie fallback when no saved accounts load |
| `CHROME_PASS` | Empty | Browser cookie-encryption password override (not your Google password); treat as a secret |
| `BRIDGE_LOCALE` | `en` | Gemini language and `Accept-Language` base |
| `BRIDGE_PROTOCOL_MODE` | `bridge` | Payload shape: `bridge` or `web`. `web` changes the consumer-tool and personal-context flags for tool-free requests |
| `BRIDGE_PERSONAL_CONTEXT` | `auto` | Personal context flags: `auto` (off in `bridge`, on in pure `web`), `on`, `off` |
| `BRIDGE_BL_OVERRIDE` | Empty | Raw `bl` token override for diagnostics; logged when used |

### 11.4 Egress and transport

| Variable | Default | Meaning |
|---|---|---|
| `BRIDGE_PROXY_SERVER` | Empty | Outbound HTTP or SOCKS5 proxy URL for Gemini transport, used by accounts without their own proxy |
| `BRIDGE_EGRESS_PER_ACCOUNT` | Empty | `account_id:proxy_url` pairs; overrides `BRIDGE_PROXY_SERVER` per account |
| `BRIDGE_IMPERSONATE` | `chrome_150_psk` | uTLS profile. See [TLS profiles](#115-tls-profiles) |
| `BRIDGE_UA_OS` | Host OS | Override the OS string in User-Agent / `Sec-CH-UA*`: `linux`, `windows`, `macos` |
| `BRIDGE_TLS_EXT_ORDER` | `fixed` | TLS extension order: `fixed` or `random` |
| `BRIDGE_FOLLOW_REDIRECTS` | `true` | Follow upstream HTTP redirects |
| `BRIDGE_IPV6` | `auto` | `auto`, `off`, or `on` |
| `BRIDGE_DISABLE_IPV6` | Empty | `1` sets the default IPv6 mode to `off` when `BRIDGE_IPV6` is unset |
| `BRIDGE_ALLOW_PRIVATE_IMAGE_URLS` | Disabled | Exactly `1` permits private/loopback image destinations; weakens SSRF protection |
| `BRIDGE_MAX_SESSIONS` | `16` | Transport session pool limit |
| `BRIDGE_MAX_QUEUE` | `4` | Per-session wait-queue bound |
| `BRIDGE_SESSION_WAIT_MS` | `10000` | Wait timeout for an available transport session |
| `BRIDGE_IDLE_SESSION_MS` | `180000` | Idle transport-session expiry |

### 11.5 TLS profiles

`BRIDGE_IMPERSONATE` accepts:

`chrome_152_psk`, `chrome_152`, `chrome_150_psk` (default; aliases `chrome150_psk`, `chrome_latest`, empty), `chrome_150`, `chrome_146_psk`, `chrome_146`, `chrome_144_psk`, `chrome_144`, `chrome_133_psk`, `chrome_133`, `chrome_131_psk`, `chrome_131`, `brave_146_psk` / `brave`, `firefox_148` / `firefox`, `firefox_147_psk`, `safari_16_0` / `safari`.

An unknown value falls back to `chrome_150_psk`. User-Agent and `Sec-CH-UA*` version numbers are derived from the profile name.

### 11.6 Routing and quota

| Variable | Default | Meaning |
|---|---|---|
| `BRIDGE_ACCOUNT_STRATEGY` | `rotate` | `rotate`, `lru`, or `fill-first` |
| `BRIDGE_ACCOUNT_PRIORITY` | Empty | Comma-separated account IDs for `fill-first` and UI order |
| `BRIDGE_QUOTA_PREFLIGHT` | `strict` | `strict` skips spent accounts, `advisory` logs and proceeds, `off` disables the check. With `BRIDGE_REJECT_DOWNGRADE=0`, `strict` is also skipped: quota is only enforced by the upstream response |
| `BRIDGE_MAX_RPM_PER_ACCOUNT` | `24` | Per-account requests per rolling minute; `0` disables |
| `BRIDGE_MAX_CONCURRENT_PER_ACCOUNT` | `2` | Per-account concurrent active streams; `0` disables |
| `BRIDGE_REJECT_DOWNGRADE` | `true` | Reject detected upstream model substitution instead of silently returning another model |
| `BRIDGE_QUIET_HOURS` | Empty | `HH:MM-HH:MM` window in which background rotation/model refresh pauses (requests still work) |

### 11.7 Generation and history

| Variable | Default | Meaning |
|---|---|---|
| `BRIDGE_DEFAULT_THINKING` | `low` | Effort applied when a request sends none: `low`, `medium`, `high` |
| `BRIDGE_LIVE_THOUGHTS` | `true` | Include separately extracted reasoning in API output |
| `BRIDGE_TEMPORARY` | `false` | Request temporary Gemini conversations; skips continuation-cache reuse |
| `BRIDGE_SESSION_CACHE` | `true` | Persist and reuse conversation continuation metadata |
| `BRIDGE_TOOL_PREAMBLE` | `false` | Preserve assistant text before tool calls as answer text instead of moving it into reasoning |
| `BRIDGE_MAX_PROMPT_CHARS` | `850000` | Inline history budget in bytes before older turns are offloaded; not a token count |
| `BRIDGE_MAX_OLD_TOOL_CHARS` | `32000` | Compaction threshold for older tool-result messages |
| `BRIDGE_HISTORY_OFFLOAD` | `true` | Upload older turns as text attachments when inline budget is exceeded; `false` forces inline compaction |
| `BRIDGE_INCLUDE_GROUNDING` | `true` | Append Google Search citations to answers as a `Sources:` list |
| `BRIDGE_CONTEXT_WINDOW` | `1048576` | Fallback context window for models Google has not published limits for |
| `BRIDGE_MAX_OUTPUT_TOKENS` | `65536` | Fallback output limit, used on the same terms |
| `BRIDGE_TOKEN_ESTIMATOR` | `script` | `script` (CJK-aware) or `bytes4` (~4 UTF-8 bytes per token) |

Models with documented limits always use the documented value; `capabilities.limitsSource` reports `documented` or `config`.

### 11.8 Tools and streaming

| Variable | Default | Meaning |
|---|---|---|
| `BRIDGE_LIVE_TOOL_CALLS` | `false` | Emit incremental tool-call deltas instead of buffering and validating the complete call |
| `BRIDGE_TOOL_SCHEMA_COMPACT` | `true` | Compact tool schemas in the prompt contract |
| `BRIDGE_TOOL_DESC_MAX_CHARS` | `400` | Maximum tool-description length in the compact contract (minimum 64) |
| `BRIDGE_STREAM_BUFFER` | `verified` | `verified` buffers until model identity is verified; `eager` streams earlier |
| `BRIDGE_EARLY_SSE` | `false` | Flush streaming headers immediately when downgrade rejection is disabled or `BRIDGE_STREAM_BUFFER=eager` |
| `BRIDGE_EARLY_SSE_GRACE_MS` | `20000` | Under the same conditions, flush headers after this long even if the first frame has not arrived; `0` keeps them buffered |
| `BRIDGE_MAX_ATTEMPTS` | `3` | Total attempts per request, including failover (1–10) |
| `BRIDGE_MAX_UPSTREAM_ATTEMPTS` | `3` | Maximum upstream calls per request (1–10) |
| `BRIDGE_RETRY_BUDGET_MS` | `150000` | Total wall-clock budget for retries; `0` disables the deadline |

### 11.9 Timeouts and background work

| Variable | Default | Meaning |
|---|---|---|
| `BRIDGE_FIRST_BYTE_TIMEOUT_MS` | `60000` | Initial upstream response timeout |
| `BRIDGE_STALL_TIMEOUT_MS` | `90000` | Timeout between upstream generation frames |
| `BRIDGE_STREAM_TIMEOUT_MS` | `600000` | Overall request timeout |
| `BRIDGE_ROTATE_COOKIES_MS` | `540000` | Background cookie rotation interval, clamped to 60 s–3600 s. Idle accounts rotate at half frequency |
| `BRIDGE_MODEL_REFRESH_MS` | `1800000` | Model/quota refresh interval |
| `BRIDGE_SEARCH_TIMEOUT_MS` | `180000` | Default MCP `web_search` deadline; clamped to 10 s–600 s |

### 11.10 Diagnostics

| Variable | Default | Meaning |
|---|---|---|
| `BRIDGE_LOG_REQUESTS` | `false` | Log one JSON line per request with timings, byte counts, cache/attempt telemetry |
| `BRIDGE_DUMP_REQUEST` | Empty | Directory for redacted outbound request dumps (header order, cookie names and hashes, body SHA-256) |

`gsb --capture-reference` writes `$BRIDGE_HOME/reference-fingerprint.json`. Compare a live dump against it with:

```sh
BRIDGE_DUMP_REQUEST=/tmp/gsb-dumps ./gsb --probe
./scripts/diff-fingerprint.sh /tmp/gsb-dumps/req-*.json ~/.gemini-stealth-bridge/reference-fingerprint.json
```

### 11.11 Environment-only settings

These are **not** editable from the Web UI: listener, keys, home directory, proxies, TLS/fingerprint options, timeouts, session limits, prompt budgets, logging/dump paths, and quiet hours. Change them in the process environment or `config.env`, then restart.

---

## 12. Data layout

```text
$BRIDGE_HOME/                     # default ~/.gemini-stealth-bridge
├── .lock                         # single-writer lock (pid + start time)
├── config.env                    # UI/admin-persisted runtime settings, mode 0600
├── session-cache.json            # continuation metadata, mode 0600
├── reference-fingerprint.json    # only after --capture-reference
├── web-credentials.json          # legacy single-account credential file
└── accounts/
    ├── acc1.json                 # one Google session
    └── acc2.json
```

Account file fields include:

`kind`, `name`, `email`, `cookies`, `at`, `bl`, `fSid`, `capturedAt`, `rotatedAt`, `cookieDb`, `importedFrom`, `profileId`, `tokenFetchedAt`, `tokenSource`, `lastReqId`, `quarantined`, `quarantineReason`, `disabled`, `accountGroup`, `proxyUrl`, `surfaces`.

Account files and `session-cache.json` contain credentials or sensitive continuation metadata. They are written with mode `0600` on POSIX systems. Protect the whole data directory with OS permissions and exclude it from version control and shared backups. On Windows, use filesystem ACLs rather than relying on POSIX mode bits.

Only one daemon may use a `BRIDGE_HOME` at a time. A second instance exits with a lock error naming the owning PID.

One-shot commands behave differently:

- `--login`, `--login-helper`, `--import-chrome`, `--import-cookie`, `--accounts`, `--accounts-live`, `--models-json`, `--unquarantine`, `--check-config`, and `--capture-reference` run without taking the daemon lock.
- `--probe` tries to take the lock but proceeds when it is already held, so it can inspect a running daemon's data directory.
- `--rotate` takes the lock and fails when the daemon is already running. Use the Web UI or `POST /v1/admin/accounts/<id>/rotate` for a live daemon.

---

## 13. Security

The daemon listens on loopback by default. **`BRIDGE_API_KEY` defaults to `public`, and unauthenticated access is allowed in that mode.**

### 13.1 Exposure checklist

- Set `BRIDGE_API_KEY` to a random value before exposing the daemon beyond loopback. Account keys keep working alongside it.
- Set `BRIDGE_ADMIN_KEY` (or a non-public pool key) before opening `/ui` remotely. Docker bridge NAT always looks remote.
- The daemon serves plain HTTP, not HTTPS. Use a private network, an SSH tunnel, or an authenticated TLS reverse proxy that restricts access independently of routing keys.
- Do not publish port `20133` directly to an untrusted network.
- Startup warns about a non-loopback bind with default authentication; it does **not** refuse to start.
- `/health`, `/readyz`, and `/v1/media/*` are reachable without an API key. `/mcp` uses the same API-key resolver as `/v1/*`, so it always requires a valid pool, group, or account key.
- `/v1/refresh` rejects requests carrying an `Origin` header to reduce cross-site request forgery.
- The UI sends `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, and a restrictive CSP.
- Admin authentication is rate-limited to five failures per IP per 60 seconds.
- Remote-image fetching blocks private, loopback, link-local, multicast, and unspecified addresses unless `BRIDGE_ALLOW_PRIVATE_IMAGE_URLS=1`. Request bodies are capped at 32 MiB; MCP bodies at 8 MiB; remote images at 20 MiB.
- `BRIDGE_PROXY_SERVER` is not a process-wide proxy and does not protect the remote-image downloader.

### 13.2 Credential handling

- Treat cookie exports, `Cookie` headers, account JSON, and `session-cache.json` as credentials.
- Do not paste them into chat, issues, logs, or commit messages.
- Do not commit `$BRIDGE_HOME` to version control.
- Account keys are derived from filenames and are not secrets. Use a pool key or group key when access control matters.
- `BRIDGE_DUMP_REQUEST` redacts cookie values, but the dump still reveals header order and cookie names; keep it private.

---

## 14. Docker and deployment

Docker is **optional**. The published image is multi-stage and zero-CGO (about 12 MB, non-root `uid 10001`, Web UI enabled on `:20133`). `docker compose up` pulls it from GHCR, so no Go toolchain and no local build is needed:

```sh
docker compose up -d
```

To build from this checkout instead:

```sh
BRIDGE_IMAGE=gemini-stealth-bridge:dev docker compose up -d --build
```

### 14.1 Compose defaults

`docker-compose.yml` sets:

| Setting | Value |
|---|---|
| Image | `${BRIDGE_IMAGE:-ghcr.io/azrinaldi/gemini-stealth-bridge:latest}` |
| Container user | `${BRIDGE_UID:-10001}:${BRIDGE_GID:-10001}` |
| Port binding | `${BRIDGE_BIND_IP:-127.0.0.1}:${BRIDGE_PORT:-20133}:20133` |
| `BRIDGE_HOME` | `/data` |
| `BRIDGE_HOST` / `BRIDGE_PORT` | `0.0.0.0` / `20133` |
| `BRIDGE_WEB_UI` | `1` |
| `BRIDGE_API_KEY` | `${BRIDGE_API_KEY:-public}` |
| `BRIDGE_ADMIN_KEY` | `${BRIDGE_ADMIN_KEY:-}` |
| `BRIDGE_DEFAULT_THINKING` | `${BRIDGE_DEFAULT_THINKING:-low}` |
| `BRIDGE_ACCOUNT_STRATEGY` | `${BRIDGE_ACCOUNT_STRATEGY:-rotate}` |
| `BRIDGE_REJECT_DOWNGRADE` | `${BRIDGE_REJECT_DOWNGRADE:-1}` |
| Volume | `${BRIDGE_DATA_DIR:-gsb-data}:/data` |

The named volume `gsb-data` is pre-owned by the container user.

### 14.2 Bind mounts

To keep data on the host, either `chown` the directory to `10001:10001` or run the container as your own UID:

```sh
mkdir -p data

# option A: give the directory to the container user
sudo chown -R 10001:10001 data
BRIDGE_DATA_DIR=./data docker compose up -d

# option B: FUSE / NTFS / exFAT mounts that reject chown -> run as your own UID
BRIDGE_DATA_DIR=./data BRIDGE_UID=$(id -u) BRIDGE_GID=$(id -g) docker compose up -d
```

### 14.3 Docker auth

Because Docker's bridge NAT makes every request look remote, the control plane stays locked (HTTP `403`) while `BRIDGE_API_KEY` is the default `public`. Set an admin key before opening it:

```sh
BRIDGE_ADMIN_KEY=$(openssl rand -hex 24) docker compose up -d
# then open http://127.0.0.1:20133/ui and paste that key into the auth prompt
```

Or with `docker run` bound to loopback:

```sh
docker run -d --name gsb \
  -p 127.0.0.1:20133:20133 \
  -v gsb-data:/data \
  -e BRIDGE_ADMIN_KEY="$(openssl rand -hex 24)" \
  ghcr.io/azrinaldi/gemini-stealth-bridge:latest
```

Open `http://127.0.0.1:20133/ui` (or forward the port via `ssh -L 20133:127.0.0.1:20133 user@server`) to add accounts and configure the pool.

### 14.4 Image details

- Build: `golang:1.26-alpine`, `CGO_ENABLED=0`, `-trimpath`, stripped symbols.
- Runtime: `alpine:3.21` with CA certificates and tzdata.
- Entrypoint: `/usr/local/bin/gsb`.
- Healthcheck: `gsb --healthcheck` every 30 s.
- Published tags: `X.Y.Z`, `X.Y`, and `latest` on GHCR.
- `VERSION` build arg is injected into `internal/config.Version`.

### 14.5 Binary release artifacts

Every `v*` tag publishes:

`gsb_linux_amd64`, `gsb_linux_arm64`, `gsb_darwin_amd64`, `gsb_darwin_arm64`, `gsb_windows_amd64.exe`, `gsb_windows_arm64.exe`, and `SHA256SUMS`.

The release workflow runs `go vet ./...`, `go test ./...`, and `go test -race ./...` before building.

---

## 15. Operations and troubleshooting

### 15.1 Daily checks

```sh
./gsb --help
./gsb --version
./gsb --check-config
./gsb --probe
./gsb --accounts              # read saved account files
./gsb --accounts --accounts-live   # query upstream for fresh models and quota
./gsb --rotate                # renew rotating cookies
./gsb --models-json           # print the canonical /v1/models payload
curl -sS http://127.0.0.1:20133/health
curl -sS http://127.0.0.1:20133/readyz
curl -sS http://127.0.0.1:20133/v1/quota \
  -H 'Authorization: Bearer gsb-acc1'
curl -sS -X POST http://127.0.0.1:20133/v1/refresh \
  -H 'Authorization: Bearer gsb-acc1'
```

`--accounts` prints one row per loaded account with the same computed status the HTTP API reports, plus 5-hour and weekly quota columns when available.

`--probe` bootstraps configured accounts and prints model/quota details. Inspect per-account errors, not just the command's exit status. A banned account is quarantined and persisted.

`--rotate` attempts cookie renewal, but cannot restore a revoked Google session. Re-import credentials when sign-in is required.

`--unquarantine <id>` clears quarantine state in the account file. While the daemon is running, prefer the Web UI or `POST /v1/admin/accounts/<id>/unquarantine`, which updates memory and disk together.

Run `./gsb` directly for predictable environment handling. `./start.sh` is a convenience wrapper that builds when needed, exports `BRIDGE_HOME`, enables the Web UI, and `exec`s the daemon.

### 15.2 Account status semantics

Statuses are computed in this precedence order:

| Status | UI label | Meaning |
|---|---|---|
| `disabled` | Paused | Operator paused the account in the control plane |
| `quarantined` | Quarantined | Upstream restricted the account (`TOS_VIOLATION`, `PERMISSION_DENIED`) |
| `expired` | Expired | Saved session is no longer valid; run `gsb --login <name>` |
| `exhausted` | Exhausted | A real quota window is spent until `resetAt`, or a model stayed exhausted across retries |
| `cooling` | Cooling | Failed within the last 60 seconds; temporarily skipped |
| `degraded` | Degraded | Cooldown elapsed, but the failure counter has not been cleared by a success |
| `unknown` | Unprobed | No quota check has run in this process |
| `ok` | Ready | Loaded, authenticated, no known limit |

The CLI statuses are the same strings (`ok`, `exhausted`, `expired`, `quarantined`, `cooling`, `degraded`, `disabled`, `unknown`).

### 15.3 Common failures

| Symptom | What to check |
|---|---|
| `503 auth_unavailable` / no accounts configured | Connect an account with `gsb --login <name>`; ensure the daemon and the import use the same `BRIDGE_HOME`; reload with `/v1/refresh` or restart |
| `401 authentication_error` | Confirm the key: pool key is `BRIDGE_API_KEY`, group key is a `BRIDGE_GROUP_TOKENS` entry, account keys are `gsb-<name>` or `<name>`. Check the account filename and whether the account is loaded |
| Cookie import/decryption failure | Run `gsb --login <name>` instead; cookie import only works when that browser profile is already signed in. If decryption itself fails, unlock the keyring, set `CHROME_PASS`, or use a cookie export |
| Bootstrap/sign-in failure | Open Gemini Web with the same account, complete any Google sign-in checks, then re-import |
| `401 upstream_auth_error` / `code 1016` on a previously working account | The saved session expired. Run `gsb --login <name>`, complete sign-in, then `gsb --probe`. Re-exporting the same browser cookies will not help |
| `403 account_banned` / `TOS_VIOLATION` | The account is restricted upstream. Do not retry in a loop; the account is quarantined. Review the account and Google's terms |
| `404 model_not_found` | Query `/v1/models` with the same key; do not assume another account's model is available |
| `429` quota/rate-limit error | Inspect `/v1/quota`; wait for `resetAt` or choose an account/model with available quota. Check `Retry-After` and `X-Resets-At` |
| `429 model_substitution` | Gemini served a different model and downgrade rejection is enabled; retry after checking quota or explicitly select an available model |
| `429 session_busy` | Per-account concurrency or queue is saturated; raise `BRIDGE_MAX_CONCURRENT_PER_ACCOUNT` / `BRIDGE_MAX_QUEUE` or add accounts |
| Tool-call validation error | Check the tool schema and `tool_choice`; the generated call must match the requested contract. Schema coercion repairs stringified scalars, but not missing required fields |
| Stream appears stalled or fails | Check daemon logs, upstream connectivity/proxy, client buffering, and client/reverse-proxy timeouts. Raise `BRIDGE_STALL_TIMEOUT_MS` or `BRIDGE_STREAM_TIMEOUT_MS` if the model legitimately thinks longer |
| Reasoning appears in normal chat output | Check the client's reasoning-field support; model-authored inline reasoning may not be extracted |
| UI shows `403 remote_web_ui_locked` | Set `BRIDGE_ADMIN_KEY` or a non-public `BRIDGE_API_KEY`, or use an SSH tunnel |
| UI shows `401` after a restart | The key is stored in `sessionStorage`; re-enter it in the Access Key dialog |
| Media URL returns `404` | The in-memory entry was evicted or the daemon restarted; regenerate the media |
| `503` from `/readyz?strict=1` | No account has fresh models and no auth error; run `gsb --accounts --accounts-live` |

### 15.4 Streaming errors

SSE requests can emit an error after HTTP headers have been sent. Clients must inspect stream events, not only the HTTP status. Chat errors are sent as `data: {"error":{...}}`; Responses errors use the same envelope within the event stream.

### 15.5 Logs

With `BRIDGE_LOG_REQUESTS=1`, `gsb` logs one JSON line per request containing endpoint, model, account, effort, prompt byte breakdown, TTFT, total time, status, error code, cache hit, attempts, upload time, identity time, and account switches. Cookie values and prompt bodies are not logged.

---

## 16. How it works

```mermaid
flowchart LR
    C[OpenAI-compatible client] -->|Chat Completions or Responses| B[gsb API bridge]
    B -->|Authenticated web requests| G[Gemini Web]
    G -->|Generation frames| B
    B -->|JSON or SSE| C
```

The request path has four parts:

1. **Resolve scope and model.** Authenticate the caller, choose the eligible account set (pool, group, or single account), resolve the requested model and thinking effort, and check the tool contract and known quota.
2. **Normalize the conversation.** Flatten messages, images, thinking effort, and tools into the Gemini Web prompt. Reuse cached continuation metadata where possible; upload older history as text attachments when necessary.
3. **Send upstream.** Use a per-account uTLS/HTTP2 transport with ordered browser headers. The default `Chrome_150_PSK` profile replaces a headless browser process; it does not emulate every property of a real browser.
4. **Parse and format.** Parse Gemini's generation frames, separate reasoning and tool calls, validate the result, and emit the selected OpenAI-compatible response format.

Background workers:

- refresh model and quota state every `BRIDGE_MODEL_REFRESH_MS`,
- rotate rotating cookies every `BRIDGE_ROTATE_COOKIES_MS`,
- reap idle transport sessions every 60 seconds,
- persist updated cookies to account files,
- persist continuation metadata to `session-cache.json`.

Retries and failover happen before client-visible output is committed. Once text or tool deltas have been sent, the answer is committed and a retry would duplicate or contradict it, so only in-stream-retryable failures are retried at that point.

---

## 17. Development

### 17.1 Build and test

```sh
go build -o gsb ./cmd/gsb
go vet ./...
go test ./...
go test -race ./...
```

Build without the embedded Web UI:

```sh
go build -tags nowebui -o gsb ./cmd/gsb
```

`-tags nowebui` makes `GET /ui` return `404` with `webui_compiled_out`.

### 17.2 CLI reference

| Flag | Purpose |
|---|---|
| `--login <name>` | Interactive isolated-browser sign-in; saves `accounts/<name>.json` |
| `--login-helper` | Run the local desktop login helper on `127.0.0.1:20134` for remote VPS use |
| `--import-chrome <name>` | Import cookies from a local browser profile or cookie database |
| `--import-cookie <name>` | Import a raw `Cookie` header or Cookie-Editor JSON |
| `--chrome-profile <name\|dir\|db>` | Browser profile name, directory, or Cookies SQLite path |
| `--cookie-db <path>` | Explicit cookie database or profile directory; takes precedence over `--chrome-profile` |
| `--cookie-value <payload>` | Inline cookie payload (avoid; leaks into shell history) |
| `--cookie-file <path>` | File containing a raw cookie header or JSON export |
| `--probe` | Bootstrap all accounts, verify models/quota, print details, exit |
| `--rotate` | Run cookie rotation on all accounts and exit |
| `--accounts` | Print a status table for every loaded account and exit |
| `--accounts-live` | With `--accounts`, query upstream for fresh models/quota first. Used alone, it also prints the table with live data |
| `--unquarantine <id>` | Clear quarantine state in the account file and exit |
| `--models-json` | Print the canonical `/v1/models` JSON payload and exit |
| `--capture-reference` | Write a reference browser fingerprint JSON to `$BRIDGE_HOME` |
| `--check-config` | Validate environment configuration and exit |
| `--healthcheck` | Probe local `/health` and exit 0/1 (container healthcheck) |
| `--web` | Enable `/ui` and `/v1/admin/*` |
| `--version` | Print build version, OS/arch, and Go version |

### 17.3 Scripts

| Script | Purpose |
|---|---|
| `scripts/check-router-metadata.sh <base-url> [key]` | Verify that `/v1/models` carries context limits, output limits, and thinking metadata |
| `scripts/check-router-metadata.sh --compare <bridge> <router> [bridge-key] [router-key]` | Report exactly which metadata fields the router dropped |
| `scripts/diff-fingerprint.sh <dump.json> <reference.json>` | Compare a live request dump against the reference fingerprint; reports missing/extra headers, order mismatches, and value diffs |
| `start.sh` | Build if needed, export `BRIDGE_HOME`, enable the UI, and start the daemon |

### 17.4 Repository map

| Path | Responsibility |
|---|---|
| [`cmd/gsb`](./cmd/gsb) | CLI, imports/login, daemon startup and shutdown |
| [`internal/server`](./internal/server) | API routing, authentication, account selection, response formatting, continuation cache, media registry, MCP, admin API |
| [`internal/gemini`](./internal/gemini) | Gemini Web authentication, model/quota discovery, uploads, generation and frame parsing, grounding |
| [`internal/prompt`](./internal/prompt) | History normalization, text offloading, image handling, usage estimation |
| [`internal/toolbridge`](./internal/toolbridge) | Prompt-based function-call contract, JSON Schema validation/coercion, call parsing, streaming, media tool |
| [`internal/cookies`](./internal/cookies) | Cookie import/decryption, persistence, authentication headers, rotation, isolated browser login |
| [`internal/transport`](./internal/transport) | uTLS/HTTP2 client, account sessions, queues, proxies, timeouts |
| [`internal/config`](./internal/config) | Environment configuration, `config.env` persistence, browser header profiles |
| [`internal/webui`](./internal/webui) | Embedded single-file Web Control Plane, with `-tags nowebui` stub |
| [`docs/assets`](./docs/assets) | Documentation images |
| [`.github/workflows/release.yml`](./.github/workflows/release.yml) | Tests, six-target binary release, GHCR image publish |

[`AGENTS.md`](./AGENTS.md) is a complete operations manual for AI coding agents. Its main rule: connect an account with `gsb --login`, and treat cookie import as a fallback for browser profiles that are already signed in.

When reporting a bug, include the endpoint, model ID, HTTP/error code, OS, Go version, and redacted daemon logs. Remove cookies, API keys, email addresses, prompts, and tool outputs containing private data.

---

## 18. License

[MIT](./LICENSE).
