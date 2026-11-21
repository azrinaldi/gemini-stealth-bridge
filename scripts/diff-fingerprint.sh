#!/bin/sh
# Compare a gsb RequestDump JSON file against a reference Chrome fingerprint JSON file.
# Usage: scripts/diff-fingerprint.sh <dump.json> <reference.json>

set -eu

if [ "$#" -lt 2 ]; then
  echo "Usage: $0 <dump.json> <reference.json>" >&2
  exit 2
fi

DUMP_FILE="$1"
REF_FILE="$2"

if [ ! -f "$DUMP_FILE" ]; then
  echo "Error: dump file not found: $DUMP_FILE" >&2
  exit 2
fi

if [ ! -f "$REF_FILE" ]; then
  echo "Error: reference file not found: $REF_FILE" >&2
  exit 2
fi

python3 - "$DUMP_FILE" "$REF_FILE" <<'PYEOF'
import json
import sys

dump_path, ref_path = sys.argv[1], sys.argv[2]
with open(dump_path, "r", encoding="utf-8") as f:
    dump = json.load(f)
with open(ref_path, "r", encoding="utf-8") as f:
    ref = json.load(f)

dump_order = [h.lower() for h in dump.get("headerOrder", [])]
ref_order = [h.lower() for h in ref.get("headerOrder", [])]

dump_headers = {k.lower(): v for k, v in dump.get("headers", {}).items()}
ref_headers = {k.lower(): v for k, v in ref.get("headers", {}).items()}

missing = [h for h in ref_order if h not in dump_order]
extra = [h for h in dump_order if h not in ref_order]

print(f"=== Fingerprint Diff: {dump_path} vs {ref_path} ===")
print(f"Kind: {dump.get('kind', 'unknown')} | Method: {dump.get('method', '')}")

if missing:
    print(f"MISSING headers ({len(missing)}): {', '.join(missing)}")
else:
    print("MISSING headers: none")

if extra:
    print(f"EXTRA headers ({len(extra)}): {', '.join(extra)}")
else:
    print("EXTRA headers: none")

common_dump = [h for h in dump_order if h in ref_order]
common_ref = [h for h in ref_order if h in dump_order]
order_mismatches = []
for idx, (dh, rh) in enumerate(zip(common_dump, common_ref)):
    if dh != rh:
        order_mismatches.append(f"  pos {idx}: dump='{dh}' vs ref='{rh}'")

if order_mismatches:
    print("ORDER mismatches:")
    for m in order_mismatches:
        print(m)
else:
    print("ORDER matches for all shared headers.")

for key in sorted(set(list(dump_headers.keys()) + list(ref_headers.keys()))):
    if key == "accept-encoding" or key.startswith("sec-ch-ua"):
        dv = dump_headers.get(key)
        rv = ref_headers.get(key)
        if dv != rv and rv is not None:
            print(f"VALUE diff [{key}]: dump={dv} vs ref={rv}")

if missing or order_mismatches:
    sys.exit(1)
sys.exit(0)
PYEOF
