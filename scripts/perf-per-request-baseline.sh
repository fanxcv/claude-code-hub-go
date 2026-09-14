#!/usr/bin/env bash
# 每请求成本基线：真实服务 + 仓库既有 50x8MiB 夹具，采 CPU/RSS/GC。
#
# 为什么用 /proc + gctrace 而不是 pprof：本进程没有 pprof 端点（暴露端点是并行的
# perf-surface 路的交付物），而 /proc 的 CPU 时间与 GODEBUG=gctrace 的分配量是**外部可读**的，
# 不依赖被测二进制开任何东西。两者相加足以给出「每请求 CPU 秒」「每请求分配字节」。
set -uo pipefail

SERVICE_PID="${SERVICE_PID:?用法: SERVICE_PID=<cchd pid> $0 <标签>}"
LABEL="${1:?缺失标签}"
LOG="$2"
REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

read_cpu() { awk '{print $14+$15}' "/proc/$SERVICE_PID/stat"; }   # utime+stime（100 tick/秒）
read_rss() { awk '/VmRSS/{print $2}' "/proc/$SERVICE_PID/status"; }
read_hwm() { awk '/VmHWM/{print $2}' "/proc/$SERVICE_PID/status"; }
read_gc() { grep -c '^gc ' "$LOG"; }

case "$SERVICE_PID" in
  ''|*[!0-9]*) echo "SERVICE_PID 必须是单个整数，收到 [$SERVICE_PID]" >&2; exit 2 ;;
esac
# 多实例是真实故障源（早前就踩过：两个 cchd 同时跑，探针的 TARGET_PID 参数直接拒收多值）。
INSTANCES=$(pgrep -x cchd-perf | wc -l)
if [ "$INSTANCES" -ne 1 ]; then echo "期望 1 个 cchd-perf 实例，实际 $INSTANCES" >&2; exit 2; fi

snapshot() {
  printf '%s cpu=%s rss_kib=%s hwm_kib=%s gc=%s\n' "$1" "$(read_cpu)" "$(read_rss)" "$(read_hwm)" "$(read_gc)"
}

echo "--- [$LABEL] 空闲 10s 基线 ---"
CPU0=$(read_cpu); GC0=$(read_gc); HWM0=$(read_hwm)
sleep 10
CPU1=$(read_cpu); GC1=$(read_gc)
IDLE_SECS=10
echo "空闲: CPU $(awk -v a=$CPU0 -v b=$CPU1 'BEGIN{printf "%.3f", (b-a)/100}')s/${IDLE_SECS}s  GC增量 $((GC1-GC0))  HWM=$(awk -v h=$HWM0 'BEGIN{printf "%.1f", h/1024}')MiB"

echo "--- [$LABEL] 负载：50 并发 x 8 MiB ---"
pkill -f "mock-upstream.mjs --port 39217" 2>/dev/null; sleep 1
CPU2=$(read_cpu); GC2=$(read_gc)

CCH_API_KEY=sk-loadtest-50 CCH_BASE_URL=http://127.0.0.1:13599 CCH_MODEL=gpt-5.6 \
CCH_TARGET_PID="$SERVICE_PID" CCH_CONCURRENCY=50 CCH_STREAM_BYTES=8388608 \
CCH_MOCK_PORT=39217 \
node "$REPO/tests/load/node-concurrency-budget/probe.mjs" > "/tmp/probe-$LABEL.json" 2>"/tmp/probe-$LABEL.err"
PROBE_EXIT=$?

CPU3=$(read_cpu); GC3=$(read_gc); HWM1=$(read_hwm); RSS1=$(read_rss)
LOAD_TICKS=$((CPU3-CPU2))

echo "探针退出码: $PROBE_EXIT"
echo "负载期: CPU $(awk -v t=$LOAD_TICKS 'BEGIN{printf "%.3f", t/100}')s  GC增量 $((GC3-GC2))  HWM=$(awk -v h=$HWM1 'BEGIN{printf "%.1f", h/1024}')MiB  RSS=$(awk -v h=$RSS1 'BEGIN{printf "%.1f", h/1024}')MiB"

echo "--- 负载期 gctrace 汇总（分配速率、堆目标与停顿）---"
# 用 python 解：mawk 不支持三参数 match，而 gctrace 的行结构需要按位置取多个字段。
python3 - "$LOG" <<'PY'
import re, sys
clock = cpu = 0.0
gcs = []
for line in open(sys.argv[1], encoding="utf-8", errors="replace"):
    if not line.startswith("gc "):
        continue
    m = re.search(r"([0-9.]+)\+([0-9.]+)\+([0-9.]+) ms clock, ([0-9.]+)(?:\+([0-9.]+))?/([0-9.]+)/([0-9.]+)(?:\+([0-9.]+))? ms cpu", line)
    if m:
        a, b, c = (float(m.group(i)) for i in (1, 2, 3))
        cpuParts = [float(g) for g in (m.group(4), m.group(5) or 0, m.group(6), m.group(7), m.group(8) or 0)]
        clock += a + b + c
        cpu += sum(cpuParts)
    goal = re.search(r"(\d+) MB goal", line)
    gcs.append(int(goal.group(1)) if goal else 0)
if gcs:
    print(f"gc 次数={len(gcs)}  停顿总计={clock:.1f}ms  均={clock/len(gcs):.2f}ms  CPU 总计={cpu:.1f}ms  末次堆目标={gcs[-1]}MB")
else:
    print("无 gctrace 行")
PY
echo "--- heap 目标（末条 gctrace）---"
grep '^gc ' "$LOG" | tail -1

echo "--- 探针关键数字 ---"
node -e '
  const fs = require("fs");
  const text = fs.readFileSync(process.argv[1], "utf8");
  const line = text.trim().split("\n").filter((l) => l.startsWith("{")).pop() || "{}";
  const report = JSON.parse(line);
  const m = report.memory || {}, r = report.requests || {};
  console.log(JSON.stringify({
    verdict: report.verdict, launched: r.launched, success: r.success, failure: r.failure,
    receivedBytes: r.receivedBytes,
    baselineRssMiB: m.baselineRssBytes ? +(m.baselineRssBytes / 1048576).toFixed(1) : null,
    peakRssMiB: m.peakRssBytes ? +(m.peakRssBytes / 1048576).toFixed(1) : null,
    deltaRssMiB: m.deltaRssBytes ? +(m.deltaRssBytes / 1048576).toFixed(1) : null,
    heldMs: m.heldMs, ttfbP50: r.ttfbMs && r.ttfbMs.p50, ttfbP99: r.ttfbMs && r.ttfbMs.p99,
  }, null, 1));
' "/tmp/probe-$LABEL.json" 2>&1 | tail -18
