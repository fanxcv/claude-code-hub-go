// 为「逐页改造清单」取证：列出每个页面目录下的客户端视图组件所调用的 /api/v1 与 /api 路径。
import { execSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";

// 仓根：可用环境变量覆盖，默认按脚本位置推（<root>/go/cmd/uipoc/tools/）。
const root = process.env.CCH_REPO_ROOT ?? path.resolve(import.meta.dirname, "..", "..", "..", "..");

const pages = execSync('find "src/app/[locale]" -name page.tsx', { cwd: root, encoding: "utf8" })
  .split("\n").filter(Boolean).sort();

const pathsIn = (text) => {
  const out = new Set();
  for (const part of text.split('"/api/').slice(1)) out.add("/api/" + part.split(/["'`\s]/)[0]);
  for (const part of text.split('`/api/').slice(1)) out.add("/api/" + part.split(/["'`\s]/)[0].replace(/\$\{[^}]*\}/g, "{}"));
  return out;
};

for (const page of pages) {
  const dir = path.dirname(page);
  const files = execSync(`find . -maxdepth 3 -type f \\( -name '*.ts' -o -name '*.tsx' \\)`, { cwd: path.join(root, dir), encoding: "utf8" })
    .split("\n").filter(Boolean);
  const found = new Set();
  for (const f of files) {
    const text = fs.readFileSync(path.join(root, dir, f), "utf8");
    for (const u of pathsIn(text)) found.add(u);
  }
  // 页面自身声明的服务端依赖
  const self = fs.readFileSync(path.join(root, page), "utf8");
  const client = self.includes('"use client"') ? "client" : "SSR";
  const srvDeps = [...new Set([...self.matchAll(/"@\/(?:repository|actions)[^"]*"/g)].map((m) => m[0].replace(/"/g, "")))];
  const usesSession = self.includes("getSession") ? "session" : "";
  console.log(`${client}\t${page.replace("src/app/[locale]/", "")}\t${[usesSession, ...srvDeps].filter(Boolean).join(",") || "-"}\t${[...found].sort().join(" ") || "(无直接 fetch，靠 hooks)"}`);
}
