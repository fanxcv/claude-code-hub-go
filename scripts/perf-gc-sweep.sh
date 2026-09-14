#!/usr/bin/env bash
# GC 参数扫描：同一负载（50 并发 x 8 MiB 流式），只改 GC 参数，看 CPU / 停顿 / 内存三者怎么换。
#
# 为什么要扫：生产的 compose 设了 GOMEMLIMIT，而 GOGC 用默认。GC 是「拿 CPU 换内存」的旋钮，
# 但这个服务的内存地板本来就只有百来 MiB，扫一遍才知道值不值得动。
#
# 每轮都重启服务（GC 参数是启动时的环境变量，不能热改），故每轮都是干净的进程状态。
set -uo pipefail

REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
BIN=/tmp/cchd-perf
SERVICE_ENV_COMMON=(
  DSN=postgres://postgres:postgres@127.0.0.1:5432/cch_loadtest
  REDIS_URL=redis://127.0.0.1:6379/15
  ADMIN_TOKEN=perf-token
  PORT=13599
  AUTO_MIGRATE=false
  CCH_EGRESS_PAGES=off
  GODEBUG=gctrace=1
)

start_service() {
  pkill -x cchd-perf 2>/dev/null; pkill -f "^$BIN$" 2>/dev/null; sleep 2
  LOG="/tmp/cchd-gc-$1.log"
  rm -f "$LOG"
  env "${SERVICE_ENV_COMMON[@]}" "$@" setsid "$BIN" > "$LOG" 2>&1 < /dev/null &
  sleep 8
  curl -s -m 5 -o /dev/null -w "" http://127.0.0.1:13599/readyz
}

for config in "GOGC=100 GOMEMLIMIT=256MiB" "GOGC=200 GOMEMLIMIT=256MiB" "GOGC=100" "GOGC=400 GOMEMLIMIT=256MiB"; do
  label=$(echo "$config" | tr ' =' '__')
  # shellcheck disable=SC2086
  start_service $config
  pid=$(pgrep -x cchd-perf | head -1)
  if [ -z "$pid" ]; then echo "[$config] 服务未起"; continue; fi

  log="/tmp/cchd-gc-$label.log"
  # GODEBUG 已在公共 env 里，这里按 label 重新落日志名
  rm -f "/tmp/cchd-$label.log"; : > "/tmp/cchd-$label.log"
  cpu0=$(awk '{print $14+$15}' "/proc/$pid/stat")
  gc0=$(grep -c '^gc ' "$log" 2>/dev/null || echo 0)

  pkill -f "mock-upstream.mjs --port 39217" 2>/dev/null; sleep 1
  CCH_API_KEY=sk-loadtest-50 CCH_BASE_URL=http://127.0.0.1:13599 CCH_MODEL=gpt-5.6 \
  CCH_TARGET_PID="$pid" CCH_CONCURRENCY=50 CCH_STREAM_BYTES=8388608 CCH_MOCK_PORT=39217 \
  node "$REPO/tests/load/node-concurrency-budget/probe.mjs" > "/tmp/probe-gc-$label.json" 2>"/tmp/probe-gc-$label.err"
  exit_code=$?

  cpu1=$(awk '{print $14+$15}' "/proc/$pid/stat")
  alive=$(ps -C cchd-perf -o pid= | wc -l)
  panic=$(grep -c 'panic' "$log")

  stats=$(python3 - "$log" <<'PY'
import json, re, sys
clock = 0.0
gcs = []
for line in open(sys.argv[1], encoding="utf-8", errors="replace"):
    if not line.startswith("gc "):
        continue
    m = re.search(r"([0-9.]+)\+([0-9.]+)\+([0-9.]+) ms clock", line)
    if m:
        clock += sum(float(x) for x in m.groups())
    g = re.search(r"(\d+) MB goal", line)
    gcs.append(int(g.group(1)) if g else 0)
print(json.dumps({"gcCount": len(gcs), "pauseTotalMs": round(clock, 1),
                  "pauseAvgMs": round(clock / len(gcs), 2) if gcs else 0,
                  "lastGoalMB": gcs[-1] if gcs else 0}))
PY
)

  report=$(python3 - "$REPO" "$label" <<'PY'
import json, sys
repo, label = sys.argv[1], sys.argv[2]
try:
    text = open(f"/tmp/probe-gc-{label}.json", encoding="utf-8").read().strip().split("\n")
    data = json.loads([l for l in text if l.startswith("{")][-1])
except Exception as exc:  # noqa: BLE001
    print(json.dumps({"error": str(exc)})); raise SystemExit
m, r = data.get("memory", {}), data.get("requests", {})
print(json.dumps({"verdict": data.get("verdict"), "success": r.get("success"),
                  "peakRssMiB": round(m.get("peakRssBytes", 0) / 1048576, 1),
                  "baselineRssMiB": round(m.get("baselineRssBytes", 0) / 1048576, 1),
                  "deltaRssMiB": round(m.get("deltaRssBytes", 0) / 1048576, 1),
                  "ttfbP50": round(r.get("ttfbMs", {}).get("p50") or 0, 1),
                  "ttfbP99": round(r.get("ttfbMs", {}).get("p99") or 0, 1)}))
PY
)

  echo "[$config] 退出码=$exit_code 存活实例=$alive panic=$panic CPU负载期=$(awk -v a=$cpu0 -v b=$cpu1 'BEGIN{printf "%.3f", (b-a)/100}')s GC=$stats 内存=$report"
done
pkill -x cchd-perf 2>/dev/null
