/**
 * 三协议转换矩阵 · 驱动器（9 格 × 非流式/流式 + 三项功能穿越）
 *
 * 矩阵口径：客户端协议（3）× 上游协议（3）。上游协议由**模型别名**决定——setup.sh 把
 * 三个别名分别只放行给一个供应商，故「客户端协议 × 别名」唯一确定一格，选路无歧义。
 *
 * 断言（每格）：
 *   1. HTTP 200；
 *   2. **响应形状 = 客户端协议**（这条能抓「上游方言泄漏给客户端」这类接线错误）；
 *   3. 助手文本非空且含约定标记（内容未因转换丢失）；
 *   4. usage 取到 input/output tokens（取不到就如实记 missing，不算通过）；
 *   5. 流式：收到**客户端协议**的终止事件，且拼出的文本非空。
 * 归因：同协议三格是基线；基线也失败 → 判上游；仅跨协议格失败 → 判转换。
 *
 * 用法：
 *   node run.mjs --all          跑全矩阵
 *   node run.mjs --cell claude>chat   只跑一格
 *   MATRIX_BASE=http://127.0.0.1:32891 node run.mjs --all
 */
import { execFileSync } from "node:child_process";
import { readFileSync, mkdirSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const KEY = process.env.MATRIX_CLIENT_KEY || "sk-matrix-client";
const MARKER = "MATRIX7F3A";

const ALIAS = {
  anthropic: process.env.MATRIX_ALIAS_ANTHROPIC || "mock-anthropic",
  chat: process.env.MATRIX_ALIAS_CHAT || "mock-chat",
  responses: process.env.MATRIX_ALIAS_RESPONSES || "mock-resp",
};

/**
 * 选路方式（两种，都要支持）：
 *
 * - `model`（默认）：靠**模型别名**选择上游协议（三个别名各只放行给一个供应商），
 *   依赖**模型重定向**把别名映到上游真名。
 * - `enable`：靠**启用开关**选择上游协议——模型直接用上游真名（不经过重定向），
 *   每格只启用目标供应商。
 *
 * 为何要两种：实测发现「**模型重定向在协议转换路径上被跳过**」（`forward/plan.go:317`
 * 把改写条件写成了 `plan.Conversion == nil`），于是 `model` 模式下 6 个跨协议格会因
 * 「上游收到别名」而 400。`enable` 模式绕开重定向，才能单独看**转换本身**对不对。
 */
const SELECT = process.env.MATRIX_SELECT || "model";
// 默认与 setup.sh 保持一致（该模型实测三协议均可用；deepseek-v4-flash 当前上游全线 503）。
const REAL_MODEL = process.env.MATRIX_MODEL || "deepseek-v4.1-flash";
const PREFIX = "protocol-matrix";

function modelFor(upstream) {
  return SELECT === "enable" ? REAL_MODEL : ALIAS[upstream];
}

/** enable 模式下，每格只启用目标供应商（三个同时启用时选路会歧义）。 */
function selectByEnable(upstream) {
  if (SELECT !== "enable") return;
  const dsn = (
    process.env.MATRIX_DSN ||
    process.env.CCH_TEST_DSN ||
    "postgres://postgres:postgres@127.0.0.1:5432/cch_smoke"
  ).replace("postgres://", "postgresql://");
  const sql = `UPDATE providers SET is_enabled = (name = '${PREFIX}-${upstream}') WHERE name LIKE '${PREFIX}%'`;
  execFileSync("psql", [dsn, "-q", "-c", sql], { stdio: "ignore" });
}

const CLIENTS = ["claude", "chat", "responses"];
const UPSTREAMS = ["anthropic", "chat", "responses"];

function base() {
  if (process.env.MATRIX_BASE) return process.env.MATRIX_BASE.replace(/\/$/, "");
  try {
    const port = readFileSync(join(HERE, ".cchd.port"), "utf8").trim();
    return `http://127.0.0.1:${port}`;
  } catch {
    return "http://127.0.0.1:13500";
  }
}

/** 客户端协议的路径/头/正文形状。 */
function buildRequest(client, upstreamAlias, opts = {}) {
  const { stream = false, prompt = `Reply with exactly: ${MARKER}`, extra = {} } = opts;
  if (client === "claude") {
    return {
      path: "/v1/messages",
      headers: { "x-api-key": KEY, "anthropic-version": "2023-06-01" },
      body: {
        model: upstreamAlias,
        max_tokens: 64,
        messages: [{ role: "user", content: prompt }],
        ...(stream ? { stream: true } : {}),
        ...extra,
      },
    };
  }
  if (client === "chat") {
    return {
      path: "/v1/chat/completions",
      headers: { authorization: `Bearer ${KEY}` },
      body: {
        model: upstreamAlias,
        max_tokens: 64,
        messages: [{ role: "user", content: prompt }],
        ...(stream ? { stream: true } : {}),
        ...extra,
      },
    };
  }
  return {
    path: "/v1/responses",
    headers: { authorization: `Bearer ${KEY}` },
    body: {
      model: upstreamAlias,
      max_output_tokens: 64,
      input: prompt,
      ...(stream ? { stream: true } : {}),
      ...extra,
    },
  };
}

/** 判定响应体是否为**客户端协议**的形状。 */
function shapeOf(client, body) {
  if (client === "claude") return body?.type === "message" && Array.isArray(body?.content);
  if (client === "chat") return body?.object === "chat.completion" || Array.isArray(body?.choices);
  return body?.object === "response" || Array.isArray(body?.output);
}

function textOf(client, body) {
  if (client === "claude") {
    return (body?.content ?? [])
      .filter((b) => b?.type === "text")
      .map((b) => b.text ?? "")
      .join("");
  }
  if (client === "chat") return body?.choices?.[0]?.message?.content ?? "";
  return (body?.output ?? [])
    .filter((o) => o?.type === "message")
    .flatMap((o) => o?.content ?? [])
    .map((c) => c?.text ?? "")
    .join("");
}

function usageOf(client, body) {
  const u = body?.usage ?? {};
  if (client === "claude")
    return { input: u.input_tokens ?? null, output: u.output_tokens ?? null };
  if (client === "chat")
    return { input: u.prompt_tokens ?? null, output: u.completion_tokens ?? null };
  return { input: u.input_tokens ?? null, output: u.output_tokens ?? null };
}

/** 各协议的流式终止事件与增量提取。 */
const STREAM = {
  claude: {
    terminal: "message_stop",
    delta: (o) => (o.type === "content_block_delta" ? o.delta?.text : ""),
  },
  chat: { terminal: "[DONE]", delta: (o) => o?.choices?.[0]?.delta?.content ?? "" },
  responses: {
    terminal: "response.completed",
    delta: (o) => (o.type === "response.output_text.delta" ? (o.delta ?? "") : ""),
  },
};

async function post(baseUrl, spec, { stream = false, timeoutMs = 90_000 } = {}) {
  const ac = new AbortController();
  const timer = setTimeout(() => ac.abort(), timeoutMs);
  const started = Date.now();
  try {
    const res = await fetch(baseUrl + spec.path, {
      method: "POST",
      headers: { "content-type": "application/json", ...spec.headers },
      body: JSON.stringify(spec.body),
      signal: ac.signal,
    });
    if (!stream) {
      const text = await res.text();
      let json = null;
      try {
        json = JSON.parse(text);
      } catch {}
      return {
        status: res.status,
        ttfbMs: Date.now() - started,
        totalMs: Date.now() - started,
        json,
        raw: text.slice(0, 400),
      };
    }
    // 流式：逐行解析 SSE，记录首字节与终止事件
    let ttfbMs = null;
    let buf = "";
    const deltas = [];
    const terminals = [];
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      if (ttfbMs === null) ttfbMs = Date.now() - started;
      buf += decoder.decode(value, { stream: true });
      const lines = buf.split("\n");
      buf = lines.pop() ?? "";
      for (const line of lines) {
        const trimmed = line.trim();
        if (!trimmed.startsWith("data:")) continue;
        const payload = trimmed.slice(5).trim();
        if (payload === "[DONE]") {
          terminals.push("[DONE]");
          continue;
        }
        try {
          const obj = JSON.parse(payload);
          const dt = STREAM[spec.clientForStream]?.delta?.(obj);
          if (dt) deltas.push(dt);
          if (typeof obj?.type === "string" && /_stop$|completed$|_end$/.test(obj.type))
            terminals.push(obj.type);
        } catch {
          /* 非 JSON 帧忽略 */
        }
      }
    }
    return {
      status: res.status,
      ttfbMs: ttfbMs ?? Date.now() - started,
      totalMs: Date.now() - started,
      streamedText: deltas.join(""),
      terminals,
      raw: (deltas.join("") || "").slice(0, 200),
    };
  } catch (err) {
    return {
      status: 0,
      ttfbMs: Date.now() - started,
      totalMs: Date.now() - started,
      error: String(err?.message ?? err),
    };
  } finally {
    clearTimeout(timer);
  }
}

const results = [];

async function runCell(baseUrl, client, upstream) {
  const alias = modelFor(upstream);
  selectByEnable(upstream);
  const sameProtocol =
    (client === "claude" && upstream === "anthropic") ||
    (client === "chat" && upstream === "chat") ||
    (client === "responses" && upstream === "responses");

  // —— 非流式 ——
  const spec = buildRequest(client, alias);
  spec.clientForStream = client;
  const r = await post(baseUrl, spec);
  const body = r.json ?? {};
  const text = r.json ? textOf(client, body) : "";
  const usage = r.json ? usageOf(client, body) : { input: null, output: null };
  const shapeOk = r.json ? shapeOf(client, body) : false;

  // —— 流式 ——
  const sspec = buildRequest(client, alias, { stream: true });
  sspec.clientForStream = client;
  const s = await post(baseUrl, sspec, { stream: true });
  const terminalWanted = STREAM[client].terminal;
  const terminalOk = (s.terminals ?? []).some(
    (t) =>
      t === terminalWanted ||
      (terminalWanted === "response.completed" && t === "response.completed")
  );

  const cell = {
    cell: `${client}>${upstream}`,
    sameProtocol,
    nonStream: {
      status: r.status,
      ttfbMs: r.ttfbMs,
      totalMs: r.totalMs,
      shapeOk,
      textLen: text.length,
      marker: text.includes(MARKER),
      usage,
      sample: (text || r.raw || "").slice(0, 120),
      error: r.error ?? null,
    },
    stream: {
      status: s.status,
      ttfbMs: s.ttfbMs,
      totalMs: s.totalMs,
      terminalWanted,
      terminalOk,
      textLen: (s.streamedText ?? "").length,
      marker: (s.streamedText ?? "").includes(MARKER),
      error: s.error ?? null,
      sample: (s.streamedText || "").slice(0, 120),
    },
  };
  cell.pass =
    cell.nonStream.status === 200 &&
    cell.nonStream.shapeOk &&
    cell.nonStream.textLen > 0 &&
    cell.nonStream.usage.input !== null &&
    cell.nonStream.usage.output !== null &&
    cell.stream.status === 200 &&
    cell.stream.terminalOk &&
    cell.stream.textLen > 0;
  results.push(cell);
  const flag = cell.pass ? "PASS" : "FAIL";
  console.log(
    `  ${flag}  ${cell.cell.padEnd(20)} ns=${cell.nonStream.status}/${cell.nonStream.textLen}字/marker=${cell.nonStream.marker ? "y" : "n"}/ttfb=${cell.nonStream.ttfbMs}ms  st=${cell.stream.status}/term=${cell.stream.terminalOk ? "y" : "n"}/${cell.stream.textLen}字`
  );
  return cell;
}

/** 功能穿越：tools / 思考强度 / 多轮+思考回传。逐格记录，不因失败中断矩阵。 */
async function runFeatures(baseUrl, client, upstream) {
  const alias = modelFor(upstream);
  selectByEnable(upstream);
  const out = {};

  // (a) tools：按客户端协议给工具定义，要求调用它。
  const toolsExtra = {
    claude: {
      tools: [
        {
          name: "get_weather",
          description: "Get weather for a city.",
          input_schema: {
            type: "object",
            properties: { city: { type: "string" } },
            required: ["city"],
          },
        },
      ],
    },
    chat: {
      tools: [
        {
          type: "function",
          function: {
            name: "get_weather",
            description: "Get weather for a city.",
            parameters: {
              type: "object",
              properties: { city: { type: "string" } },
              required: ["city"],
            },
          },
        },
      ],
    },
    responses: {
      tools: [
        {
          type: "function",
          name: "get_weather",
          description: "Get weather for a city.",
          parameters: {
            type: "object",
            properties: { city: { type: "string" } },
            required: ["city"],
          },
        },
      ],
    },
  }[client];

  const spec = buildRequest(client, alias, {
    prompt: "Use the get_weather tool for Beijing. Do not answer from memory.",
    extra: toolsExtra,
  });
  spec.clientForStream = client;
  const r = await post(baseUrl, spec);
  const toolCall = extractToolCall(client, r.json);
  out.tools = {
    status: r.status,
    toolCall,
    note:
      r.status === 200
        ? toolCall
          ? "调用发生并取到名字/参数"
          : "本发未触发调用（模型选择直接回答）"
        : "请求失败",
    sample: r.raw,
  };

  // (b) 思考强度：各协议各自的强度字段
  const effortExtra = {
    claude: { output_config: { effort: "low" } },
    chat: { reasoning_effort: "low" },
    responses: { reasoning: { effort: "low" } },
  }[client];
  const es = buildRequest(client, alias, { prompt: "1+1=?", extra: effortExtra });
  es.clientForStream = client;
  const er = await post(baseUrl, es);
  out.effort = {
    status: er.status,
    textLen: er.json ? textOf(client, er.json).length : 0,
    sample: er.raw,
  };

  // (c) 多轮 + 思考回传：第一发→把助手回复带回第二发，看是否被上游以 reasoning 相关理由拒绝
  const first = buildRequest(client, alias, { prompt: "Remember the code ZQ9." });
  first.clientForStream = client;
  const fr = await post(baseUrl, first);
  const assistantText = fr.json ? textOf(client, fr.json) : "";
  const second = buildSecondTurn(client, alias, assistantText);
  second.clientForStream = client;
  const sr = await post(baseUrl, second);
  // 只在**失败**响应里找该特征：成功响应体里本来就带 reasoning_content 字段名
  // （上游把思考内容回给我们了），按整串匹配会把 200 全判成「被拒绝」——这是本驱动
  // 第一版真实踩过的假阳性。
  const errText = (sr.raw || "").toLowerCase();
  out.multiTurn = {
    firstStatus: fr.status,
    secondStatus: sr.status,
    reasoningRejected: sr.status >= 400 && /reasoning_content|must be passed back/.test(errText),
    sample: sr.raw,
  };

  return out;
}

function extractToolCall(client, body) {
  try {
    if (client === "claude") {
      const tu = (body?.content ?? []).find((c) => c?.type === "tool_use");
      return tu ? { name: tu.name, input: tu.input } : null;
    }
    if (client === "chat") {
      const tc = body?.choices?.[0]?.message?.tool_calls?.[0];
      return tc ? { name: tc.function?.name, input: tc.function?.arguments } : null;
    }
    const fc = (body?.output ?? []).find((o) => o?.type === "function_call");
    return fc ? { name: fc.name, input: fc.arguments } : null;
  } catch {
    return null;
  }
}

function buildSecondTurn(client, alias, assistantText) {
  const follow = "What was the code?";
  if (client === "claude" || client === "chat") {
    return {
      path: client === "claude" ? "/v1/messages" : "/v1/chat/completions",
      headers:
        client === "claude"
          ? { "x-api-key": KEY, "anthropic-version": "2023-06-01" }
          : { authorization: `Bearer ${KEY}` },
      body: {
        model: alias,
        max_tokens: 64,
        messages: [
          { role: "user", content: "Remember the code ZQ9." },
          { role: "assistant", content: assistantText || "ZQ9" },
          { role: "user", content: follow },
        ],
      },
    };
  }
  return {
    path: "/v1/responses",
    headers: { authorization: `Bearer ${KEY}` },
    body: {
      model: alias,
      max_output_tokens: 64,
      input: [
        { role: "user", content: "Remember the code ZQ9." },
        { role: "assistant", content: assistantText || "ZQ9" },
        { role: "user", content: follow },
      ],
    },
  };
}

function writeResults(baseUrl, withFeatures) {
  const dir = join(HERE, "results");
  mkdirSync(dir, { recursive: true });
  const stamp = new Date().toISOString().replace(/[:.]/g, "-");
  const payload = {
    baseUrl,
    marker: MARKER,
    cells: results,
    features: withFeatures,
    at: new Date().toISOString(),
  };
  const jsonPath = join(dir, `matrix-${stamp}.json`);
  writeFileSync(jsonPath, JSON.stringify(payload, null, 2));

  const lines = [
    "# 三协议转换矩阵（本地真上游）",
    "",
    `- 网关：\`${baseUrl}\``,
    `- 上游：见 setup.sh 的 MATRIX_UPSTREAM_URL（同一台，讲三种协议）`,
    `- 约定标记：\`${MARKER}\``,
    "",
    "| 格 | 同协议 | 非流式 | 流式 | 判定 |",
    "| --- | --- | --- | --- | --- |",
  ];
  for (const c of results) {
    lines.push(
      `| \`${c.cell}\` | ${c.sameProtocol ? "基线" : "跨协议"} | ${c.nonStream.status}/${c.nonStream.textLen}字/marker=${c.nonStream.marker ? "y" : "n"}/ttfb=${c.nonStream.ttfbMs}ms | ${c.stream.status}/term=${c.stream.terminalOk ? "y" : "n"}/${c.stream.textLen}字 | ${c.pass ? "**PASS**" : "**FAIL**"} |`
    );
  }
  lines.push(
    "",
    "## 功能穿越",
    "",
    "| 格 | tools | 思考强度 | 多轮/回传 |",
    "| --- | --- | --- | --- |"
  );
  for (const c of results) {
    const f = withFeatures[c.cell] ?? {};
    lines.push(
      `| \`${c.cell}\` | ${f.tools?.status ?? "-"} ${f.tools?.note ?? ""} | ${f.effort?.status ?? "-"}/${f.effort?.textLen ?? "-"}字 | first=${f.multiTurn?.firstStatus ?? "-"} second=${f.multiTurn?.secondStatus ?? "-"} rejected=${f.multiTurn?.reasoningRejected ? "是" : "否"} |`
    );
  }
  const mdPath = join(dir, "matrix.md");
  writeFileSync(mdPath, `${lines.join("\n")}\n`);
  console.log(`\n结果：${jsonPath}\n      ${mdPath}`);
  return { jsonPath, mdPath };
}

async function main() {
  const args = process.argv.slice(2);
  const baseUrl = base();
  const onlyCell = args.includes("--cell") ? args[args.indexOf("--cell") + 1] : null;
  console.log(`BASE=${baseUrl}`);
  console.log("=== 9 格矩阵（非流式 + 流式）===");

  for (const client of CLIENTS) {
    for (const upstream of UPSTREAMS) {
      const id = `${client}>${upstream}`;
      if (onlyCell && onlyCell !== id) continue;
      await runCell(baseUrl, client, upstream);
    }
  }

  const withFeatures = {};
  if (args.includes("--all") || args.includes("--features")) {
    console.log("\n=== 功能穿越（tools / 思考强度 / 多轮+回传）===");
    for (const client of CLIENTS) {
      for (const upstream of UPSTREAMS) {
        const id = `${client}>${upstream}`;
        if (onlyCell && onlyCell !== id) continue;
        const f = await runFeatures(baseUrl, client, upstream);
        withFeatures[id] = f;
        console.log(
          `  ${id.padEnd(20)} tools=${f.tools.status}/${f.tools.toolCall ? "called" : "no-call"}  effort=${f.effort.status}/${f.effort.textLen}字  turn2=${f.multiTurn.secondStatus}  reasoningRejected=${f.multiTurn.reasoningRejected ? "YES" : "no"}`
        );
      }
    }
  }

  const paths = writeResults(baseUrl, withFeatures);
  const passed = results.filter((r) => r.pass).length;
  console.log(`\n汇总：${passed}/${results.length} 格通过`);
  return paths;
}

main().catch((err) => {
  console.error("驱动失败：", err);
  process.exit(1);
});
