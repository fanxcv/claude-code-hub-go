#!/usr/bin/env bash
# 真实的 output:'export' 尝试：把整仓复制到临时目录（硬链接 node_modules），只把
# `output: "standalone"` 改成 `output: "export"`，记录原始编译错误——作为
# 「静态导出到底撞到什么」的一手证据。
#
# 用法：bash go/cmd/uipoc/tools/export-attempt.sh [临时目录]
#   默认在 $(mktemp -d) 下工作；日志写在 <临时目录>/../uiexport-build.log。
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
W="$(cd "$HERE/../../../.." && pwd)"          # 仓根
D="${1:-$(mktemp -d "${TMPDIR:-/tmp}/uiexport.XXXXXX")}"
LOG="$D.build.log"

rm -rf "$D"
mkdir -p "$D"
# 整仓复制，排除体积大且需重做的目录。
tar -C "$W" --exclude=./node_modules --exclude=./.git --exclude=./.next --exclude=./go \
    -cf - . | tar -C "$D" -xf -
cp -al "$W/node_modules" "$D/node_modules"
ln -s "$W/node_modules/.bin" "$D/node_modules/.bin" 2>/dev/null || true
# 唯一的改动：standalone -> export。
sed -i 's/output: "standalone"/output: "export"/' "$D/next.config.ts"
grep -n 'output:' "$D/next.config.ts" >>"$LOG"

cd "$D" || exit 1
echo "=== 开始 next build（export 模式）$(date '+%H:%M:%S') ===" >>"$LOG"
NEXT_TELEMETRY_DISABLED=1 timeout 900 bunx next build >>"$LOG" 2>&1
echo "=== 结束 exit=$? $(date '+%H:%M:%S') ===" >>"$LOG"
echo "=== out 目录 ===" >>"$LOG"
ls -la "$D/out" 2>&1 | head -20 >>"$LOG"
echo "日志：$LOG"
