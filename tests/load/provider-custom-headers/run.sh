#!/usr/bin/env bash
# 供应商自定义请求头 · 端到端验收（本地网关 + 自建「要求该头」的上游 + 真多协议上游）
#
# 三条断言，各自都有诊断价值：
#   ① 配了 custom_headers 的供应商 → 200（改前必 400：头根本没发出去）；
#   ② 未配的供应商 → 仍 400，且**不带**该头（证明「不污染其它供应商」）；
#   ③ 真上游（多协议）配了自定义头 → 200（证明配置在真实传输路径上不破坏请求）。
#
# 用法：
#   bash run.sh            # 跑全部；需要 /tmp/cchd-matrix（见 README）
#   KEEP=1 bash run.sh     # 跑完不停网关，便于手工复看
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
BIN="${MATRIX_CCHD_BIN:-/tmp/cchd-matrix}"

DSN="${CCH_TEST_DSN:-postgres://postgres:postgres@127.0.0.1:5432/cch_smoke}"
REDIS_URL="${CCH_TEST_REDIS_URL:-redis://127.0.0.1:6379/15}"
REAL_UPSTREAM="${CCH_REAL_UPSTREAM:-}"
MODEL="${CCH_CH_MODEL:-deepseek-v4.1-flash}"
CLIENT_KEY="sk-cch-custom-headers"
SESSION_VALUE="cch-verify-0001"
PREFIX="custom-headers"

if [ -z "$REAL_UPSTREAM" ]; then
  echo "缺少 CCH_REAL_UPSTREAM：本夹具需要一个真实上游地址，且不内置默认值（避免默认跑时误连到他人的环境）。" >&2
  echo "  用法：CCH_REAL_UPSTREAM='http://<your-upstream>:4321' bash $0" >&2
  exit 2
fi

if [ ! -x "$BIN" ]; then
  echo "缺少 cchd 二进制：cd go && CGO_ENABLED=0 go build -o $BIN ./cmd/cchd" >&2
  exit 1
fi

q() {
  # -q 不可少：本机 psql 在 -tA 下仍会把命令标签打到 stdout，捕获 id 时会被污染。
  psql "${DSN/postgres:\/\//postgresql://}" -tA -q -c "$1"
}

cleanup() {
  if [ -n "${ECHO_PID:-}" ]; then kill "$ECHO_PID" 2>/dev/null || true; fi
  if [ "${KEEP:-}" != "1" ]; then
    if [ -f "$HERE/.gateway.pid" ]; then
      pid=$(cat "$HERE/.gateway.pid")
      kill "$pid" 2>/dev/null || true; sleep 1; kill -9 "$pid" 2>/dev/null || true
      rm -f "$HERE/.gateway.pid" "$HERE/.gateway.port"
    fi
  fi
}
trap cleanup EXIT

echo "=== 0) 起回显上游（要求 x-opencode-session）==="
node "$HERE/upstream.mjs" 0 >"$HERE/.upstream.out" 2>&1 &
ECHO_PID=$!
for _ in $(seq 1 40); do
  ECHO_URL=$(sed -n 's/^ECHO_UPSTREAM=//p' "$HERE/.upstream.out" 2>/dev/null | head -1)
  [ -n "$ECHO_URL" ] && break
  sleep 0.25
done
if [ -z "${ECHO_URL:-}" ]; then echo "回显上游未就绪" >&2; cat "$HERE/.upstream.out" >&2; exit 1; fi
echo "  $ECHO_URL"

echo "=== 1) 造环境（可弃库 cch_smoke；幂等）==="
q "DELETE FROM providers WHERE name LIKE '${PREFIX}%'" >/dev/null
q "DELETE FROM keys WHERE name LIKE '${PREFIX}%'" >/dev/null
q "DELETE FROM users WHERE name LIKE '${PREFIX}%'" >/dev/null
USER_ID=$(q "INSERT INTO users (name) VALUES ('${PREFIX}-user') RETURNING id")
q "INSERT INTO keys (user_id, key, name) VALUES (${USER_ID}, '${CLIENT_KEY}', '${PREFIX}-key')" >/dev/null

# 三个供应商，靠模型别名把选路钉死（每个只放行自己的别名）。
add_provider() {
  local name="$1" url="$2" key="$3" alias="$4" headers="$5"
  q "INSERT INTO providers (name, url, key, provider_type, is_enabled, weight, priority, cost_multiplier,
       protocol_conversion_enabled, model_redirects, allowed_models, custom_headers)
     VALUES ('${name}', '${url}', '${key}', 'claude', true, 1, 0, 1.0, true,
             '[{\"source\":\"${alias}\",\"target\":\"${MODEL}\",\"matchType\":\"exact\"}]'::jsonb,
             '[{\"pattern\":\"${alias}\",\"matchType\":\"exact\"}]'::jsonb,
             ${headers})" >/dev/null
  echo "  ${name} | alias=${alias} | custom_headers=${headers}"
}
add_provider "${PREFIX}-with"    "$ECHO_URL"      "sk-echo" "alias-with"    "'{\"x-opencode-session\":\"${SESSION_VALUE}\"}'::jsonb"
add_provider "${PREFIX}-without" "$ECHO_URL"      "sk-echo" "alias-without" "NULL"
add_provider "${PREFIX}-real"    "$REAL_UPSTREAM" "sk-test" "alias-real"    "'{\"x-opencode-session\":\"${SESSION_VALUE}\"}'::jsonb"
# ④ 安全边界：故意把保留名与凭据头写进自定义头（模拟历史脏数据 / 恶意配置）。
# 同时带上上游必需的那个头（否则会因缺头 400，测不到剥离行为）。
add_provider "${PREFIX}-reserved" "$ECHO_URL" "sk-echo" "alias-reserved" \
  "'{\"x-opencode-session\":\"${SESSION_VALUE}\",\"host\":\"evil.example\",\"content-length\":\"999\",\"authorization\":\"Bearer attacker\",\"x-api-key\":\"sk-attacker\",\"x-ok\":\"1\"}'::jsonb"

echo "=== 2) 起本地网关（同时接管 /v1/* 与 /api/*）==="
# 为什么自起而不复用矩阵台的 start.sh：那边只把 `/v1/*` 交给 Go，`/api/*` 仍回退 Node，
# 本机没有 Node 可回退 ⇒ 管理面调用会得到 502，测不了写入侧校验。
PORT=$(python3 - <<'PY'
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()
PY
)
env -i PATH="$PATH" HOME="$HOME" \
  PORT="$PORT" CCH_INTERNAL_PORT="$((PORT + 1))" \
  DSN="$DSN" REDIS_URL="$REDIS_URL" \
  CCH_EGRESS_MODE=go CCH_EGRESS_ROUTES='* /v1/*,* /api/*' CCH_EGRESS_PAGES=off \
  AUTO_MIGRATE=false ADMIN_TOKEN=matrix-admin-token \
  "$BIN" >"$HERE/.gateway.log" 2>&1 &
echo $! >"$HERE/.gateway.pid"
echo "$PORT" >"$HERE/.gateway.port"
for _ in $(seq 1 60); do
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:${PORT}/v1/_ping" || true)
  [ "$code" = "200" ] && break
  sleep 0.25
done
echo "  /v1/_ping=${code}（日志 $HERE/.gateway.log）"
BASE="http://127.0.0.1:${PORT}"

post() {
  local alias="$1" out="$2"
  curl -s -o "$out" -D "$out.h" -w '%{http_code}' --max-time 60 \
    -X POST "${BASE}/v1/messages" \
    -H "x-api-key: ${CLIENT_KEY}" -H "anthropic-version: 2023-06-01" -H "content-type: application/json" \
    --data "{\"model\":\"${alias}\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"
}

echo "=== 3) 断言 ==="
FAILED=0
check() {
  local label="$1" want="$2" got="$3"
  if [ "$want" = "$got" ]; then echo "  PASS  ${label}（HTTP ${got}）"; else echo "  FAIL  ${label}：期望 ${want}，实际 ${got}"; FAILED=1; fi
}

# ① 配了自定义头 → 200
code=$(post "alias-with" /tmp/ch-with.json)
check "① 配了 custom_headers 的供应商" 200 "$code"
if [ "$code" = "200" ]; then
  echo "        上游收到的 x-opencode-session：$(python3 - "$SESSION_VALUE" <<'PY'
import base64, json, sys
raw = None
for line in open("/tmp/ch-with.json.h", encoding="utf-8", errors="replace"):
    if line.lower().startswith("x-echo-headers:"):
        raw = line.split(":", 1)[1].strip()
print((json.loads(base64.b64decode(raw)).get("x-opencode-session") if raw else "<无回显>") + f"（期望 {sys.argv[1]}）")
PY
)"
fi

# ② 未配 → 仍 400（未污染）
code=$(post "alias-without" /tmp/ch-without.json)
check "② 未配的供应商（不得被注入该头）" 400 "$code"
echo "        上游回：$(head -c 150 /tmp/ch-without.json)"

# ③ 真多协议上游 + 自定义头 → 200
code=$(post "alias-real" /tmp/ch-real.json)
check "③ 真上游（多协议）配了自定义头" 200 "$code"
echo "        真上游回：$(head -c 120 /tmp/ch-real.json)"

# ④ 安全边界：保留名与凭据头必须被剥离，且合法自定义头照常生效。
code=$(post "alias-reserved" /tmp/ch-reserved.json)
check "④ 含保留名/凭据头的配置仍能正常请求" 200 "$code"
if [ "$code" = "200" ]; then
  python3 - <<'PY' || FAILED=1
import base64, json, sys

def echoed():
    for line in open("/tmp/ch-reserved.json.h", encoding="utf-8", errors="replace"):
        if line.lower().startswith("x-echo-headers:"):
            return json.loads(base64.b64decode(line.split(":", 1)[1].strip()))
    return {}

got = echoed()
bad = []
if got.get("host", "").startswith("evil.example"):
    bad.append("host 被自定义头改写（凭据会发往攻击者指定的目标）")
if got.get("authorization") == "Bearer attacker":
    bad.append("authorization 被自定义头覆盖")
if got.get("x-api-key") == "sk-attacker":
    bad.append("x-api-key 被自定义头覆盖")
if got.get("content-length") == "999":
    bad.append("content-length 被自定义头覆盖（会破坏请求分帧）")
if got.get("x-ok") != "1":
    bad.append(f"合法自定义头 x-ok 未生效（实际 {got.get('x-ok')!r}）")
if bad:
    print("  FAIL  ④ 安全边界：" + "；".join(bad))
    sys.exit(1)
print("  PASS  ④ 保留名与凭据头均被剥离（host=%s，authorization=%s，x-ok=%s）"
      % (got.get("host"), got.get("authorization"), got.get("x-ok")))
PY
fi

echo "=== 4) 管理面：能写、且非法形态被拒 ==="
admin_post() {
  local body="$1" out="$2"
  curl -s -o "$out" -w '%{http_code}' --max-time 30 \
    -X POST "${BASE}/api/v1/providers" \
    -H "Authorization: Bearer matrix-admin-token" -H "content-type: application/json" \
    --data "$body"
}

# ⑥ 合法：写入后应能在库里读到归一化结果。
code=$(admin_post "{\"name\":\"${PREFIX}-written\",\"url\":\"http://127.0.0.1:1\",\"key\":\"sk-written\",\"provider_type\":\"claude\",\"custom_headers\":{\"x-a\":\"1\",\"X-B\":\"2\"}}" /tmp/ch-write.json)
check "⑥ 管理面接受合法 custom_headers" 201 "$code"
echo "        库里存的是：$(q "SELECT custom_headers FROM providers WHERE name = '${PREFIX}-written'")"

# ⑤ 非法形态：逐条断言错误码（Node 的自定义码原文）。
assert_admin_rejects() {
  local label="$1" json="$2" want="$3"
  code=$(admin_post "{\"name\":\"${PREFIX}-reject\",\"url\":\"http://127.0.0.1:1\",\"key\":\"sk-x\",\"provider_type\":\"claude\",\"custom_headers\":${json}}" /tmp/ch-reject.json)
  got=$(python3 -c "import json;d=json.load(open('/tmp/ch-reject.json'));p=(d.get('invalidParams') or [{}])[0];print(p.get('code',''))" 2>/dev/null)
  if [ "$code" = "400" ] && [ "$got" = "$want" ]; then
    echo "  PASS  ⑤ ${label}（HTTP 400，code=${got}）"
  else
    echo "  FAIL  ⑤ ${label}：期望 400/${want}，实际 ${code}/${got}"; FAILED=1
  fi
}
assert_admin_rejects "鉴权头被拒"      '{"authorization":"Bearer evil"}'       "protected_name"
assert_admin_rejects "非对象被拒"      '["x"]'                              "not_object"
assert_admin_rejects "非字符串值被拒"  '{"x-n":7}'                          "invalid_value"
assert_admin_rejects "重复键被拒"      '{"x-a":"1","X-A":"2"}'            "duplicate_name"
assert_admin_rejects "非法头名被拒"    '{"x a":"1"}'                        "invalid_name"
assert_admin_rejects "值含 CRLF 被拒"  '{"x-a":"1\r\n2"}'                   "crlf"

q "DELETE FROM providers WHERE name LIKE '${PREFIX}%'" >/dev/null

echo
if [ "$FAILED" = "0" ]; then echo "全部通过"; else echo "存在失败"; fi
exit "$FAILED"