#!/usr/bin/env bash
# 断言 UI 产物已收编到 go/internal/uiapp/assets/（release.yml 与 docker.yml 共用）。
#
# 为什么必须拦：`go:embed all:assets` 只要求目录存在（`.gitkeep` 即可匹配），
# 缺产物时**编译照样成功**，但二进制会是个没有界面的壳。
#
# 用法（仓库根）：bash scripts/assert-uiapp-assets.sh
set -uo pipefail

ASSETS_DIR="${1:-go/internal/uiapp/assets}"
MIN_FILES="${MIN_ASSET_FILES:-300}"

if [ ! -f "$ASSETS_DIR/MANIFEST.json" ]; then
  echo "::error::UI 产物未收编（缺 $ASSETS_DIR/MANIFEST.json）"
  exit 1
fi

n="$(find "$ASSETS_DIR" -type f | wc -l | tr -d ' ')"
echo "assets 文件数：${n}（压缩态实测约 500）"
if [ "$n" -lt "$MIN_FILES" ]; then
  echo "::error::UI 产物文件数过少（${n} < ${MIN_FILES}），疑似导出失败"
  exit 1
fi
