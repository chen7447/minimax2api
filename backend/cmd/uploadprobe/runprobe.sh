#!/bin/bash
# Run the upload probe with one live account's credentials.
#
# Reads the account straight out of the gateway's own state file so the secrets
# are never copied off the box, exported into this process only, and never
# echoed. Usage: runprobe.sh <account_id>
set -euo pipefail

ACCOUNT="${1:?usage: runprobe.sh <account_id>}"
STATE=/opt/minimax2api/data/app.json

eval "$(sudo python3 - "$ACCOUNT" "$STATE" <<'PY'
import json, shlex, sys
account, path = sys.argv[1], sys.argv[2]
data = json.load(open(path))
match = [a for a in data["accounts"] if a.get("id") == account]
if not match:
    sys.exit("account %s not found" % account)
a = match[0]
for env, key in [("MM_TOKEN", "token"), ("MM_USER_ID", "userId"), ("MM_AGENT_ID", "agentID"),
                 ("MM_DEVICE_ID", "deviceID"), ("MM_UUID", "uuid"), ("MM_REGION", "region"),
                 ("MM_BASE_URL", "baseURL")]:
    value = a.get(key) or ""
    print("export %s=%s" % (env, shlex.quote(str(value))))
PY
)"

exec /tmp/uploadprobe
