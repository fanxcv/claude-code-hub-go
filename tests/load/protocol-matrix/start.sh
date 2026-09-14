#!/usr/bin/env bash
# 三协议转换矩阵 · 起一个**本地** cchd（Go 数据面），不动生产、不依赖 Node。
#
# 关键环境变量（依据 go/internal/egress/decision.go 的归属语义）：
#   CCH_EGRESS_MODE=go    但**白名单为空时全部回退 Node**（ReasonGoNoRules）⇒ 必须显式给 ROUTES
#   CCH_EGRESS_ROUTES='* /v1/*'  让 Go 独占数据面；其余路径由前端自答
#   CCH_EGRESS_PAGES=off         非 API 路径由本进程自答（本机没有 Node 可反代）
#   AUTO_MIGRATE=false           库结构已由既有测试库/smoke 库提供
#
# 用法：bash start.sh          （后台起进程，PID 写到 .cchd.pid 与 .cchd.port）
#      bash start.sh stop     （停进程）
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PIDFILE="$HERE/.cchd.pid"
PORTFILE="$HERE/.cchd.port"
LOGFILE="$HERE/.cchd.log"

if [ "${1:-start}" = "stop" ]; then
  if [ -f "$PIDFILE" ]; then
    pid=$(cat "$PIDFILE")
    if kill -0 "$pid" 2>/dev/null; then kill "$pid" 2>/dev/null || true; sleep 1; kill -9 "$pid" 2>/dev/null || true; fi
    rm -f "$PIDFILE" "$PORTFILE"
    echo "已停 cchd"
  else
    echo "无 pid 文件（可能未启动）"
  fi
  exit 0
fi

DSN="${MATRIX_DSN:-${CCH_TEST_DSN:-postgres://postgres:postgres@127.0.0.1:5432/cch_smoke}}"
REDIS_URL="${MATRIX_REDIS_URL:-${CCH_TEST_REDIS_URL:-redis://127.0.0.1:6379/15}}"

# 取一个空闲端口
port=$(python3 - <<'PY'
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()
PY
)
internal=$((port + 1))

BIN="${MATRIX_CCHD_BIN:-/tmp/cchd-matrix}"
if [ ! -x "$BIN" ]; then
  echo "缺少 cchd 二进制（${BIN}）。先：cd go && CGO_ENABLED=0 go build -o ${BIN} ./cmd/cchd"
  exit 1
fi

echo "启动 cchd：PORT=${port} DSN=${DSN%%:*}://***  REDIS_URL(dbid)=${REDIS_URL##*/}"
env -i PATH="$PATH" HOME="$HOME" \
  PORT="$port" \
  CCH_INTERNAL_PORT="$internal" \
  DSN="$DSN" \
  REDIS_URL="$REDIS_URL" \
  CCH_EGRESS_MODE=go \
  CCH_EGRESS_ROUTES='* /v1/*' \
  CCH_EGRESS_PAGES=off \
  AUTO_MIGRATE=false \
  ADMIN_TOKEN=matrix-admin-token \
  "$BIN" >"$LOGFILE" 2>&1 &
echo $! >"$PIDFILE"
echo "$port" >"$PORTFILE"

for i in $(seq 1 40); do
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:${port}/v1/_ping" || true)
  if [ "$code" = "200" ]; then
    echo "  /v1/_ping=200（$((i * 250))ms）"
    break
  fi
  sleep 0.25
done
ready=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:${port}/readyz" || true)
echo "  readyz=${ready}（503 表示未就绪；日志见 $LOGFILE）"
echo "  BASE=http://127.0.0.1:${port}"
