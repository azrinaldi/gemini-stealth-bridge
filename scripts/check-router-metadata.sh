#!/bin/sh
# Validate that a router or gemini-stealth-bridge endpoint exposes full model metadata.
#
# Usage:
#   scripts/check-router-metadata.sh <base-url> [api-key]
#   scripts/check-router-metadata.sh --compare <bridge-url> <router-url> [bridge-key] [router-key]
#
# The single-URL form checks that every gsb/gemini model carries the fields an
# OpenAI-compatible client needs. The --compare form checks the same fields on the
# bridge, then checks whether the router still forwards them. That distinction
# matters: a router that drops `thinking` or `supported_parameters` is the reason
# a downstream harness cannot offer low/medium/high, and blaming the bridge would
# send the operator to the wrong place.
#
# The two endpoints usually have different credentials, so compare mode accepts a
# key per endpoint. Pass "-" to send no key for one side.

set -eu

usage() {
  echo "Usage: $0 <base-url> [api-key]" >&2
  echo "       $0 --compare <bridge-url> <router-url> [bridge-key] [router-key]" >&2
  exit 2
}

if [ "$#" -lt 1 ]; then
  usage
fi

COMPARE=0
if [ "$1" = "--compare" ]; then
  COMPARE=1
  if [ "$#" -lt 3 ]; then
    usage
  fi
  BRIDGE_URL="${2%/}"
  ROUTER_URL="${3%/}"
  BRIDGE_KEY="${4:-}"
  ROUTER_KEY="${5:-}"
  [ "$BRIDGE_KEY" = "-" ] && BRIDGE_KEY=""
  [ "$ROUTER_KEY" = "-" ] && ROUTER_KEY=""
else
  BASE_URL="${1%/}"
  API_KEY="${2:-}"
  [ "$API_KEY" = "-" ] && API_KEY=""
fi

models_url() {
  case "$1" in
    */v1) printf '%s/models' "$1" ;;
    */models) printf '%s' "$1" ;;
    *) printf '%s/v1/models' "$1" ;;
  esac
}

fetch() {
  url="$(models_url "$1")"
  out="$2"
  key="$3"
  if [ -n "$key" ]; then
    curl -fsSL -H "Authorization: Bearer $key" "$url" -o "$out"
  else
    curl -fsSL "$url" -o "$out"
  fi
}

# check_payload <json-file> <label> <strict>
#
# strict=1 requires the full OpenRouter-style surface (reasoning, supported_parameters,
# thinking). strict=0 only reports what survived, which is what a router is judged on:
# it must not lose thinkingLevels even if it normalizes other fields.
check_payload() {
  python3 - "$1" "$2" "$3" <<'PYEOF'
import json
import sys

path, label, strict = sys.argv[1], sys.argv[2], sys.argv[3] == "1"

with open(path, "r", encoding="utf-8") as f:
    payload = json.load(f)

data = payload.get("data", [])
if not isinstance(data, list) or not data:
    print(f"ERROR: {label}: /v1/models returned an empty data array", file=sys.stderr)
    sys.exit(1)

problems = []
checked = 0
for m in data:
    mid = str(m.get("id", ""))
    if not (mid.startswith("gsb/") or "gemini" in mid.lower()):
        continue
    checked += 1
    caps = m.get("capabilities") or {}
    ctx_len = m.get("context_length")
    max_out = m.get("max_output_tokens", m.get("max_completion_tokens"))
    think_fmt = caps.get("thinkingFormat")

    if not isinstance(ctx_len, int) or ctx_len < 4096:
        problems.append(f"{mid}: context_length={ctx_len!r} missing or implausible")
    if not isinstance(max_out, int) or max_out < 256:
        problems.append(f"{mid}: max_output_tokens={max_out!r} missing or implausible")
    if think_fmt is None:
        problems.append(f"{mid}: capabilities.thinkingFormat missing")

    if strict:
        if not isinstance(m.get("reasoning"), bool):
            problems.append(f"{mid}: missing boolean 'reasoning'")
        supp = m.get("supported_parameters")
        if not isinstance(supp, list) or "tools" not in supp:
            problems.append(f"{mid}: supported_parameters={supp!r} missing or incomplete")
        thinking = m.get("thinking")
        if not isinstance(thinking, dict):
            problems.append(f"{mid}: 'thinking' object missing")
        elif not isinstance(thinking.get("efforts"), list):
            problems.append(f"{mid}: thinking.efforts missing")

if checked == 0:
    print(f"ERROR: {label}: no gsb/* or gemini-* models found", file=sys.stderr)
    sys.exit(1)

if problems:
    print(f"FAILED: {label}: {len(problems)} problem(s) across {checked} model(s):", file=sys.stderr)
    for p in problems:
        print(f"  - {p}", file=sys.stderr)
    sys.exit(1)

print(f"OK: {label}: {checked} model(s) carry context_length, max_output_tokens, and thinkingFormat"
      + (", reasoning, supported_parameters, and thinking.efforts." if strict else "."))
PYEOF
}

# levels_report <json-file> <label>
#
# Prints, per model, whether the thinking level list survived. This is the field a
# harness needs to render a low/medium/high selector.
levels_report() {
  python3 - "$1" "$2" <<'PYEOF'
import json
import sys

path, label = sys.argv[1], sys.argv[2]
with open(path, "r", encoding="utf-8") as f:
    payload = json.load(f)

print(f"{label} thinking levels:")
for m in payload.get("data", []):
    mid = str(m.get("id", ""))
    if not (mid.startswith("gsb/") or "gemini" in mid.lower()):
        continue
    caps = m.get("capabilities") or {}
    levels = caps.get("thinkingLevels")
    if levels is None:
        levels = (m.get("thinking") or {}).get("efforts")
    status = "MISSING" if not levels else ",".join(str(x) for x in levels)
    print(f"  {mid:32s} {status}")
PYEOF
}

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

if [ "$COMPARE" = "1" ]; then
  fetch "$BRIDGE_URL" "$TMP_DIR/bridge.json" "$BRIDGE_KEY"
  fetch "$ROUTER_URL" "$TMP_DIR/router.json" "$ROUTER_KEY"

  check_payload "$TMP_DIR/bridge.json" "bridge" 1

  # The router is judged separately: it may normalize the payload, but it must not
  # drop the thinking levels a harness needs to render a low/medium/high selector.
  python3 - "$TMP_DIR/bridge.json" "$TMP_DIR/router.json" <<'PYEOF'
import json
import sys

def load(path):
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)

def models(payload):
    out = {}
    for m in payload.get("data", []):
        mid = str(m.get("id", ""))
        if not (mid.startswith("gsb/") or "gemini" in mid.lower()):
            continue
        caps = m.get("capabilities") or {}
        levels = caps.get("thinkingLevels")
        if levels is None:
            levels = (m.get("thinking") or {}).get("efforts")
        out[mid] = {
            "levels": levels or [],
            "format": caps.get("thinkingFormat"),
            "reasoning": caps.get("reasoning"),
            "ctx": m.get("context_length"),
            "max_out": m.get("max_output_tokens", m.get("max_completion_tokens")),
        }
    return out

bridge = models(load(sys.argv[1]))
router = models(load(sys.argv[2]))

problems = []
preserved = 0
for mid, want in bridge.items():
    # A router may rename the model with a provider prefix; match by suffix.
    match = next((v for k, v in router.items() if k == mid or k.endswith("/" + mid) or mid.endswith("/" + k)), None)
    if match is None:
        problems.append(f"{mid}: not exposed by the router at all")
        continue
    if want["levels"] and not match["levels"]:
        problems.append(f"{mid}: router dropped thinking levels {want['levels']}")
    elif want["levels"]:
        preserved += 1
    if want["format"] and not match["format"]:
        problems.append(f"{mid}: router dropped capabilities.thinkingFormat")
    for field in ("ctx", "max_out"):
        if match[field] != want[field]:
            problems.append(f"{mid}: {field} changed {want[field]!r} -> {match[field]!r}")

print(f"bridge models: {len(bridge)} | router preserved thinking levels for: {preserved}")
for mid, want in bridge.items():
    match = next((v for k, v in router.items() if k == mid or k.endswith("/" + mid) or mid.endswith("/" + k)), {})
    got = ",".join(match.get("levels") or []) or "MISSING"
    print(f"  {mid:32s} bridge={','.join(want['levels']) or 'none':20s} router={got}")

if problems:
    print(f"FAILED: router lost or altered metadata ({len(problems)} problem(s)):", file=sys.stderr)
    for p in problems:
        print(f"  - {p}", file=sys.stderr)
    print("This is a router-side loss: the bridge advertised the field and the router "
          "did not forward it.", file=sys.stderr)
    sys.exit(1)
print("OK: router preserves thinking levels, thinkingFormat, and limits for every bridged model.")
PYEOF
else
  fetch "$BASE_URL" "$TMP_DIR/models.json" "$API_KEY"
  check_payload "$TMP_DIR/models.json" "endpoint" 1
  levels_report "$TMP_DIR/models.json" "endpoint"
fi
