import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import path from "node:path";
import { describe, expect, test } from "vitest";

/**
 * 敏感信息钉子：受跟踪文件与最近一次提交的元数据里**不得**出现内部标识、凭据形态、
 * 个人路径与真实生产数据。
 *
 * 为什么需要它：本仓在发布前做过一次「隐私清理 + 历史折叠」——删掉了自建域名、内网地址、
 * 部署主机名与一批被跟踪的运行态产物。那次清理是**一次性**的：只要没有机器检查，下一个人
 * （或下一个代理）把内部域名写进文档、把真实 DSN 贴进夹具、把带私有域名的身份提交进历史，
 * 就会随下一次 `v*` 发布一起泄漏出去。所以这里把纪律钉死。
 *
 * 判据两条，缺一不可：
 * 1. **内容**：`git ls-files` 里的每个文本文件都要过下面六组规则（见 RULES 注释）；
 * 2. **元数据**：最近一次提交的作者/提交者身份与提交信息也要过同一组规则（身份另有白名单）。
 *
 * 口径与取舍（改动本文件前先读这段）：
 * - 规则按**语境**定义，不按目录豁免：URL 主机、镜像引用、`host:port`、私网字面量、本机绝对
 *   路径、凭据形态各扫各的；测试夹具惯例使用 RFC1918 字面量（`10.0.0.1` 之类）故单独放行，
 *   其余每一处放行都要在 ALLOWLIST 里写明「文件 + 值 + 理由」。
 * - 允许清单只放宽**被点名的那个值**，不是整份文件：同一个文件里出现新的地址仍会红。
 * - 失败信息里的凭据片段会被遮蔽（只留首尾），避免「为了报告泄漏而在 CI 日志里再泄漏一次」。
 */

const ROOT = process.cwd();

/** 扫描上限：超过 2 MiB 或前 8 KiB 含 NUL 的文件按二进制跳过（本仓没有这么大的文本源）。 */
const MAX_FILE_BYTES = 2 * 1024 * 1024;

/**
 * 公开主机白名单：出现在 URL / 镜像引用里的主机必须在这里，或者命中下面的占位形态。
 *
 * 为什么要白名单而不是黑名单：自建域名是可以随便换的（本仓就换过一次），而「公开服务」是
 * 有限且稳定的集合。新增一个外部服务时往这里加一行即可，加的同时也就被人工看过一眼。
 */
const PUBLIC_HOSTS: Record<string, string> = {
  // 本仓自己（代码托管、发布、镜像）
  "github.com": "代码托管（本仓及其上游所在）",
  "api.github.com": "GitHub REST API",
  "raw.githubusercontent.com": "GitHub 原始文件分发",
  "help.github.com": "GitHub 文档",
  "ghcr.io": "GitHub 容器镜像仓库",
  "docker.io": "Docker Hub（镜像名默认注册表）",
  "hub.docker.com": "Docker Hub 页面",
  "get.docker.com": "Docker 安装脚本",
  // 依赖与工具链（公开源）
  "registry.npmjs.org": "npm 官方源",
  "www.npmjs.com": "npm 包页面",
  "registry.npmmirror.com": "npm 公开镜像源",
  "registry.yarnpkg.com": "Yarn 官方源",
  "nodejs.org": "Node.js 官方",
  "deb.nodesource.com": "NodeSource apt 源",
  "proxy.golang.org": "Go module 官方代理",
  "goproxy.cn": "Go module 公开镜像",
  "biomejs.dev": "Biome（lint/format 工具）",
  "nextjs.org": "Next.js 官方",
  "ui.shadcn.com": "shadcn/ui 组件文档",
  "www.w3.org": "W3C 标准",
  "img.shields.io": "README 徽章服务",
  "fonts.googleapis.com": "Google Fonts",
  "basemaps.cartocdn.com": "公开底图 CDN",
  "mapcn.dev": "地图组件库文档",
  // 上游模型供应商与协议文档（数据面上游）
  "anthropic.com": "Anthropic 官方",
  "www.anthropic.com": "Anthropic 官方",
  "api.anthropic.com": "Anthropic Messages API",
  "claude.ai": "Claude 产品站",
  "docs.claude.com": "Claude 文档",
  "platform.claude.com": "Claude 平台文档",
  "api.openai.com": "OpenAI API",
  "platform.openai.com": "OpenAI 平台文档",
  "chatgpt.com": "ChatGPT 产品站",
  "generativelanguage.googleapis.com": "Gemini API",
  "cloudcode-pa.googleapis.com": "Gemini Code Assist API",
  "google.com": "Google",
  "www.google.com": "Google",
  "cloud.google.com": "Google Cloud 文档",
  "ollama.com": "Ollama 官方",
  "opencode.ai": "OpenCode 官方",
  "openrouter.ai": "OpenRouter（第三方聚合）",
  "open.bigmodel.cn": "智谱开放平台（表单占位示例）",
  "api.minimaxi.com": "MiniMax API（表单占位示例）",
  "app.factory.ai": "Factory AI（客户端 UA 识别）",
  "cloud.langfuse.com": "Langfuse Cloud（公开 SaaS）",
  "aws-external-anthropic.us-east-1.api.aws": "AWS 官方模型端点",
  "api.telegram.org": "Telegram Bot API（通知通道）",
  "qyapi.weixin.qq.com": "企业微信 API（通知通道）",
  "oapi.dingtalk.com": "钉钉 API（通知通道）",
  // 上游项目的公开配套服务（本仓是二开，这些默认值继承自上游）
  "cch-plus.com": "上游配套的云价格表服务（默认值）",
  "ip-api.claude-code-hub.app": "上游配套的 IP 归属地服务（默认值）",
  "claude-code-hub.app": "上游项目站点",
  // 夹具与文档里的虚构域名（第三方中转示例）
  "api.test.com": "测试夹具里的虚构域名",
  "test.com": "测试夹具里的虚构域名（占位用）",
  "api.gptclubapi.xyz": "URL 重写用例里的示例中转地址（虚构/公开第三方）",
  "api.privnode.com": "密钥解析用例里的示例上游地址（虚构/公开第三方）",
  "api.legacy.com": "夹具里的旧版上游示例",
  "dummy.com": "夹具里的占位域名",
  "xxx.com": "夹具里的占位域名",
  "your-domain.com": "文档里的占位域名",
};

/** 占位主机形态：RFC 2606/6761 保留域、容器服务名、示例后缀与回环名。 */
const PLACEHOLDER_HOST = new RegExp(
  [
    // RFC 2606/6761：example.com/net/org、`.test`、`.invalid`、`.local`、localhost
    "^(?:[a-z0-9-]+\\.)*(?:example\\.(?:com|net|org|test|invalid)|test|invalid|local|internal|localhost)$",
    // 上游 Node 测试惯用的「单段 .example」写法（relay.example、vendor.example）
    "^(?:[a-z0-9-]+\\.)*example$",
    // 末段是「占位/服务」标签的主机（sentinel-redis-host、claude-code-hub-db、host、proxy…）
    "^(?:[a-z0-9-]+\\.)*(?:[a-z0-9-]*-)?(?:host|proxy|relay|upstream|endpoint|client|vendor|hub|db|pg|postgres|redis|redacted)$",
    "^(?:[a-z0-9-]+\\.)*(?:tld|host-placeholder)$",
  ].join("|"),
  "u"
);

/**
 * 镜像与注册表主机白名单。镜像引用出现在 compose / Dockerfile / 构建脚本里，自建 registry
 * 的地址一旦写进仓库就等于公开了内网服务位置，所以这里也按白名单收口。
 */
const PUBLIC_REGISTRIES: Record<string, string> = {
  "docker.io": "Docker Hub",
  "ghcr.io": "GitHub 容器镜像仓库",
  "docker.m.daocloud.io": "DaoCloud 公开镜像加速站（构建基座默认值）",
};

/**
 * 允许的私网网段写法：只放行 RFC 定义里的**标准**前缀。
 *
 * 为什么要这样区分：`10.0.0.0/8` 是协议定义（人人皆知），而 `10.1.0.0/24` 描述的是**真实子网**，
 * 那是拓扑信息——后者必须红。
 */
const STANDARD_PRIVATE_CIDRS = new Set([
  "10.0.0.0/8",
  "172.16.0.0/12",
  "192.168.0.0/16",
  "169.254.0.0/16",
  "100.64.0.0/10",
  "127.0.0.0/8",
  "0.0.0.0/0",
  "224.0.0.0/4",
  "240.0.0.0/4",
]);

/** RFC 5737 / RFC 3849 文档保留段：示例一律用它，写死允许。 */
const DOC_RANGE_IP =
  /^(?:192\.0\.2\.\d{1,3}|198\.51\.100\.\d{1,3}|203\.0\.113\.\d{1,3}|2001:db8::)$/u;

/** 允许清单：`文件 -> 值 -> 理由`。只放宽被点名的值。 */
const ALLOWLIST: Record<string, Record<string, string>> = {
  // 私网字面量：测试夹具惯例（真实地址不会出现在夹具里；这些值都是「文档里随手一写」的形态）
  "go/internal/adminapi/audit_ip_test.go": { "10.9.9.9": "审计 IP 夹具" },
  "go/internal/adminapi/error_text_redact.go": { "10.0.0.1": "遮蔽规则文档注释里的示例 DSN" },
  "go/internal/debugapi/debugapi_test.go": { "10.0.0.2": "调试面夹具" },
  "go/internal/forward/headers_test.go": { "10.0.0.1": "转发头夹具", "10.0.0.2": "转发头夹具" },
  "go/internal/guard/adapters_ip_test.go": {
    "10.0.0.1": "IP 适配器夹具",
    "10.0.0.2": "IP 适配器夹具",
  },
  "go/internal/guard/auth_test.go": {
    "10.0.0.1": "鉴权夹具",
    "10.0.0.2": "鉴权夹具",
    "10.0.0.3": "鉴权夹具",
  },
  "go/internal/ipgeo/client_test.go": {
    "10.0.0.1": "归属地夹具",
    "10.1.2.3": "归属地夹具（虚构，非生产网段）",
    "172.16.5.5": "归属地夹具",
    "192.168.1.1": "归属地夹具",
    "169.254.10.10": "链路本地夹具",
    "100.64.0.1": "CGN 夹具",
    "100.127.255.255": "CGN 夹具上界",
  },
  "go/internal/limit/abuse_test.go": {
    "10.0.0.1": "滥用检测夹具",
    "10.0.0.2": "滥用检测夹具",
    "10.0.0.9": "滥用检测夹具",
  },
  "src/app/[locale]/dashboard/_components/ip-details-dialog.test.tsx": {
    "192.168.1.1": "IP 详情弹窗夹具",
    "10.0.0.1": "IP 详情弹窗夹具",
    "100.64.0.1": "CGN 夹具（取 100.64/10 段首作占位，非真实设备地址）",
  },
  ".env.example": { "192.168.1.100": "APP_URL 注释里的示例值（非真实拓扑）" },
  "src/types/message.ts": { "192.168.1.1": "错误消息形态的注释示例" },

  // 单段主机名（容器服务名/夹具占位）：不含域名后缀，列在这里以保留可见性
  "go/internal/dataplane/provider_chain_test.go": { hc: "夹具里的容器服务名（EndpointURL）" },
  "go/internal/route/fingerprint_test.go": { x: "媒体指纹用例里的占位主机" },
  "tests/unit/ui/retired-database-capabilities.test.ts": { x: "退役端点扫描用例里的占位主机" },
  // 长 sk- 夹具：显式登记，避免「形状规则」被当成漏检
  "tests/unit/lib/utils/provider-text-parser.test.ts": {
    "sk-ant-abcdef1234567890ghijklmnopqrstuvwxyz": "密钥解析用例里的假密钥",
    "sk-ant-1234567890abcdefghijklmnopqrstuvwxyz": "密钥解析用例里的假密钥",
  },
};

/** 提交身份白名单：发布用身份（GitHub 提供的一次性邮箱，不含任何私人域名）。 */
const IDENTITY_EMAIL_ALLOWLIST = new Set(["fanxcv@users.noreply.github.com"]);

/** 身份红了时的修复指令（写进报错里，因为这条红起来只有一个原因：本机 git 配置还是私人邮箱）。 */
const IDENTITY_FIX_HINT =
  "修复：git config user.email '<用户名>@users.noreply.github.com'；" +
  "单次覆盖可用 git -c user.email=<该值> commit。";

type Violation = { file: string; line: number; rule: string; masked: string };

function trackedFiles(): string[] {
  const out = execFileSync("git", ["ls-files", "-z"], { cwd: ROOT, encoding: "buffer" });
  return out.toString("utf8").split("\0").filter(Boolean);
}

function readText(file: string): string | null {
  let buf: Buffer;
  try {
    buf = readFileSync(path.join(ROOT, file));
  } catch {
    return null;
  }
  if (buf.byteLength > MAX_FILE_BYTES) return null;
  if (buf.subarray(0, 8192).includes(0)) return null; // 二进制
  return buf.toString("utf8");
}

function lineOf(text: string, index: number): number {
  let line = 1;
  for (let i = 0; i < index; i += 1) if (text.charCodeAt(i) === 10) line += 1;
  return line;
}

/** 凭据片段遮蔽：只留首尾，避免在失败信息里二次泄漏。 */
function mask(value: string): string {
  if (value.length <= 8) return "<短值>";
  return `${value.slice(0, 4)}…${value.slice(-2)}（长度 ${value.length}）`;
}

function isPrivateIPv4(ip: string): boolean {
  const [a, b] = ip.split(".").map(Number);
  if (a === 10 || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168)) return true;
  if (a === 169 && b === 254) return true;
  if (a === 100 && b >= 64 && b <= 127) return true;
  return false;
}

function isLoopbackOrAny(host: string): boolean {
  return host === "127.0.0.1" || host === "0.0.0.0" || host === "::1" || host === "localhost";
}

function hostAllowed(host: string): boolean {
  if (!host) return true;
  // 模板与格式化占位（`%s.example.com`、`${HOST}`、`<pg-host>`）：运行期才成形，静态判不了。
  if (/[%<>${}]/u.test(host)) return true;
  if (isLoopbackOrAny(host)) return true;
  if (DOC_RANGE_IP.test(host)) return true;
  if (PLACEHOLDER_HOST.test(host)) return true;
  return Object.hasOwn(PUBLIC_HOSTS, host);
}

function allowedValue(file: string, value: string): boolean {
  return Object.hasOwn(ALLOWLIST[file] ?? {}, value);
}

const URL_RE = /https?:\/\/(?:[^\s/@:]+(?::[^\s/@]*)?@)?([A-Za-z0-9._~%-]+|\[[0-9a-fA-F:]+\])/gu;
/**
 * 镜像引用抽取：只认无歧义的写法——compose/清单的 `image:` 键、构建脚本的 `*_IMAGE=` 参数，
 * 以及 **Dockerfile 专属**的 `FROM` 行。
 *
 * 为什么 `FROM` 要按文件限定：SQL（`select … from information_schema.columns`）里的 `from`
 * 与 Go 结构体字段（`Registry: …`）都会把下一段当成「镜像名」——不加限定就会出现大面积误报。
 */
const IMAGE_RE = /(?:^\s*image:\s*|\b--build-arg\s+\w*IMAGE=|^\s*ARG\s+\w*IMAGE=)([^\s"']+)/gmu;
const DOCKERFILE_FROM_RE = /^\s*FROM\s+(?:--platform=\S+\s+)?([^\s"']+)/gmu;
function looksLikeDockerfile(file: string): boolean {
  const base = file.split("/").pop() ?? "";
  return base.startsWith("Dockerfile") || base.endsWith(".dockerfile");
}
/**
 * `host:port` 候选。**「文件:行号」写法必须排除**（`foo.ts:123`）：本仓 2500 多个受跟踪
 * 文件，这种引用遍布文档与注释，不排除会淹没真正的信号。排除放在代码里而不是正则里——
 * 把一长串扩展名塞进嵌套前瞻会写出难读且易错的正则（本文件第一版就写出了括号不配的版本）。
 */
const HOST_PORT_RE =
  /(?<![\w./-])([A-Za-z0-9][A-Za-z0-9.-]*\.[A-Za-z]{2,}|(?:\d{1,3}\.){3}\d{1,3}):(\d{2,5})\b/gu;
const FILE_EXT_LABELS = new Set(
  "ts tsx js jsx mjs cjs json jsonc md mdx go yaml yml lock mod sum sh bash sql lua html css scss txt toml ps1 proto conf ini crt pem xml svg png jpg jpeg webp gif ico woff woff2 map patch diff env example".split(
    " "
  )
);
function looksLikeFileReference(host: string): boolean {
  const label = host.split(".").pop() ?? "";
  return FILE_EXT_LABELS.has(label.toLowerCase());
}
const IPV4_RE = /(?<![\d.])((?:\d{1,3}\.){3}\d{1,3})(?![\d.])/gu;
const CIDR_RE = /(?<![\d.])((?:\d{1,3}\.){3}\d{1,3})\/(\d{1,2})(?![\d.])/gu;
const LOCAL_PATH_RE = /(?<![\w.-])(\/(?:mnt|home|Users)\/[^\s"'`)]+|C:[\\/]Users[\\/][^\s"'`)]+)/gu;
const PATH_PLACEHOLDER = /(your-username|your-user|你的用户名|username|<[^>]*>|\$\{[^}]*\}|…)/u;
const CREDENTIAL_RES: { name: string; re: RegExp }[] = [
  { name: "GitHub token", re: /\bgh[pousr]_[A-Za-z0-9]{20,}\b/gu },
  { name: "GitHub 细粒度 token", re: /\bgithub_pat_[A-Za-z0-9_]{20,}\b/gu },
  { name: "私钥块", re: /-----BEGIN [A-Z ]*PRIVATE KEY-----/gu },
  { name: "AWS access key", re: /\bAKIA[0-9A-Z]{16}\b/gu },
  { name: "Slack token", re: /\bxox[baprs]-[A-Za-z0-9-]{10,}\b/gu },
  { name: "模型 API key", re: /\bsk-[A-Za-z0-9_-]{40,}\b/gu },
  { name: "Bearer token", re: /\bBearer\s+[A-Za-z0-9._-]{32,}\b/gu },
  { name: "JWT", re: /\beyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\./gu },
];
/** 凭据型 URL 的 `user:pass@host`：主机必须是占位/回环/服务名（真实内网 DSN 会红）。 */
const USERINFO_RE = /:\/\/([^/\s:@]+):([^/\s@]+)@([A-Za-z0-9._-]+|\[[0-9a-fA-F:]+\])/gu;

function scan(file: string, text: string): Violation[] {
  const out: Violation[] = [];
  const push = (index: number, rule: string, masked: string) =>
    out.push({ file, line: lineOf(text, index), rule, masked });

  for (const m of text.matchAll(URL_RE)) {
    const host = (m[1] ?? "").toLowerCase().replace(/^\[|\]$/gu, "");
    // 紧跟着 `/` 或 `:` 的字符说明这是「host 段」；若是 `:` 则上一段是 userinfo（正则回退所致）。
    const next = text[(m.index ?? 0) + m[0].length];
    if (next === ":") continue;
    if (hostAllowed(host)) continue;
    if (allowedValue(file, host)) continue;
    push(m.index ?? 0, "URL 主机不在公开白名单", host);
  }

  const imageRefs = looksLikeDockerfile(file)
    ? [...text.matchAll(IMAGE_RE), ...text.matchAll(DOCKERFILE_FROM_RE)]
    : [...text.matchAll(IMAGE_RE)];
  for (const m of imageRefs) {
    const ref = m[1] ?? "";
    if (ref.includes("$") || ref.includes("<")) continue; // 变量/模板，由运行期注入
    const first = ref.split("/")[0] ?? "";
    if (!first.includes(".")) continue; // 无注册表主机（Docker Hub 简写）
    if (Object.hasOwn(PUBLIC_REGISTRIES, first)) continue;
    if (allowedValue(file, first)) continue;
    push(m.index ?? 0, "镜像注册表不在公开白名单", first);
  }

  for (const m of text.matchAll(HOST_PORT_RE)) {
    const host = (m[1] ?? "").toLowerCase();
    if (looksLikeFileReference(host)) continue; // `foo.ts:123` 这类「文件:行号」
    if (isPrivateIPv4(host)) continue; // 交给私网规则（它带允许清单）
    if (hostAllowed(host)) continue;
    if (allowedValue(file, host)) continue;
    push(m.index ?? 0, "host:port 的主机不在公开白名单", host);
  }

  for (const m of text.matchAll(CIDR_RE)) {
    const cidr = `${m[1]}/${m[2]}`;
    if (!isPrivateIPv4(m[1] ?? "")) continue;
    if (STANDARD_PRIVATE_CIDRS.has(cidr)) continue;
    push(m.index ?? 0, "私网网段不是标准 RFC 前缀（疑似真实子网）", cidr);
  }

  for (const m of text.matchAll(IPV4_RE)) {
    const ip = m[1] ?? "";
    if (!isPrivateIPv4(ip)) continue;
    if (text[(m.index ?? 0) + ip.length] === "/") continue; // CIDR 写法由网段规则判定
    if (allowedValue(file, ip)) continue;
    push(m.index ?? 0, "私网/特殊段地址字面量", ip);
  }

  for (const m of text.matchAll(LOCAL_PATH_RE)) {
    const value = m[1] ?? "";
    if (PATH_PLACEHOLDER.test(value)) continue;
    push(m.index ?? 0, "本机绝对路径", value.slice(0, 40));
  }

  for (const { name, re } of CREDENTIAL_RES) {
    for (const m of text.matchAll(re)) {
      const value = m[0] ?? "";
      if (allowedValue(file, value)) continue;
      push(m.index ?? 0, `凭据形态（${name}）`, mask(value));
    }
  }

  for (const m of text.matchAll(USERINFO_RE)) {
    const host = (m[3] ?? "").toLowerCase();
    if (isPrivateIPv4(host)) continue; // 私网地址由 R4 判（它带允许清单）
    const end = (m.index ?? 0) + m[0].length;
    if (/[$%{<]/u.test(text.slice(end, end + 1))) continue; // `…@host-${SUFFIX}`：运行期拼装
    if (hostAllowed(host)) continue;
    if (allowedValue(file, host)) continue;
    push(m.index ?? 0, "凭据型 URL 指向非占位主机", host);
  }

  return out;
}

/**
 * 内容扫描的自免集合。**只有本文件**：规则书得把要禁的模式与放行的值原样写出来，扫它就是循环；
 * 而且它本就在「每次改动这个钉子的人」的视野里。下面有断言钉住这个集合只准有一个成员。
 */
const SELF_EXEMPT = new Set(["tests/unit/docs/sensitive-patterns.test.ts"]);

const files = trackedFiles();
const scanned = files.filter((f) => readText(f) !== null && !SELF_EXEMPT.has(f));
const violations: Violation[] = [];
/** 各规则的候选计数：防空跑——抽取器被改坏时这里会掉到 0，下面有下限断言。 */
const counts = { files: files.length, urlHosts: 0, imageRefs: 0, hostPorts: 0, cidrs: 0 };

for (const file of scanned) {
  const text = readText(file) ?? "";
  counts.urlHosts += [...text.matchAll(URL_RE)].length;
  counts.imageRefs += looksLikeDockerfile(file)
    ? [...text.matchAll(IMAGE_RE), ...text.matchAll(DOCKERFILE_FROM_RE)].length
    : [...text.matchAll(IMAGE_RE)].length;
  counts.hostPorts += [...text.matchAll(HOST_PORT_RE)].filter(
    (m) => !looksLikeFileReference((m[1] ?? "").toLowerCase())
  ).length;
  counts.cidrs += [...text.matchAll(CIDR_RE)].length;
  violations.push(...scan(file, text));
}

const describeViolations = (list: Violation[]) =>
  list.map((v) => `${v.file}:${v.line} [${v.rule}] ${v.masked}`).join("\n");

describe("受跟踪文件里的敏感信息", () => {
  test("扫描量与规则命中量达标（防空跑）", () => {
    expect(counts.files).toBeGreaterThan(2000);
    expect(scanned.length).toBeGreaterThan(2000);
    expect(SELF_EXEMPT.size, "内容扫描的自免集合只准包含本钉子文件").toBe(1);
    for (const exempt of SELF_EXEMPT) expect(files).toContain(exempt);
    expect(counts.urlHosts).toBeGreaterThan(500); // URL 主机抽取器真的在跑
    expect(counts.imageRefs).toBeGreaterThan(5); // 镜像引用抽取器
    expect(counts.hostPorts).toBeGreaterThan(20); // host:port 抽取器（已排除文件引用）
    expect(counts.cidrs).toBeGreaterThan(5); // CIDR 抽取器
  });

  test("没有内部标识、真实地址、个人路径或凭据形态", () => {
    expect(
      violations,
      `发现敏感信息（放行要写进 ALLOWLIST 并写明理由）：\n${describeViolations(violations)}`
    ).toEqual([]);
  });
});

describe("最近一次提交的元数据", () => {
  const meta = execFileSync("git", ["log", "-1", "--format=%an%n%ae%n%cn%n%ce%n%B"], {
    cwd: ROOT,
    encoding: "utf8",
  });
  const [authorName, authorEmail, committerName, committerEmail, ...rest] = meta.split("\n");
  const message = rest.join("\n");

  test("身份用仓库约定邮箱（不含私人域名与私人邮箱）", () => {
    // 为什么只看最近一次提交：历史里大量旧分支保留了本机身份（`fan@<私有域名>`），全量扫描
    // 会在第一天就红且无从修复；而**被发布出去的**只有发布流程折叠出来的那一个提交，
    // 因此把检查点放在「将要发布的那次提交」上，兼顾可执行与零阻力。
    expect(
      IDENTITY_EMAIL_ALLOWLIST.has(authorEmail ?? ""),
      `作者邮箱：${authorEmail}。${IDENTITY_FIX_HINT}`
    ).toBe(true);
    expect(
      IDENTITY_EMAIL_ALLOWLIST.has(committerEmail ?? ""),
      `提交者邮箱：${committerEmail}。${IDENTITY_FIX_HINT}`
    ).toBe(true);
    expect(authorName, "作者名不含私人域名").not.toMatch(/fan-x|qksxin|spn3/iu);
    expect(committerName, "提交者名不含私人域名").not.toMatch(/fan-x|qksxin|spn3/iu);
  });

  test("提交信息不含内部标识、真实地址、个人路径或凭据形态", () => {
    const found = scan("<commit message>", message);
    expect(found, `提交信息里发现敏感信息：\n${describeViolations(found)}`).toEqual([]);
  });
});
