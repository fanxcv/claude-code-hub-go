#!/bin/bash
# Go 侧静态分析一键复跑。
#
# 用途：装好固定版本的工具后跑 staticcheck + go vet 全量，按「规则大组 / 规则号 / 文件」聚合，
#      终端只打摘要（原始输出落 /tmp/sa/out/，量大，别直接倒进终端）。
#
# 为什么固定版本：不同版本的 staticcheck 检查集合会变（规则新增/删除/改名），
# 报告里的计数只有同版本可比。2025.1.1 是本仓库本轮报告的取值基线。
#
# 用法：
#   bash scripts/static-analysis.sh            # 摘要（默认）
#   bash scripts/static-analysis.sh --full     # 摘要 + 原始输出尾部
#   bash scripts/static-analysis.sh --escape   # 附带热点包的逃逸分析计数
#
# 前置：go 1.25+；GOPROXY 可达（本仓库用 goproxy.cn）。工具缺失时会自动 go install。
set -u

STATICCHECK_VERSION="2025.1.1"
GOIMPORTS_VERSION="v0.41.0"
GOBIN_DEFAULT="${GOBIN:-/root/go/bin}"
SC="$GOBIN_DEFAULT/staticcheck"
OUT_DIR="/tmp/sa/out"
REPO_GO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/go"

FULL=0
ESCAPE=0
for arg in "$@"; do
  case "$arg" in
    --full) FULL=1 ;;
    --escape) ESCAPE=1 ;;
    *) echo "未知参数: $arg" >&2; exit 2 ;;
  esac
done

mkdir -p "$OUT_DIR"
cd "$REPO_GO_DIR" || { echo "找不到 go 模块目录: $REPO_GO_DIR" >&2; exit 1; }

# ---- 工具 ----
if [ ! -x "$SC" ]; then
  echo "安装 staticcheck@$STATICCHECK_VERSION …"
  GOBIN="$GOBIN_DEFAULT" go install "honnef.co/go/tools/cmd/staticcheck@$STATICCHECK_VERSION" || exit 1
fi
if [ ! -x "$GOBIN_DEFAULT/goimports" ]; then
  GOBIN="$GOBIN_DEFAULT" go install "golang.org/x/tools/cmd/goimports@$GOIMPORTS_VERSION" >/dev/null 2>&1 || true
fi
echo "staticcheck: $("$SC" -version)"

# ---- staticcheck ----
"$SC" ./... >"$OUT_DIR/staticcheck.txt" 2>"$OUT_DIR/staticcheck.err"
SC_EXIT=$?
TOTAL=$(wc -l <"$OUT_DIR/staticcheck.txt")

# 只统计真正的发现行（以 <path>:<line>:<col>: 开头），排除工具自身的汇总行。
grep -E '^[^ ]+\.go:[0-9]+:[0-9]+: ' "$OUT_DIR/staticcheck.txt" >"$OUT_DIR/findings.txt" || true
# 多行诊断（SA5011/SA4023 等会多打一行续行解释）计入行数但不计独立发现。
CONT_LINES=$(grep -vcE '\([A-Z]+[0-9]+\)[[:space:]]*$' "$OUT_DIR/findings.txt" || true)
findings_only=$(( $(wc -l <"$OUT_DIR/findings.txt") - CONT_LINES ))

echo
echo "=== staticcheck（exit=$SC_EXIT，输出 $TOTAL 行）==="
echo "  独立发现: $findings_only 条（另有 $CONT_LINES 行是多行诊断的续行，非独立发现）"
echo "--- 按大组（SA=疑似缺陷 / S1=简化 / ST=风格 / QF=快速修复 / U=未使用 / 其他）---"
awk '{ if (match($0, /\(([A-Z]+[0-9]+)\)[[:space:]]*$/)) { r=substr($0, RSTART+1, RLENGTH-2);
         if (r ~ /^SA/) g="SA"; else if (r ~ /^S1/) g="S1"; else if (r ~ /^ST/) g="ST"; else if (r ~ /^QF/) g="QF"; else if (r ~ /^U/) g="U1000"; else g="其他";
         print g } }' "$OUT_DIR/findings.txt" \
  | sort | uniq -c | sort -rn | awk '{printf "  %-6s %s\n", $2, $1}'
echo "--- 按规则号（Top 12）---"
awk '{ if (match($0, /\(([A-Z]+[0-9]+)\)[[:space:]]*$/)) { r=substr($0, RSTART+1, RLENGTH-3); print r } }' "$OUT_DIR/findings.txt" \
  | sort | uniq -c | sort -rn | head -12 | awk '{printf "  %-8s %s\n", $2, $1}'
echo "--- 测试文件 vs 生产文件（按行数；含 $CONT_LINES 行续行）---"
test_lines=$(grep -c '_test\.go:' "$OUT_DIR/findings.txt" || true)
printf "  测试文件: %s 行 -> 独立发现 %s 条\n" "$test_lines" "$(( test_lines - $(grep '_test\.go:' "$OUT_DIR/findings.txt" | grep -vcE '\([A-Z]+[0-9]+\)[[:space:]]*$' || true) ))"
printf "  生产文件: %s 行 -> 独立发现 %s 条\n" "$(( $(wc -l <"$OUT_DIR/findings.txt") - test_lines ))" "$(( findings_only - ( test_lines - $(grep '_test\.go:' "$OUT_DIR/findings.txt" | grep -vcE '\([A-Z]+[0-9]+\)[[:space:]]*$' || true) ) ))"
echo "--- SA 组明细（疑似缺陷，逐条）---"
grep -E '\((SA[0-9]+)\)[[:space:]]*$' "$OUT_DIR/findings.txt" || echo "  （无）"
echo "--- 按文件（Top 12）---"
sed -E 's#:[0-9]+:[0-9]+:.*##' "$OUT_DIR/findings.txt" | sort | uniq -c | sort -rn | head -12 | sed 's/^/  /'
echo "--- 多行诊断的续行（非独立发现，供定位上下文）---"
printf "  条数: %s\n" "$CONT_LINES"
[ "$CONT_LINES" -gt 0 ] && grep -vE '\([A-Z]+[0-9]+\)[[:space:]]*$' "$OUT_DIR/findings.txt" | head -5 | sed 's/^/  /'

# ---- go vet ----
echo
echo "=== go vet ./...（默认 analyzer 集，即仓库门禁口径）==="
go vet ./... >"$OUT_DIR/vet.txt" 2>&1
VET_EXIT=$?
printf "  exit=%s  输出行数=%s\n" "$VET_EXIT" "$(wc -l <"$OUT_DIR/vet.txt")"
[ "$VET_EXIT" -ne 0 ] && head -10 "$OUT_DIR/vet.txt"

# ---- 逃逸分析（可选）----
if [ "$ESCAPE" -eq 1 ]; then
  echo
  echo "=== 逃逸分析（热点包，-gcflags=-m 聚合计数）==="
  for p in internal/forward internal/gate internal/ingress internal/convert internal/dataplane internal/store internal/jobs; do
    out=$(go build -gcflags="-m" "./$p" 2>&1)
    printf "  %-22s leaking=%4s moved=%4s escapes=%4s\n" "$p" \
      "$(printf '%s\n' "$out" | grep -c 'leaking param')" \
      "$(printf '%s\n' "$out" | grep -c 'moved to heap')" \
      "$(printf '%s\n' "$out" | grep -c 'escapes to heap')"
  done
  echo "  最热三包里 leaking param 涉及 []byte/string 的条数（0 = 无「可避免的缓冲逃逸」信号）："
  for p in internal/forward internal/gate internal/ingress; do
    printf "    %-20s %s\n" "$p" "$(go build -gcflags="-m" "./$p" 2>&1 | grep 'leaking param' | grep -cE '\[\]byte|string')"
  done
fi

echo
echo "=== 原始输出 ==="
echo "  $OUT_DIR/findings.txt   （staticcheck 发现，按行）"
echo "  $OUT_DIR/vet.txt        （go vet）"
if [ "$FULL" -eq 1 ]; then
  echo "--- findings.txt 尾部 20 行 ---"
  tail -20 "$OUT_DIR/findings.txt"
fi
