// UI embed POC 的取证脚本：对本地 uipoc 实例发真实 HTTP 请求，打印有界摘要。
// 用法：node .sisyphus/ui-embed-poc/probe.mjs [baseURL]
const base = process.argv[2] ?? "http://127.0.0.1:18080";

const cases = [
  ["根路径（根壳）", "/"],
  ["哈希 chunk（应 immutable）", "/_next/static/chunks/43k39urre6rl6.js"],
  ["locale 前缀未知路径", "/zh-CN/dashboard/providers"],
  ["另一 locale 未知路径", "/en/dashboard/not-real"],
  ["未注册 locale 前缀", "/ja/dashboard"],
  ["无前缀未知路径", "/no-such-page"],
  ["二进制资源 ico", "/_next/static/media/favicon.2vob68tjqpejf.ico"],
  ["SVG（非哈希产物）", "/next.svg"],
  ["路径穿越", "/../../etc/passwd"],
];

const shellOf = (body) => {
  const m = body.match(/(根壳|zh-CN 壳|en shell)[^<]*/);
  return m ? m[0] : "(无壳标记)";
};

for (const [name, path] of cases) {
  const res = await fetch(base + path, { redirect: "manual" });
  const body = await res.text();
  const len = body.length;
  console.log(
    [
      name.padEnd(24),
      String(res.status).padStart(3),
      String(res.headers.get("content-type") ?? "-").split(";")[0].padEnd(24),
      String(res.headers.get("cache-control") ?? "-").padEnd(42),
      `etag=${res.headers.get("etag") ?? "-"}`,
      len > 0 && len < 4096 ? `| ${shellOf(body)}` : `| ${len} bytes`,
    ].join(" ")
  );
}

// 协商缓存：复访应得 304 且无正文。
const first = await fetch(base + "/zh-CN/dashboard");
const etag = first.headers.get("etag");
const again = await fetch(base + "/zh-CN/dashboard", { headers: { "If-None-Match": etag } });
const againLen = (await again.text()).length;
console.log(
  `协商缓存           ${again.status} body=${againLen} bytes (If-None-Match: ${etag})`
);

// 非 GET/HEAD。
const post = await fetch(base + "/zh-CN", { method: "POST" });
console.log(`POST 壳路径        ${post.status} allow=${post.headers.get("allow")}`);

// 路径穿越：fetch/curl 默认会在客户端归一化 `..`，必须用原始 socket 才能把字面 `..` 送到服务端。
import net from "node:net";
const rawProbe = (rawPath) =>
  new Promise((resolve) => {
    const url = new URL(base);
    const sock = net.connect(Number(url.port), url.hostname, () => {
      sock.write(
        `GET ${rawPath} HTTP/1.1\r\nHost: ${url.host}\r\nConnection: close\r\n\r\n`
      );
    });
    let buf = "";
    sock.on("data", (d) => (buf += d.toString("utf8")));
    sock.on("end", () => resolve(buf.split("\r\n")[0]));
    sock.on("error", (e) => resolve(`socket error: ${e.message}`));
  });
console.log(`原始 socket 穿越    ${await rawProbe("/../../etc/passwd")}`);
console.log(`原始 socket 正常    ${await rawProbe("/zh-CN/dashboard")}`);
