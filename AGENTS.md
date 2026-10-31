# Agent instructions for `gemini-stealth-bridge` (`gsb`)

This file is the operating manual for AI coding agents that help a user install, configure, integrate, debug, or operate `gsb`. It is written to be complete on its own: every capability, endpoint, environment variable, status value, and failure mode an agent is likely to need is documented here.

Read this file top to bottom before answering a non-trivial `gsb` question. If a fact is not here, check `README.md` and then the code — do not guess.

---

## Contents

1. [Operating principles](#1-operating-principles)
2. [What the product is](#2-what-the-product-is)
3. [Mental model: accounts, keys, and routing](#3-mental-model-accounts-keys-and-routing)
4. [Account onboarding](#4-account-onboarding)
5. [Never recommend](#5-never-recommend)
6. [Diagnosing auth and account failures](#6-diagnosing-auth-and-account-failures)
7. [Pool routing and quota](#7-pool-routing-and-quota)
8. [Client integration](#8-client-integration)
9. [API surface agents must know](#9-api-surface-agents-must-know)
10. [Function calling and tool schemas](#10-function-calling-and-tool-schemas)
11. [Images and native media generation](#11-images-and-native-media-generation)
12. [Web search grounding and MCP](#12-web-search-grounding-and-mcp)
13. [Per-account proxies and groups](#13-per-account-proxies-and-groups)
14. [Web Control Plane and admin API](#14-web-control-plane-and-admin-api)
15. [Configuration reference](#15-configuration-reference)
16. [Data layout and credentials](#16-data-layout-and-credentials)
17. [Security rules](#17-security-rules)
18. [Deployment patterns](#18-deployment-patterns)
19. [Troubleshooting playbook](#19-troubleshooting-playbook)
20. [Answer templates](#20-answer-templates)

---

## 1. Operating principles

1. **Prefer `--login` over cookie copying.** The only reliable way to create a session is to sign in through `gsb --login <name>` (or the Web UI's isolated browser). Cookie import is a fallback for browser profiles that are **already signed in**.
2. **Default to the pool key.** The pool key is the only key that fails over across every account and keeps conversation continuity. Account keys are for testing or pinning.
3. **Never put credentials in a message, log, commit, issue, or command that will be echoed.** Cookies, `Cookie` headers, account JSON, and `session-cache.json` are credentials.
4. **Verify before declaring success.** A saved `accounts/<name>.json` is not a working account. `gsb --probe` or a real model request is.
5. **Do not invent OpenAI compatibility.** Sampling controls are ignored; `previous_response_id` is not supported; token usage is estimated. Say so when relevant.
6. **Do not treat a quota number as a guarantee.** 5-hour and weekly windows are upstream values that change without notice.
7. **Check which key the client used before blaming routing.** Most "it failed while another account had quota" reports are account-key requests, which never fail over.
8. **When a command can affect a live daemon, prefer the API over editing files.** The daemon holds state in memory; direct file edits can be overwritten.
9. **Keep the user's data directory intact.** Do not delete or overwrite `$BRIDGE_HOME` without explicit confirmation.
10. **Quote exact status/error codes.** `exhausted`, `cooling`, `expired`, `quarantined`, `degraded`, `disabled`, `unknown` mean different things and imply different fixes.

---

## 2. What the product is

`gsb` is an OpenAI-compatible HTTP daemon that proxies to **Gemini Web** (`gemini.google.com`) using Google session cookies. It is **not**:

- Google's Gemini Developer API,
- an OpenAI service,
- a browser automation tool (no headless Chrome or CDP at runtime),
- a guarantee against detection or account restriction.

It provides:

| Surface | Endpoint |
|---|---|
| Chat Completions | `POST /v1/chat/completions` (JSON or SSE) |
| Responses | `POST /v1/responses` (JSON or SSE) |
| Image generation | `POST /v1/images/generations` |
| Video generation | `POST /v1/videos/generations` |
| Media proxy | `GET /v1/media/<id>` |
| MCP Streamable HTTP | `POST /mcp`, `POST /v1/mcp` |
| Model catalog | `GET /v1/models` |
| Account/quota/session state | `GET /v1/accounts`, `/v1/quota`, `/v1/sessions` |
| Refresh | `POST /v1/refresh` |
| Web Control Plane | `GET /ui` |
| Admin API | `/v1/admin/*` (UI enabled only) |
| Health | `GET /health`, `GET /readyz` |

Default listener: `127.0.0.1:20133`. Default pool key: `public` (unauthenticated).

---

## 3. Mental model: accounts, keys, and routing

### 3.1 Three kinds of key

| Key | Value | Scope | Failover |
|---|---|---|---|
| **Pool key** | `BRIDGE_API_KEY` (default `public`) | Every loaded account | Yes, across the pool |
| **Group key** | `BRIDGE_GROUP_TOKENS` entry (`token:group`) | The accounts in one group | Yes, inside the group only |
| **Account key** | `gsb-<name>` or `<name>` | Exactly one account | No |

Default to the pool key in every example and client configuration.

The pool key is the only key that fails over across every account. An account key stays on that account and returns `429` or `401` instead of switching. A group key fails over, but only among the accounts of its own group.

When a user reports that a request failed while another account had quota, **check which key the client used** before assuming a routing bug.

### 3.2 Key resolution details

- `BRIDGE_API_KEY=public` accepts requests with **no** key, the literal `public`, or the derived `gsb-pool-<hash>` token shown in the Web UI.
- With exactly one loaded account, the pool key resolves to that account's scope.
- Account aliases (`gsb-<name>` and `<name>`) are registered automatically for every loaded account file. They are predictable from filenames and are not secrets.
- `BRIDGE_ACCOUNT_TOKENS="token:account_id"` adds custom aliases without removing the automatic ones. It also disables the on-demand hot reload of newly added account files for unknown `gsb-*` tokens.
- `BRIDGE_ADMIN_KEY` grants the admin scope: all accounts, plus `/v1/admin/*`.
- Accepted headers: `Authorization: Bearer`, `x-api-key`, `x-goog-api-key`, `?key=`. Prefer `Authorization`.

### 3.3 Routing strategies

| Strategy | Behavior | When to use |
|---|---|---|
| `rotate` (default) | Stick to one account until it is exhausted/banned/rate-limited/failing, then advance and wrap | Best session continuity; standby accounts keep full quota |
| `lru` | Spread load to the least recently used healthy account | Many small independent requests |
| `fill-first` | Drain a fixed order (`BRIDGE_ACCOUNT_PRIORITY`, then load order) | A preferred account should reclaim traffic as soon as it is healthy |

All strategies skip:

- disabled (`Paused`) and quarantined accounts,
- accounts missing the requested model,
- accounts with the requested model's quota known exhausted (per `BRIDGE_QUOTA_PREFLIGHT`),
- accounts in a synthetic model-substitution cooldown,
- accounts at their RPM or concurrency limit,
- accounts that failed within the last 60 seconds.

All strategies prefer the account holding the conversation's continuation cache. Failover never crosses a group boundary.

`rotate` does **not** jump back to an earlier account the moment it recovers. If a user expects traffic to return to a preferred account immediately after a quota reset, that is `fill-first`, not `rotate`.

---

## 4. Account onboarding

### 4.1 Recommended: `--login`

```sh
gsb --login <name>
gsb --probe
```

`--login` launches a temporary Chrome/Chromium/Brave/Edge window with a disposable `--user-data-dir`, opens `https://gemini.google.com/app`, waits for Google sign-in (including 2FA), captures cookies via CDP, verifies the session against Gemini Web, and deletes the temporary profile.

Why this is the recommended path: the disposable profile is destroyed after capture, so no background browser process competes with `gsb` for `__Secure-1PSIDTS` cookie rotation.

If no local browser or display is available, `--login` falls back to a terminal prompt: open Gemini in an Incognito window, paste the `Cookie` header or Cookie-Editor JSON, then close the window.

### 4.2 Remote VPS / headless

Two supported paths:

1. **Incognito cookie paste (works everywhere)**
   - Open a **New Incognito Window** (`Ctrl+Shift+N`).
   - Sign in to `gemini.google.com/app`.
   - Export cookies with Cookie-Editor (or copy the `Cookie` header).
   - **Immediately close the Incognito window without signing out.**
   - Paste into the Web UI's **+ Add Account** or pipe it to `gsb --import-cookie <name>`.

2. **One-click isolated browser with a local helper**
   - On the laptop: `gsb --login-helper` (listens on `127.0.0.1:20134`).
   - Forward both ports: `ssh -L 20133:127.0.0.1:20133 -L 20134:127.0.0.1:20134 user@vps`.
   - In the VPS Web UI: **+ Add Account → Isolated browser window → Launch**.
   - The page calls the VPS first; if the VPS has no browser/GUI, it falls back to the local helper and posts the captured cookie to the VPS import endpoint.
   - Account name, proxy, and group are collapsed under **Optional settings** in that dialog; leave them closed for a normal single-key setup.

### 4.3 Cookie import is acceptable only when the profile is already signed in

```sh
gsb --import-chrome <name> --chrome-profile "<Profile Name>"
gsb --import-chrome <name> --chrome-profile arc
gsb --import-chrome <name> --cookie-db "/path/to/Cookies"
```

Supported: Chrome, Chromium, Brave, Edge, Vivaldi, Arc, Firefox.

- `--cookie-db` takes precedence over `--chrome-profile`.
- `--chrome-profile` accepts a profile name, a profile directory, or a Cookies SQLite path.
- With no hint, `gsb` auto-detects every known browser and prefers the first database that actually contains a `SAPISID` cookie.
- Arc on macOS: `~/Library/Application Support/Arc/User Data`.
- Arc on Windows: `%LOCALAPPDATA%\Packages\TheBrowserCompany.Arc_*\LocalCache\Local\Arc\User Data` (auto-detected by glob).
- macOS keychain service names: `Chrome Safe Storage`, `Arc Safe Storage`, `Brave Safe Storage`, `Chromium Safe Storage`, `Microsoft Edge Safe Storage`, `Vivaldi Safe Storage`, `Opera Safe Storage`. The first import triggers a Keychain prompt; `CHROME_PASS` is the escape hatch.
- Linux keyring-backed cookies may require `secret-tool` and an unlocked keyring.
- Windows DPAPI requires the same Windows user context; App-Bound (`v20`) encryption requires a manual export.

**Multi-account Chrome profiles:** a profile signed in to several Google accounts always imports the profile's *primary* account, because Chromium stores one cookie set per domain. To target a secondary account, set `"authUser": <N>` in the account JSON after import (the index from Google's `/u/N/app` surface). Bootstrap fetches `/u/N/app` and falls back to `/app` if the index no longer resolves. There is no auto-detection: Chrome's `account_info` order does not reliably map to `/u/N`, so guessing could attribute the session to the wrong account.

Cookie export is the fallback when browser decryption is unavailable:

```sh
gsb --import-cookie <name> --cookie-file /path/to/cookies.json
gsb --import-cookie <name> < /path/to/cookies.json
```

`--cookie-value` works but leaks into shell history and process listings. Do not suggest it unless the user explicitly asks.

### 4.4 What "success" means

Both import paths bootstrap the account before reporting success, but neither can create a session that was never established.

Success signal:

```sh
gsb --probe
```

The account must report models and 5h/weekly quota without an auth error. The mere existence of `accounts/<name>.json` is not success.

### 4.5 Additional accounts

```sh
gsb --login acc2
gsb --import-chrome acc2 --chrome-profile "Profile 2"
```

Each account has its own cookie state, model list, quota state, transport session, optional proxy, and group. Files for the same Google email are deduplicated; they do not create extra quota.

---

## 5. Never recommend

- Copying cookies from a browser that was never signed in to Gemini. Those cookies expire almost immediately and surface later as `401 upstream_auth_error` or upstream `code 1016`.
- Pasting cookies, `Cookie` headers, account JSON, or `session-cache.json` into chat, issues, logs, commits, or shell history.
- `--rotate` as a fix for an expired session. It only renews rotating cookies such as `__Secure-1PSIDTS`; it cannot restore a revoked or never-established sign-in.
- Committing `$BRIDGE_HOME` (default `~/.gemini-stealth-bridge`) to version control.
- Creating one router connection per account. Rotation belongs to `gsb`, which tracks quota, quarantine, and continuation affinity.
- Setting `BRIDGE_API_KEY=public` on a non-loopback bind. The daemon warns but does not refuse to start.
- Disabling `BRIDGE_REJECT_DOWNGRADE` to "fix" quota errors. It hides silent model substitution instead of resolving it.
- Setting `BRIDGE_ALLOW_PRIVATE_IMAGE_URLS=1` for untrusted clients. It weakens SSRF protection.
- Using `--unquarantine` in a loop. A quarantined account is restricted upstream; repeated retries do not help.
- Promising that a model, context window, or quota is always available. All three come from the account and can change.

---

## 6. Diagnosing auth and account failures

### 6.1 Status vocabulary

Statuses are computed in this precedence order and are shared by `/v1/accounts`, `gsb --accounts`, and the UI:

| Status | UI label | Meaning | Action |
|---|---|---|---|
| `disabled` | Paused | Operator paused the account | Resume in UI or `POST /v1/admin/accounts/<id>/toggle` |
| `quarantined` | Quarantined | Upstream restricted the account (`TOS_VIOLATION`, `PERMISSION_DENIED`) | Do not retry in a loop; review the account |
| `expired` | Expired | Saved session invalid (`upstream_auth_error`, `auth_redirect`, `code 1016`, `snlm0e`, …) | `gsb --login <name>` |
| `exhausted` | Exhausted | A real quota window is spent until `resetAt`, or a model stayed exhausted across retries | Wait for reset, use another account/model, or use `gemini-flash-lite` |
| `cooling` | Cooling | Failed within the last 60 seconds | Wait; the pool routes around it |
| `degraded` | Degraded | Cooldown elapsed but the failure counter is still nonzero | A success or a manual reset clears it |
| `unknown` | Unprobed | No quota check has run in this process | `gsb --accounts --accounts-live` or query a running daemon |
| `ok` | Ready | Loaded, authenticated, no known limit | None |

### 6.2 Auth failure table

| Symptom | Meaning | Action |
|---|---|---|
| `401 upstream_auth_error` | Session expired or rejected | `gsb --login <name>`, then `gsb --probe` |
| upstream `code 1016` | Same as above | Same |
| `403 account_banned` / `TOS_VIOLATION` | Account restricted upstream | Stop retrying; the account is quarantined |
| `no SAPISID cookie found` | Selected browser profile is not signed in | Use `gsb --login` |
| `401 authentication_error` from `gsb` | Wrong API key, not a Google problem | Check pool/group/account key |
| `503 auth_unavailable` | No accounts loaded | Check `BRIDGE_HOME`, import an account, `/v1/refresh` |
| `403 remote_web_ui_locked` | Remote UI with no admin key | Set `BRIDGE_ADMIN_KEY` or use an SSH tunnel |

### 6.3 Distinguishing causes

- **`gsb` 401** = caller's API key. **`upstream_auth_error` 401** = Google session.
- **`model_not_found` 404** = the selected account does not list that model. Query `/v1/models` with the same key; another account may have it.
- **`429 rate_limit_error`** = quota, RPM, concurrency, or session queue. Inspect `/v1/quota` and the message text.
- **`429 model_substitution`** = Gemini silently served a different model. Downgrade rejection is enabled; retry after checking quota or choose an available model.
- **`429 session_busy`** = transport queue saturation, not account quota. Raise `BRIDGE_MAX_CONCURRENT_PER_ACCOUNT` / `BRIDGE_MAX_QUEUE` or add accounts.
- **`tool_call_error`** = the model's tool call did not satisfy the client's schema. Check the schema and `tool_choice`; `gsb` coerces stringified scalars but cannot invent required fields.

---

## 7. Pool routing and quota

### 7.1 Quota windows

| Bucket | Default total | Notes |
|---|---|---|
| `5h` | `2,400` | Rolling five-hour window |
| `weekly` | `48,384` | Seven-day weekly allowance |

Sources: `batchexecute` RPC `qpEbW` (`CHECK_GEMINI_QUOTA`) plus GraphQL `GetUserQuotaService` pools `GXU_FIVE_HOURLY` and `GXU_WEEKLY`.

The 5-hour window is nested under the weekly allowance, so effective 5-hour remaining is capped at weekly remaining.

When an account reaches `0` credits, non-lite models (`gemini-flash`, `gemini-pro`, `gemini-argon`) are blocked or downgraded until `resetAt`; `gemini-flash-lite` remains usable.

### 7.2 Synthetic cooldowns

When upstream silently substitutes a model, `gsb` records a synthetic per-model cooldown that escalates across repeated observations:

`1m → 5m → 15m → 1h → 6h → 24h`

These appear as quota buckets with `synthetic: true` and IDs beginning `sub:`. They are excluded from real quota bars and cleared by a successful request, a fresh credential capture, or unquarantine.

### 7.3 Checking state

```sh
gsb --accounts                 # read saved files
gsb --accounts --accounts-live # query upstream first
```

Or, against a running daemon:

```sh
curl -sS http://127.0.0.1:20133/v1/accounts?live=1 \
  -H 'Authorization: Bearer <key>'
curl -sS http://127.0.0.1:20133/v1/quota?fresh=1 \
  -H 'Authorization: Bearer <key>'
curl -sS http://127.0.0.1:20133/v1/sessions \
  -H 'Authorization: Bearer <key>'
```

Account keys see only their account; group keys see only their group; the pool/admin key sees everything.

### 7.4 Rate and concurrency controls

| Variable | Default | Effect |
|---|---|---|
| `BRIDGE_MAX_RPM_PER_ACCOUNT` | `24` | Rolling-minute request cap per account; `0` disables |
| `BRIDGE_MAX_CONCURRENT_PER_ACCOUNT` | `2` | Concurrent active streams per account; `0` disables |
| `BRIDGE_MAX_SESSIONS` | `16` | Total transport sessions |
| `BRIDGE_MAX_QUEUE` | `4` | Queue depth per session |
| `BRIDGE_SESSION_WAIT_MS` | `10000` | Queue wait timeout |
| `BRIDGE_QUOTA_PREFLIGHT` | `strict` | `strict` skips spent accounts, `advisory` logs and proceeds, `off` disables. `strict` is skipped when `BRIDGE_REJECT_DOWNGRADE=0` |

If a user hits `session_busy`, it is usually concurrency, not quota. If they hit `rate_limit_error` with a reset timestamp, it is quota.

---

## 8. Client integration

### 8.1 Direct

| Setting | Value |
|---|---|
| Base URL | `http://127.0.0.1:20133/v1` |
| API key | Pool key (default `public`), group key, or `gsb-<name>` |
| Model | An `id` from `GET /v1/models` with the same key |

### 8.2 Routers (9router, etc.)

One provider, one credential, pool key:

| Setting | Value |
|---|---|
| Base URL | `http://127.0.0.1:20133/v1` |
| API type | Chat Completions or Responses |
| Prefix | `gsb` |
| Credential | Pool key |

The bridge accepts the `gsb/` prefix itself, plus `google/`, `models/`, `vx/`, and `vertex/`. A `@think=<level>` suffix overrides thinking effort per request.

Validate metadata propagation:

```sh
./scripts/check-router-metadata.sh http://127.0.0.1:20133 public
./scripts/check-router-metadata.sh --compare \
  http://127.0.0.1:20133 http://127.0.0.1:20130 - sk-router-key
```

If a harness cannot render a low/medium/high selector, run the compare form before blaming `gsb`. It names the field the router dropped.

### 8.3 Coding agents

Point the agent at the Chat Completions or Responses base URL with the pool key. Tools, images, reasoning, and media generation all work through that one connection.

Do not tell users to create one router connection per account. Account rotation belongs to `gsb`, which tracks quota, quarantine state, and conversation continuation affinity. A router that spreads requests over per-account connections cannot see any of that and will discard the cached Gemini session between turns.

### 8.4 Multi-user

Give each user a group key instead of the shared pool key:

```sh
BRIDGE_ACCOUNT_GROUP="acct-a:team-a,acct-b:team-a,acct-c:team-b" \
BRIDGE_GROUP_TOKENS="alice-key:team-a,bob-key:team-b" ./gsb
```

Each user's traffic stays on their own accounts, fails over only inside them, and never spends another group's quota.

---

## 9. API surface agents must know

### 9.1 Chat Completions

`POST /v1/chat/completions`

Accepted body fields:

| Field | Notes |
|---|---|
| `model` | Optional; supports prefixes and `@think=` |
| `messages` | Required, non-empty |
| `stream` | Boolean |
| `tools`, `tool_choice` | Prompt-based function calling |
| `temporary` | Temporary Gemini conversation, no cache reuse |
| `reasoning_effort` | `low`/`medium`/`high` |
| `thinking_level`, `thinkingLevel` | Aliases |
| `reasoning` | `{enabled, effort, level}` |
| `thinking` | `{enabled, type, effort, level, thinkingLevel, budget_tokens}` |
| `thinking_budget` | ≤4096 → low, ≤16384 → medium, above → high |
| `thinkingConfig`, `generationConfig.thinkingConfig` | `thinkingLevel` / `thinkingBudget` |
| `output_config` | `effort` |

Ignored: `temperature`, `top_p`, `max_tokens`, `stop`, and other sampling controls. Do not present them as working output limits.

Streaming: `chat.completion.chunk` events, final chunk with `finish_reason` and `usage`, then `data: [DONE]`. `: ping` comments every 15 seconds.

Non-streaming: `choices[0].message.content`, optional `reasoning_content`, optional `tool_calls`, `finish_reason`, `usage`.

### 9.2 Responses

`POST /v1/responses`

`input` accepts a string, a list of items, or `messages` as a fallback. `instructions` becomes a system message.

Supported item types: `message`, `function_call`, `function_call_output`, `custom_tool_call`, `custom_tool_call_output`, `reasoning`, `input_text`, `input_image`, `image`.

Supported tool shapes: flat `{type:"function", name, description, parameters}`, nested Chat-style, `custom`, `local_shell`, and `namespace` wrappers.

Output item types: `reasoning`, `message` with `output_text`, `function_call`, `custom_tool_call`.

Streaming events: `response.created`, `response.in_progress`, `response.output_item.added`, `response.content_part.added`, `response.output_text.delta`, `response.output_text.done`, `response.content_part.done`, `response.reasoning_summary_part.added`, `response.reasoning_summary_text.delta`, `response.reasoning_summary_text.done`, `response.function_call_arguments.delta`, `response.function_call_arguments.done`, `response.custom_tool_call_input.delta`, `response.custom_tool_call_input.done`, `response.output_item.done`, `response.completed`.

There is **no** `[DONE]` marker; the stream ends with `response.completed`.

For multi-turn calls, resend history in `input`. `previous_response_id` is not supported.

### 9.3 Models

`GET /v1/models` returns OpenAI-style `data` plus OpenRouter-style metadata: `context_length`, `max_output_tokens`, `reasoning`, `supported_parameters`, `thinking`, and `capabilities` including `thinkingLevels`, `defaultThinkingLevel`, and `limitsSource`.

`ETag` / `If-None-Match` are supported. `?fresh=1` forces live discovery. The list is per-account, so different keys can see different models.

### 9.4 Accounts, quota, sessions, refresh

- `GET /v1/accounts` — status, failures, provenance, models, quota, `quotaWindow`, telemetry. `?live=1` refreshes upstream.
- `GET /v1/quota` — quota buckets; `?fresh=1`.
- `GET /v1/sessions` — transport sessions plus continuation cache hits/misses.
- `POST /v1/refresh` — reload credentials and models. Rejects requests with an `Origin` header.

### 9.5 Health

- `GET /health` — unauthenticated liveness/counts. Does not prove upstream generation.
- `GET /readyz` — per-account `models_fresh`, `quota_fresh`, `auth_ok`, `quarantined`. `?strict=1` returns `503` when no healthy account exists.

### 9.6 Diagnostic headers

`X-GSB-Account`, `X-GSB-Model`, `X-GSB-Attempt`, `X-GSB-Upstream-Status`, `X-GSB-Retry-Reason`, plus `Retry-After` and `X-Resets-At` on quota errors. MCP adds `X-GSB-Duration-Ms` and `X-GSB-Sources`.

Use `X-GSB-Account` to confirm which account served a request before concluding that rotation is broken.

---

## 10. Function calling and tool schemas

Function calling is **prompt-based**. `gsb` renders the tool definitions into the prompt, asks the model for a JSON tool-call envelope, parses it, validates it against the tool's JSON Schema, and emits OpenAI-style calls. The client executes the tools.

### 10.1 What is validated

Supported JSON Schema subset includes:

- `type` (including type unions), `enum`, `const`
- `$ref` to `#/...` local definitions
- string `minLength`, `maxLength`, `pattern`
- numeric `minimum`, `maximum`, `exclusiveMinimum`, `exclusiveMaximum`, `multipleOf`
- array `items`, `prefixItems`, `minItems`, `maxItems`, `uniqueItems`
- object `properties`, `required`, `additionalProperties`
- `allOf`, `anyOf`, `oneOf`, `not`, `nullable`

Tool names must match `^[A-Za-z0-9_.:-]{1,128}$`. Duplicate names are rejected. Note that the error message for an invalid name mentions 64 characters, but the validator accepts up to 128; prefer short names for client compatibility.

### 10.2 Automatic coercion

Before validation, `gsb` coerces common model mistakes into the declared types:

- `"42"` → `42`
- `"true"` → `true`
- JSON-encoded objects/arrays in strings → real objects/arrays
- scalar → single-element array when an array is required

Coercion only applies when the raw value fails validation and the coerced value passes.

### 10.3 Repair and recovery

- Strips redundant closing braces/brackets outside string literals.
- Removes trailing commas before `}` or `]`.
- Quotes bare identifier keys (`{name: "bash"}` → `{"name": "bash"}`).
- Repairs JSON string escapes and HTML-sanitized content inside string values.
- Detects truncated `{"tool_calls":[...` arrays and retries.
- Extracts valid calls even when preceded by refusal prose.
- Retries when the model refuses a required tool turn or invents a tool-budget limit.
- Keeps raw JSON envelopes out of plain answer text.
- Does not mark an account `degraded` for client/schema `tool_call_error`.

### 10.4 `tool_choice`

| Value | Behavior |
|---|---|
| `auto` / absent | Model decides |
| `none` | No tools offered |
| `required` | Model must call a tool |
| `{"type":"function","function":{"name":"…"}}` | Force a specific function |

A forced function must exist in `tools`, or the request fails with `invalid_request_error`.

### 10.5 Streaming behavior with tools

- `BRIDGE_LIVE_TOOL_CALLS=false` (default) buffers and validates the complete call before emitting it.
- `BRIDGE_LIVE_TOOL_CALLS=true` emits incremental deltas; a late upstream revision cannot be repaired after emission.
- With tools enabled, prose answers longer than 240 bytes stream live once the served model identity is verified. Mid-answer envelopes and fenced look-alikes stay out of the streamed prefix until parsing commits the turn.
- Quoted envelope examples in documentation pass through as text and do not error as malformed calls.

### 10.6 Media tool

When tools are enabled and the prompt has media intent, `gsb` injects `generate_media`. See [Images and native media generation](#11-images-and-native-media-generation). Do not tell users to define it themselves; it is server-side.

---

## 11. Images and native media generation

### 11.1 Input images

Accepted shapes:

```json
{ "type": "image_url", "image_url": { "url": "data:image/png;base64,..." } }
{ "type": "input_image", "image_url": "https://example.com/image.png" }
{ "type": "image", "data": "<base64>", "mimeType": "image/png" }
```

Limits:

- remote image: 20 MiB, at most 5 redirects, private/loopback blocked unless `BRIDGE_ALLOW_PRIVATE_IMAGE_URLS=1`
- request body: 32 MiB
- MCP body: 8 MiB

MIME type is detected from magic bytes, not trusted from the URL.

### 11.2 Three media surfaces

1. **Coding Agent Mode (`tools` enabled)**
   - `generate_media` is injected only when the request has client tools, `tool_choice` is `auto`/unset, and the last three user messages contain media intent (or history already has a `generate_media` call).
   - Parameters: `media_type` (`image`|`video`), `prompt`, `aspect_ratio`, `format` (`png`|`jpeg`|`gif`), `duration` (`10`|`15`|`30`), `output_path`.
   - Result includes `url`, `urls`, `revised_prompt`, `markdown`, `suggested_filename`, `local_download_cmd`, and a note telling the agent to save the file with bash.
   - If generation fails on the selected account, a pool/group request retries on other eligible accounts.

2. **Direct Chat / Responses**
   - Ask for an image or video in a normal request.
   - `gsb` enables media extensions and rewrites upstream media URLs to local `/v1/media/<id>` URLs.

3. **Dedicated endpoints**
   - `POST /v1/images/generations` — `prompt`, `model`, `response_format` (`url`|`b64_json`), `size`, `aspect_ratio`, `format`/`output_format`. `size` is parsed as `WxH` and snapped to the nearest supported ratio (`auto` is accepted and ignored); when both shape hints are sent they must agree, and a conflict or malformed value returns `400`. The response reports the produced `width`/`height`/`size` instead of echoing the request.
   - `POST /v1/videos/generations` — `prompt`, `aspect_ratio`, `duration`, returns MP4 `url` plus `thumbnail_url`.

### 11.3 Media URLs

Generated media is served at `GET /v1/media/<id>`. The proxy URL is built from the request's `Host` (or `X-Forwarded-Host`) and scheme (or `X-Forwarded-Proto`), so a client connecting through a reverse proxy receives a URL it can reach. The endpoint is unauthenticated because upstream URLs require Google cookies. Media is cached in memory (max 96 entries), so URLs die on restart or eviction.

Full-resolution images use `=s0-d`; lossless PNG uses `=s0-rp`. Both HTTP redirects and application-level `text/plain` URL redirects are followed. `revised_prompt` comes from Gemini Web's internal `modelDescription` when available.

### 11.4 Video expectations

Veo generation is slow. Measured runs commonly take 60–150 seconds. Tell users to set a generous client timeout (`--max-time 240` or more) and not to retry immediately; a cut connection can still consume quota.

---

## 12. Web search grounding and MCP

### 12.1 Grounding

When Gemini Web answers with Google Search grounding, `gsb` appends:

```
<answer>

---
Sources:
1. Title — https://example.com/a
2. Title — https://example.com/b
```

`BRIDGE_INCLUDE_GROUNDING=0` suppresses the list. Citations are merged across frames and deduplicated by URL.

A grounded answer is a **full model turn**, not a search-API lookup. Measured searches take 14–110 seconds. Cutting one short throws away the quota it already spent.

### 12.2 MCP server

`POST /mcp` (alias `/v1/mcp`), Streamable HTTP, JSON-RPC 2.0, protocol version `2025-06-18`.

- `POST` only; `GET` → `405` (no server-initiated stream)
- notifications → `202` empty body
- methods: `initialize`, `ping`, `tools/list`, `tools/call`
- one tool: `web_search`
- body limit: 8 MiB
- accepts pool, group, and account keys
- JSON-RPC errors return HTTP 200; inspect the `error` object

`web_search` arguments:

| Argument | Required | Notes |
|---|---|---|
| `query` | Yes | Search query or question |
| `model` | No | `gemini-flash`, `gemini-flash-lite`, `gemini-pro`, `gemini-argon` |
| `timeout_seconds` | No | 10–600; default from `BRIDGE_SEARCH_TIMEOUT_MS` (180 s) |

Client config:

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

Search behavior:

- runs as a temporary chat, so it does not add threads to the user's Gemini history,
- uses the same account pool, quota accounting, and failover as chat,
- model preference order when no override: `gemini-flash`, `gemini-flash-lite`, `gemini-pro`, `gemini-argon`, then the account's first model,
- returns `search failed: …` as an MCP tool error (`isError: true`), not a JSON-RPC error,
- diagnostic headers: `X-GSB-Account`, `X-GSB-Model`, `X-GSB-Duration-Ms`, `X-GSB-Sources`.

When reporting a slow or "hung" search, check whether a request is still in flight before assuming the endpoint is broken, and tell the user to set a generous client timeout rather than a search-API-sized one.

---

## 13. Per-account proxies and groups

### 13.1 Proxies

```sh
BRIDGE_PROXY_SERVER=socks5://127.0.0.1:1080 ./gsb
BRIDGE_EGRESS_PER_ACCOUNT="acct-a:socks5://127.0.0.1:1081,acct-b:http://user:pass@proxy.example:3128" ./gsb
```

Precedence per account:

1. `BRIDGE_EGRESS_PER_ACCOUNT` entry
2. `proxyUrl` in the account file / Web UI
3. `BRIDGE_PROXY_SERVER`
4. direct

Changing an account's proxy rebuilds its transport session so no connection survives from the previous egress.

`BRIDGE_PROXY_SERVER` applies to Gemini transport only. It does not proxy remote-image downloads and is not a process-wide privacy boundary.

### 13.2 Groups

- Account group precedence: `BRIDGE_ACCOUNT_GROUP` entry, then the account file's `accountGroup`, then empty.
- A group key sees and fails over only inside its group.
- Failover never crosses a group boundary: a request served by a grouped account retries only inside that group.
- A pool-key request that starts on an ungrouped account also retries only on ungrouped accounts; a grouped account is not a valid fallback for it. Initial selection may still pick any eligible account, so grouping the whole pool avoids mixed behavior.
- A group with no loaded accounts returns `401` explaining that the group is empty.

---

## 14. Web Control Plane and admin API

### 14.1 Enabling

`--web`, `BRIDGE_WEB_UI=1`, `start.sh`, or Docker (which sets `BRIDGE_WEB_UI=1`).

Compile-time removal: `go build -tags nowebui`. `GET /ui` then returns `404` with `webui_compiled_out`.

### 14.2 Access gate

- Loopback with no admin key: allowed.
- Remote (non-loopback peer, or forwarding headers naming a non-loopback client) with `BRIDGE_API_KEY=public` and no `BRIDGE_ADMIN_KEY`: HTTP `403 remote_web_ui_locked`.
- With a key configured: the page prompts; the key is stored in `sessionStorage` and sent as `Authorization` and `X-GSB-Admin-Key`.
- 5 failed attempts per IP per 60 s → HTTP `429 admin_auth_rate_limited`.
- `Sec-Fetch-Site: cross-site` → `403 cross_site_forbidden`.

Docker always looks remote because of bridge NAT. Tell users to set `BRIDGE_ADMIN_KEY` before opening the UI in a container.

### 14.3 Tabs and actions

- Accounts tab: pool/per-account keys, quota bars, filters, priority `↑`/`↓`, `⋯` menu (Refresh, Pause/Resume, Unquarantine/Reset failures, Copy Key, Remove), Refresh All.
- Add Account: Incognito cookie paste plus an isolated-browser launcher (with local helper fallback). Account name, proxy, and group are collapsed under **Optional settings**.
- Runtime configuration: strategy, quota preflight, priority, default thinking, RPM, concurrency, downgrade rejection, live thoughts, live tool calls, tool preamble, history offload. Apply in memory or persist to `config.env`.

### 14.4 Admin API

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/admin/config` | Effective config, sources, pool key, cursor, uptime, metrics |
| `PATCH` | `/v1/admin/config` | Update runtime fields; `persist: true` writes `config.env` |
| `POST` | `/v1/admin/accounts/import` | Import from cookie payload (`id`, `cookie`, `accountGroup`, `proxyUrl`) |
| `POST` | `/v1/admin/accounts/browser-login` | Isolated login + import (4-minute timeout) |
| `DELETE` | `/v1/admin/accounts/<id>` | Remove account file and unload |
| `POST` | `/v1/admin/accounts/<id>/probe` | Bootstrap and refresh; clears failures on success |
| `POST` | `/v1/admin/accounts/<id>/rotate` | Rotate rotating cookies |
| `POST` | `/v1/admin/accounts/<id>/unquarantine` | Clear quarantine, failures, synthetic cooldowns |
| `POST` | `/v1/admin/accounts/<id>/toggle` | Pause/resume |
| `POST` | `/v1/admin/accounts/<id>/move` | `{"direction":"up"\|"down"}`, persists priority |

Runtime fields editable through `PATCH`:

`defaultThinking`, `accountStrategy`, `accountPriority`, `quotaPreflight`, `maxRpmPerAccount` (0–600), `maxConcurrentPerAccount` (0–64), `rejectDowngrade`, `liveThoughts`, `liveToolCalls`, `keepToolPreamble`, `historyOffload`, and `persist`.

Everything else is environment-only.

---

## 15. Configuration reference

### 15.1 Precedence

1. process environment
2. `$BRIDGE_HOME/config.env`
3. defaults

`HOST`/`PORT` are fallbacks for `BRIDGE_HOST`/`BRIDGE_PORT`. Booleans accept `1/true/yes/on` and `0/false/no/off`. Durations are milliseconds. `gsb --check-config` validates.

### 15.2 Listener and keys

| Variable | Default | Notes |
|---|---|---|
| `HOST` / `BRIDGE_HOST` | `127.0.0.1` | Listen address |
| `PORT` / `BRIDGE_PORT` | `20133` | Listen port |
| `BRIDGE_HOME` | `~/.gemini-stealth-bridge` | Data root |
| `BRIDGE_API_KEY` | `public` | Pool key; `public` also allows no key |
| `BRIDGE_ADMIN_KEY` | empty → `BRIDGE_API_KEY` | Admin key |
| `BRIDGE_ACCOUNT_TOKENS` | empty | `token:account_id` |
| `BRIDGE_GROUP_TOKENS` | empty | `token:group_name` |
| `BRIDGE_ACCOUNT_GROUP` | empty | `account_id:group_name` |
| `BRIDGE_WEB_UI` | `false` (`1` in start.sh/Docker) | Enable UI/admin |
| `WEB_CRED_FILE` | `$BRIDGE_HOME/web-credentials.json` | Parsed but unused; the loader always reads `$BRIDGE_HOME/web-credentials.json` and `accounts/*.json` |

### 15.3 Credentials and protocol

| Variable | Default | Notes |
|---|---|---|
| `CHROME_COOKIES` | empty | Cookie DB/profile hint; live-cookie fallback when no saved accounts |
| `CHROME_PASS` | empty | Browser cookie-encryption password (not Google password) |
| `BRIDGE_LOCALE` | `en` | Gemini language |
| `BRIDGE_PROTOCOL_MODE` | `bridge` | `bridge` or `web` |
| `BRIDGE_PERSONAL_CONTEXT` | `auto` | `auto`, `on`, `off` |
| `BRIDGE_BL_OVERRIDE` | empty | Raw `bl` token override |

### 15.4 Egress and transport

| Variable | Default | Notes |
|---|---|---|
| `BRIDGE_PROXY_SERVER` | empty | HTTP/SOCKS5 proxy |
| `BRIDGE_EGRESS_PER_ACCOUNT` | empty | `account_id:proxy_url` |
| `BRIDGE_IMPERSONATE` | `chrome_150_psk` | uTLS profile |
| `BRIDGE_UA_OS` | host OS | `linux`, `windows`, `macos` |
| `BRIDGE_TLS_EXT_ORDER` | `fixed` | `fixed` or `random` |
| `BRIDGE_FOLLOW_REDIRECTS` | `true` | Follow redirects |
| `BRIDGE_IPV6` | `auto` | `auto`, `off`, `on` |
| `BRIDGE_DISABLE_IPV6` | empty | `1` → default `off` |
| `BRIDGE_ALLOW_PRIVATE_IMAGE_URLS` | disabled | `1` allows private image URLs |
| `BRIDGE_MAX_SESSIONS` | `16` | Session pool |
| `BRIDGE_MAX_QUEUE` | `4` | Queue depth |
| `BRIDGE_SESSION_WAIT_MS` | `10000` | Queue wait |
| `BRIDGE_IDLE_SESSION_MS` | `180000` | Idle expiry |

TLS profiles: `chrome_152_psk`, `chrome_152`, `chrome_150_psk`, `chrome_150`, `chrome_146_psk`, `chrome_146`, `chrome_144_psk`, `chrome_144`, `chrome_133_psk`, `chrome_133`, `chrome_131_psk`, `chrome_131`, `brave_146_psk`/`brave`, `firefox_148`/`firefox`, `firefox_147_psk`, `safari_16_0`/`safari`.

### 15.5 Routing and quota

| Variable | Default | Notes |
|---|---|---|
| `BRIDGE_ACCOUNT_STRATEGY` | `rotate` | `rotate`, `lru`, `fill-first` |
| `BRIDGE_ACCOUNT_PRIORITY` | empty | Comma-separated IDs |
| `BRIDGE_QUOTA_PREFLIGHT` | `strict` | `strict`, `advisory`, `off` |
| `BRIDGE_MAX_RPM_PER_ACCOUNT` | `24` | `0` disables |
| `BRIDGE_MAX_CONCURRENT_PER_ACCOUNT` | `2` | `0` disables |
| `BRIDGE_REJECT_DOWNGRADE` | `true` | Reject silent substitution |
| `BRIDGE_QUIET_HOURS` | empty | `HH:MM-HH:MM`; pauses background work only |

### 15.6 Generation, history, tools

| Variable | Default | Notes |
|---|---|---|
| `BRIDGE_DEFAULT_THINKING` | `low` | `low`, `medium`, `high` |
| `BRIDGE_LIVE_THOUGHTS` | `true` | Emit `reasoning_content` |
| `BRIDGE_TEMPORARY` | `false` | Temporary conversations |
| `BRIDGE_SESSION_CACHE` | `true` | Continuation cache |
| `BRIDGE_TOOL_PREAMBLE` | `false` | Keep pre-tool text in content |
| `BRIDGE_MAX_PROMPT_CHARS` | `850000` | Inline budget in bytes |
| `BRIDGE_MAX_OLD_TOOL_CHARS` | `32000` | Tool-result compaction |
| `BRIDGE_HISTORY_OFFLOAD` | `true` | Upload older turns as attachments |
| `BRIDGE_INCLUDE_GROUNDING` | `true` | Append `Sources:` |
| `BRIDGE_CONTEXT_WINDOW` | `1048576` | Fallback context |
| `BRIDGE_MAX_OUTPUT_TOKENS` | `65536` | Fallback output |
| `BRIDGE_TOKEN_ESTIMATOR` | `script` | `script` or `bytes4` |
| `BRIDGE_LIVE_TOOL_CALLS` | `false` | Incremental tool deltas |
| `BRIDGE_TOOL_SCHEMA_COMPACT` | `true` | Compact schemas |
| `BRIDGE_TOOL_DESC_MAX_CHARS` | `400` | Description cap |
| `BRIDGE_STREAM_BUFFER` | `verified` | `verified` or `eager` |
| `BRIDGE_EARLY_SSE` | `false` | Flush headers early |
| `BRIDGE_EARLY_SSE_GRACE_MS` | `20000` | Grace before header flush |
| `BRIDGE_MAX_ATTEMPTS` | `3` | Total attempts |
| `BRIDGE_MAX_UPSTREAM_ATTEMPTS` | `3` | Upstream calls |
| `BRIDGE_RETRY_BUDGET_MS` | `150000` | Retry wall-clock budget |

### 15.7 Timeouts and diagnostics

| Variable | Default | Notes |
|---|---|---|
| `BRIDGE_FIRST_BYTE_TIMEOUT_MS` | `60000` | First byte |
| `BRIDGE_STALL_TIMEOUT_MS` | `90000` | Between frames |
| `BRIDGE_STREAM_TIMEOUT_MS` | `600000` | Whole request |
| `BRIDGE_ROTATE_COOKIES_MS` | `540000` | Rotation interval (60s–3600s) |
| `BRIDGE_MODEL_REFRESH_MS` | `1800000` | Model/quota refresh |
| `BRIDGE_SEARCH_TIMEOUT_MS` | `180000` | MCP search default (10s–600s) |
| `BRIDGE_LOG_REQUESTS` | `false` | JSON request logs |
| `BRIDGE_DUMP_REQUEST` | empty | Redacted request dumps |

### 15.8 CLI flags

| Flag | Purpose |
|---|---|
| `--login <name>` | Isolated browser sign-in |
| `--login-helper` | Local desktop helper on `127.0.0.1:20134` |
| `--import-chrome <name>` | Import from browser profile |
| `--import-cookie <name>` | Import raw cookie payload |
| `--chrome-profile <v>` | Profile name/dir/DB |
| `--cookie-db <path>` | Explicit DB or profile dir |
| `--cookie-value <v>` | Inline payload (avoid) |
| `--cookie-file <path>` | Payload file |
| `--probe` | Bootstrap, verify, print |
| `--rotate` | Rotate cookies |
| `--accounts` | Status table |
| `--accounts-live` | With `--accounts`, refresh upstream first; used alone it also prints the live table |
| `--unquarantine <id>` | Clear quarantine in the account file |
| `--models-json` | Print `/v1/models` payload |
| `--capture-reference` | Write reference fingerprint |
| `--check-config` | Validate config |
| `--healthcheck` | Probe `/health` |
| `--web` | Enable UI/admin |
| `--version` | Print version |

---

## 16. Data layout and credentials

```text
$BRIDGE_HOME/
├── .lock
├── config.env
├── session-cache.json
├── reference-fingerprint.json
├── web-credentials.json
└── accounts/
    ├── acc1.json
    └── acc2.json
```

Account files contain: `kind`, `name`, `email`, `cookies`, `at`, `bl`, `fSid`, `authUser`, `capturedAt`, `rotatedAt`, `cookieDb`, `importedFrom`, `profileId`, `tokenFetchedAt`, `tokenSource`, `lastReqId`, `quarantined`, `quarantineReason`, `disabled`, `accountGroup`, `proxyUrl`, `surfaces`.

Permissions: `0600` for account files, `session-cache.json`, and `config.env`; `0700` for directories.

Continuation cache: 30-minute TTL, 256 entries, persisted. Upload cache: 25 minutes, 128 entries, memory only.

Only one daemon may hold a `BRIDGE_HOME` lock. `--probe` proceeds without it; `--rotate` requires it. `--login`, `--import-*`, `--accounts`, `--models-json`, `--unquarantine`, `--check-config`, and `--capture-reference` do not take the daemon lock.

---

## 17. Security rules

- Loopback by default; `BRIDGE_API_KEY=public` permits unauthenticated requests. Set a random pool key before exposing the daemon.
- Set `BRIDGE_ADMIN_KEY` before opening `/ui` remotely; Docker always looks remote.
- Plain HTTP only. Use a private network, SSH tunnel, or authenticated TLS reverse proxy.
- `/health`, `/readyz`, and `/v1/media/*` are reachable without an API key. `/mcp` uses the same API-key resolver as `/v1/*` and always requires a valid pool, group, or account key.
- `/v1/refresh` rejects requests with an `Origin` header.
- Admin auth is rate-limited; cross-site admin requests are rejected.
- Remote-image fetching blocks private/loopback/link-local destinations unless explicitly allowed.
- Account keys are predictable from filenames; treat them as aliases, not secrets.
- Never echo or persist credentials in logs, commits, issues, or chat.
- `BRIDGE_DUMP_REQUEST` redacts cookie values but still reveals header order and cookie names.

---

## 18. Deployment patterns

### 18.1 Local

```sh
./start.sh
# http://127.0.0.1:20133/ui
```

### 18.2 Remote VPS with SSH tunnel

```sh
ssh -L 20133:127.0.0.1:20133 -L 20134:127.0.0.1:20134 user@vps
gsb --web
gsb --login-helper   # on the laptop, optional
```

### 18.3 Docker

```sh
docker compose up -d
BRIDGE_ADMIN_KEY=$(openssl rand -hex 24) docker compose up -d
```

Docker's bridge NAT makes every request look remote, so `/ui` and `/v1/admin/*` return `403 remote_web_ui_locked` until `BRIDGE_ADMIN_KEY` or a non-public `BRIDGE_API_KEY` is set. Always set one before opening the UI.

The container runs as `uid 10001` with `BRIDGE_HOME=/data`. The default named volume `gsb-data` is pre-owned by that user. For a host bind mount that cannot be `chown`ed (FUSE, NTFS, exFAT, rootless Docker):

```sh
BRIDGE_DATA_DIR=./data BRIDGE_UID=$(id -u) BRIDGE_GID=$(id -g) docker compose up -d
```

The published port binds to `127.0.0.1` by default (`BRIDGE_BIND_IP`). Do not change it to `0.0.0.0` without a key and an external access control layer.

### 18.4 systemd

A minimal unit:

```ini
[Unit]
Description=gemini-stealth-bridge
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=gsb
WorkingDirectory=/opt/gemini-stealth-bridge
Environment=BRIDGE_HOME=/var/lib/gemini-stealth-bridge
Environment=BRIDGE_API_KEY=change-me
Environment=BRIDGE_ADMIN_KEY=change-me-too
ExecStart=/opt/gemini-stealth-bridge/start.sh
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

`start.sh` prefers an existing binary when no Go toolchain is installed, so a deployed tree keeps starting after a source checkout without `go`.

### 18.5 Binary upgrade

Build or download the new binary, replace `gsb`, restart the service. The data directory is forward-compatible; back it up first if the release notes mention schema changes.

---

## 19. Troubleshooting playbook

### 19.1 Ordered checklist

1. Is the daemon running? `curl /health`.
2. Are accounts loaded? `gsb --accounts` or `GET /v1/accounts`.
3. Is any account `ok`? `gsb --accounts --accounts-live`.
4. Which key did the client use? Pool, group, or account?
5. Does the key see the model? `GET /v1/models` with that key.
6. Is quota available? `GET /v1/quota`.
7. Is the error an `gsb` auth error or an upstream auth error?
8. Is the client reading SSE correctly? (Errors can arrive after HTTP 200.)
9. Is a proxy or tunnel in the path? Is `127.0.0.1` the right host from that process?
10. Is the client timeout shorter than the model's real latency?

### 19.2 Symptom → action

| Symptom | Action |
|---|---|
| `503 auth_unavailable` | Import an account; verify `BRIDGE_HOME`; `/v1/refresh` |
| `401 authentication_error` | Fix the API key |
| `401 upstream_auth_error` / `code 1016` | `gsb --login <name>` |
| `403 account_banned` | Stop retrying; review the account |
| `404 model_not_found` | Query `/v1/models` with the same key |
| `429` with `X-Resets-At` | Wait for reset or use another account/model |
| `429 model_substitution` | Check quota; choose an available model |
| `429 session_busy` | Raise concurrency/queue or add accounts |
| `tool_call_error` | Check schema and `tool_choice`; simplify the schema |
| Stream stalls | Raise `BRIDGE_STALL_TIMEOUT_MS`; check proxy/client buffering |
| UI `403` | Set `BRIDGE_ADMIN_KEY` or tunnel |
| UI `401` after restart | Re-enter the key (sessionStorage) |
| Media `404` | Entry evicted or daemon restarted; regenerate |
| `readyz` `503` | No healthy account; inspect per-account fields |

### 19.3 What to collect for a bug report

- endpoint and HTTP/error code,
- model ID,
- which key type was used,
- OS and Go version (`gsb --version`),
- `X-GSB-*` headers,
- redacted daemon logs (`BRIDGE_LOG_REQUESTS=1`),
- `/v1/accounts` output with emails and cookies removed.

Never ask for cookies, account JSON, or unredacted logs.

---

## 20. Answer templates

### 20.1 New user setup

```text
1. Install gsb (prebuilt binary or `go install`).
2. `gsb --login acc1` and complete Google sign-in.
3. `gsb --probe` — confirm models and quota, not just the file.
4. `./start.sh` or `gsb --web`.
5. Client: base URL `http://127.0.0.1:20133/v1`, key `public` (or your BRIDGE_API_KEY), model from `GET /v1/models`.
```

### 20.2 Multi-user setup

```text
BRIDGE_ACCOUNT_GROUP="a1:team-a,a2:team-a,a3:team-b" \
BRIDGE_GROUP_TOKENS="alice-key:team-a,bob-key:team-b" ./gsb
```

Give each user their group key and the same base URL. Each key fails over only inside its group.

### 20.3 Router setup

```text
One provider, one credential: the pool key. Base URL http://127.0.0.1:20133/v1.
Do not add one provider per account; that discards quota-aware rotation and the continuation cache.
Verify metadata with scripts/check-router-metadata.sh.
```

### 20.4 "It says quota exceeded but another account has quota"

```text
Check which key the request used. An account key (gsb-<name>) never fails over.
Use the pool key for automatic rotation, or a group key if the accounts are grouped.
Then check /v1/accounts?live=1 for per-account status.
```

### 20.5 "The UI is locked / 403"

```text
Remote access is locked while BRIDGE_API_KEY=public and BRIDGE_ADMIN_KEY is empty.
Set BRIDGE_ADMIN_KEY, or use an SSH tunnel to 127.0.0.1.
In Docker this is always the case because bridge NAT makes requests look remote.
```

### 20.6 "Search takes too long"

```text
A grounded search is a full model turn (measured 14–110 s). Raise the client timeout,
and optionally raise BRIDGE_SEARCH_TIMEOUT_MS or pass timeout_seconds.
Cutting it short still spends the quota.
```

### 20.7 "Image generation returns 404 later"

```text
Media is cached in memory (max 96 entries) and is not persisted.
A restart or eviction invalidates the URL; regenerate the media.
```
