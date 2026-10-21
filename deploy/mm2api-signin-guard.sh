#!/bin/bash
# mm2api 签到守卫 —— 2026-10-12 教训：swap 抖动把进程饿死 5 小时，09:05 槽位
# 错过且当天未补。守卫每小时跑一次：服务不在就拉起，健康检查失败就重启，
# 调度显示已到点（nextRunAt <= now）且没在跑就补触发签到。
# 随机槽位上线后 nextRunAt 由后端计算（每天变化，确定性 seed），守卫只看这个值。
set -u
# The admin key never lives in this file: it is read from the environment or a
# 0600 root-owned file. Install with:
#   printf '%s\n' 'mm2api-…' | sudo tee /etc/mm2api-guard.key >/dev/null
#   sudo chmod 600 /etc/mm2api-guard.key
KEY="${MINIMAX2API_ADMIN_KEY:-$(cat /etc/mm2api-guard.key 2>/dev/null)}"
if [ -z "$KEY" ]; then
  logger -t mm2api-guard "no admin key (env MINIMAX2API_ADMIN_KEY or /etc/mm2api-guard.key), bail"
  exit 1
fi
BASE="http://127.0.0.1:8080"
log() { logger -t mm2api-guard "$*"; }

if ! systemctl is-active --quiet minimax2api.service; then
  log "service inactive, restarting"
  systemctl restart minimax2api.service
  sleep 8
fi

if ! curl -s --noproxy '*' --max-time 90 "$BASE/health" | grep -q '"status":"ok"'; then
  log "health check failed, restarting service"
  systemctl restart minimax2api.service
  sleep 12
fi

state=$(curl -s --noproxy '*' --max-time 90 -H "Authorization: Bearer $KEY" "$BASE/admin/api/signin")
if [ -z "$state" ]; then
  log "signin API unreachable, bail"
  exit 0
fi

parsed=$(printf '%s' "$state" | python3 -c '
import json,sys,time
d=json.load(sys.stdin)
running=d.get("running",False)
nra=d.get("nextRunAt") or ""
due=False
if nra:
    try:
        due = time.time() >= time.fromisoformat(nra.replace("Z","+00:00")).timestamp()
    except Exception:
        pass
print("RUNNING=%s" % str(bool(running)).lower())
print("DUE=%s" % str(bool(due)).lower())
')
[ -n "$parsed" ] || { log "state parse failed, bail"; exit 0; }
eval "$parsed"

if [ "$DUE" = "true" ] && [ "$RUNNING" = "false" ]; then
  log "sweep overdue (nextRunAt passed), triggering signin/run"
  code=$(curl -s --noproxy '*' --max-time 30 -o /dev/null -w '%{http_code}' \
    -X POST -H "Authorization: Bearer $KEY" "$BASE/admin/api/signin/run")
  log "signin/run responded $code"
else
  log "ok (due=$DUE running=$RUNNING)"
fi
