#!/usr/bin/env bash
# 三协议转换矩阵 · 环境准备（幂等）
#
# 做三件事：
#   1) 在一个**可弃库**里造 1 个用户 + 1 个密钥 + 3 个供应商（分别声明 anthropic / chat / responses 三种协议）；
#   2) 三个供应商都指向**同一台真·多协议上游**，靠 model_redirects 把别名映射到上游真实模型名；
#   3) 打印客户端密钥与矩阵所需的别名（调用方据此跑 run.mjs）。
#
# 为什么用可弃库而不是共享测试库：共享库里有数千条历史供应商（本地实测），
# 会让选路出现「谁都可能被选中」的不确定性；本脚本自带清场（只删自己命名前缀的行），
# 因此可在同一库上反复重跑。
#
# 上游与凭据（用户提供，仅测试用途）：
#   MATRIX_UPSTREAM_URL   **必填**（无内置默认值，避免默认跑时误连到他人的环境）；形如 http://<your-upstream>:4321
#   MATRIX_UPSTREAM_KEY   默认 sk-test
#   MATRIX_MODEL          上游真实模型名；默认 deepseek-v4.1-flash。
#                         为何不是 deepseek-v4-flash：2026-09-13 实测该模型在上游**三个协议上全部 503**
#                         （auth_unavailable: no auth available (providers=openai-compatible-hc-test, model=deepseek-v4-flash)），
#                         即上游侧该模型暂无可用渠道；deepseek-v4.1-flash 三路均 200。探活命令见 README。
#
# 用法：MATRIX_UPSTREAM_URL=http://<your-upstream>:4321 MATRIX_DSN=postgres://user:pw@host:5432/db bash setup.sh
set -euo pipefail

DSN="${MATRIX_DSN:-${CCH_TEST_DSN:-postgres://postgres:postgres@127.0.0.1:5432/cch_smoke}}"
UPSTREAM_URL="${MATRIX_UPSTREAM_URL:-}"
if [ -z "$UPSTREAM_URL" ]; then
  echo "缺少 MATRIX_UPSTREAM_URL：本夹具需要一个上游地址，且不内置默认值（避免默认跑时误连到他人的环境）。" >&2
  echo "  用法：MATRIX_UPSTREAM_URL='http://<your-upstream>:4321' MATRIX_DSN=… bash $0" >&2
  exit 2
fi
UPSTREAM_KEY="${MATRIX_UPSTREAM_KEY:-sk-test}"
MODEL="${MATRIX_MODEL:-deepseek-v4.1-flash}"
CLIENT_KEY="${MATRIX_CLIENT_KEY:-sk-matrix-client}"
PREFIX="protocol-matrix"

psql_dsn="${DSN/postgres:\/\//postgresql://}"

# -q 必不可少：本机 psql 在 -tA 下单条 INSERT ... RETURNING 仍会把
# 「INSERT 0 1」命令标签打到 stdout，捕获时会把 id 污染成两行。
q() { psql "$psql_dsn" -q -v ON_ERROR_STOP=1 -tAc "$1"; }

echo "=== [1/4] 清场（只删本脚本命名的行，可反复重跑）==="
q "DELETE FROM message_request WHERE provider_id IN (SELECT id FROM providers WHERE name LIKE '${PREFIX}%')" >/dev/null
q "DELETE FROM providers WHERE name LIKE '${PREFIX}%'" >/dev/null
q "DELETE FROM keys WHERE name LIKE '${PREFIX}%'" >/dev/null
q "DELETE FROM users WHERE name LIKE '${PREFIX}%'" >/dev/null
echo "  已清"

echo "=== [2/4] 造用户 + 客户端密钥 ==="
USER_ID=$(q "INSERT INTO users (name) VALUES ('${PREFIX}-user') RETURNING id")
q "INSERT INTO keys (user_id, key, name) VALUES (${USER_ID}, '${CLIENT_KEY}', '${PREFIX}-key')" >/dev/null
echo "  user_id=${USER_ID} key=${CLIENT_KEY}"

echo "=== [3/4] 造三个供应商（同一上游，三种协议声明）==="
# allowed_models 同时放行「自己的别名」与「上游真名」，一套夹具供两种选路模式共用：
#   · model 模式（默认）：客户端发别名 → 只有该别名所属的供应商放行 → 选路唯一；
#   · enable 模式：客户端发真名 → 三家都放行，但只启用目标那家 → 选路仍然唯一，
#     且**不经过重定向**（重定向规则只匹配别名），于是能把「转换本身」与「模型改名」分开看。
# model_redirects 把别名映到真名 ⇒ model 模式下上游始终收到 deepseek-v4-flash。
# protocol_conversion_enabled=true ⇒ 跨协议格允许转换。
add_provider() {
  local name="$1" ptype="$2" alias="$3"
  q "INSERT INTO providers (name, url, key, provider_type, is_enabled, weight, priority, cost_multiplier,
                            protocol_conversion_enabled, allowed_models, model_redirects)
     VALUES ('${name}', '${UPSTREAM_URL}', '${UPSTREAM_KEY}', '${ptype}', true, 1, 0, 1.0, true,
             '[{\"pattern\":\"${alias}\",\"matchType\":\"exact\"},{\"pattern\":\"${MODEL}\",\"matchType\":\"exact\"}]'::jsonb,
             '[{\"source\":\"${alias}\",\"target\":\"${MODEL}\",\"matchType\":\"exact\"}]'::jsonb)
     RETURNING id" >/dev/null
  echo "  ${ptype}: ${alias} | ${MODEL}  -> ${MODEL}  (provider=${name})"
}
add_provider "${PREFIX}-anthropic" "claude"            "mock-anthropic"
add_provider "${PREFIX}-chat"      "openai-compatible" "mock-chat"
add_provider "${PREFIX}-responses" "codex"             "mock-resp"

echo "=== [4/4] 校验 ==="
q "SELECT '  providers=' || count(*) FROM providers WHERE name LIKE '${PREFIX}%' AND is_enabled AND deleted_at IS NULL"
q "SELECT '  keys=' || count(*) FROM keys WHERE name LIKE '${PREFIX}%' AND is_enabled"

cat <<EOF

环境就绪。供 run.mjs 使用的变量：
  MATRIX_CLIENT_KEY=${CLIENT_KEY}
  MATRIX_ALIAS_ANTHROPIC=mock-anthropic
  MATRIX_ALIAS_CHAT=mock-chat
  MATRIX_ALIAS_RESPONSES=mock-resp
EOF
