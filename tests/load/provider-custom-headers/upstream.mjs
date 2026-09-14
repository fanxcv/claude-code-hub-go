/**
 * 验收用上游：**要求** `x-opencode-session`，否则 400。
 *
 * 为什么自建而不是只用真上游：真实 OpenCode 渠道对该头的要求是「缺即 400」，
 * 但真实渠道（地址由 `CCH_REAL_UPSTREAM` 给出）并不强制，无法反过来证明「我们发出的头**确实到达了上游**」。
 * 这台上游同时把人收到的头回显到 `x-echo-headers`，于是断言既覆盖状态码也覆盖头部本身。
 *
 * 用法：node upstream.mjs <port>
 */
import { createServer } from "node:http";

const port = Number(process.argv[2] ?? 0);
const REQUIRED = "x-opencode-session";

const server = createServer((req, res) => {
  let body = "";
  req.on("data", (chunk) => {
    body += chunk;
  });
  req.on("end", () => {
    const session = req.headers[REQUIRED];
    if (!session) {
      res.writeHead(400, { "content-type": "application/json" });
      res.end(
        JSON.stringify({
          error: {
            message:
              "Request is missing x-opencode-session and cannot be routed efficiently. Please see https://opencode.ai/docs/go/#where-can-i-use-it",
            type: "invalid_request_error",
          },
        })
      );
      return;
    }

    // 回显收到的头，供驱动脚本断言「头真的到达上游」与「未配置的供应商不带该头」。
    const echo = Object.fromEntries(
      Object.entries(req.headers).map(([name, value]) => [
        name,
        Array.isArray(value) ? value.join(",") : value,
      ])
    );
    const headers = {
      "content-type": "application/json",
      "x-echo-headers": Buffer.from(JSON.stringify(echo)).toString("base64"),
    };

    if (req.url?.startsWith("/v1/messages")) {
      res.writeHead(200, headers);
      res.end(
        JSON.stringify({
          id: "msg_echo",
          type: "message",
          role: "assistant",
          model: "echo-model",
          content: [{ type: "text", text: `session=${session}` }],
          stop_reason: "end_turn",
          usage: { input_tokens: 1, output_tokens: 1 },
        })
      );
      return;
    }

    res.writeHead(200, headers);
    res.end(
      JSON.stringify({
        id: "chatcmpl-echo",
        object: "chat.completion",
        model: "echo-model",
        choices: [
          {
            index: 0,
            message: { role: "assistant", content: `session=${session}` },
            finish_reason: "stop",
          },
        ],
        usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 },
      })
    );
  });
});

server.listen(port, "127.0.0.1", () => {
  const address = server.address();
  // 驱动脚本据此拿到实际端口（port=0 表示随机）。
  process.stdout.write(`ECHO_UPSTREAM=http://127.0.0.1:${address.port}\n`);
});
