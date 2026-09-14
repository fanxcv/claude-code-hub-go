#!/usr/bin/env bun
/**
 * 把 Next 静态导出产物转成**压缩态嵌入目录**，供 `go/internal/uiapp` 的 `go:embed` 收编。
 *
 * 为什么需要：UI 产物几乎全是文本（HTML 壳、RSC payload `.txt`、JS/CSS），原始态
 * **142.3 MiB / 736 文件**——直接 embed 会把这 142 MiB 打进二进制。brotli 对这类文本
 * 有 4.0–4.4x 的压缩率，压后约 **32.8 MiB**（q9 实测）。二进制随之从 148 MiB 量级降到 40 MiB 量级。
 *
 * 设计（三条）：
 *
 *  1. **只压文本**。`.png/.jpg/.ico/.woff2` 等已压缩格式再压几乎没有收益（实测 PNG 仅 1.1x）
 *     且会白费构建时间，故原样保留。判据是扩展名白名单，不是「压了看变小没有」——
 *     后者会让产物内容决定元数据，构建不可复现。
 *  2. **每个文件独立压缩**，不打包成单一归档。这样服务端可以**按需解压单个文件**，
 *     不必为一次请求解压整棵树；也让 embed.FS 的路径语义（含 `all:` 前缀的必要性）保持不变。
 *  3. **元数据写在 MANIFEST.json**，而不是靠扩展名后缀（`.br`）猜。理由：`uiapp` 的择路逻辑
 *     按 URL 路径精确匹配 embed key，若把 `index.html` 存成 `index.html.br`，择路与 MIME
 *     推断都要跟着改（两处都可能漏改）。元数据是显式的、单点的，且能顺带记录原始大小
 *     与原始扩展名，MIME 推断仍按**逻辑名**进行——`.txt` 的 `charset=utf-8` 修复因此不受影响。
 *
 * 用法（仓库根目录）：
 *   bun scripts/build-ui-embed.mjs                    # out/ -> go/internal/uiapp/assets
 *   bun scripts/build-ui-embed.mjs --src out --dst go/internal/uiapp/assets
 *   bun scripts/build-ui-embed.mjs --quality 11      # 更慢更小（默认 9）
 *   bun scripts/build-ui-embed.mjs --keep-raw        # 只写 MANIFEST，不压（回退对照用）
 *
 * 与 build-ui-export.mjs 的分工：那个脚本负责 **导出**（跑 next build 并收编产物），
 * 本脚本负责 **压缩**。可以单独重跑本脚本（产物已在 out/ 时不必重跑导出）。
 *
 * 回退方式：
 * bun scripts/build-ui-embed.mjs --keep-raw
 * 该档下 MANIFEST 里每个条目的 `raw: true`，`uiapp` 据此直出明文（不设 Content-Encoding），
 * 行为与压缩前完全一致。
 */
import fs from "node:fs";
import path from "node:path";
import zlib from "node:zlib";

const ROOT = process.cwd();
const args = process.argv.slice(2);

/** 取 `--name value` 形式的参数。 */
function argValue(name, fallback) {
  const i = args.indexOf(name);
  if (i >= 0 && i + 1 < args.length) return args[i + 1];
  return fallback;
}

const SRC = path.resolve(ROOT, argValue("--src", "out"));
const DST = path.resolve(ROOT, argValue("--dst", path.join("go", "internal", "uiapp", "assets")));
const QUALITY = Number(argValue("--quality", "9"));
const KEEP_RAW = args.includes("--keep-raw");
/**
 * BUILD_ID 由 build-ui-export.mjs 从 .next/BUILD_ID 读出后传入。
 *
 * 它必须在这里写回：本脚本会清空 DST，而 BUILD_ID 不在 out/ 里（导出脚本是复制完之后
 * 单独写入的），不传就会把它弄丢——表现为 /readyz 的构建号变「未知构建号」，
 * 「二进制与前端产物同版」的核对随之失效。
 */
const BUILD_ID = argValue("--build-id", "");
const MANIFEST = "MANIFEST.json";

/**
 * 需要压缩的扩展名白名单。
 *
 * 判据是「文本 vs 已压缩二进制」，不是「压了会不会变小」：白名单是构建常量，
 * 产物内容变了也不会改变构建决策（可复现）。缺项时的表现是「白压一次、体积不变」，
 * 而不是「该压的没压」——所以宁可宽列。
 */
const COMPRESSIBLE = new Set([
  ".html",
  ".htm",
  ".txt",
  ".js",
  ".mjs",
  ".cjs",
  ".css",
  ".json",
  ".map",
  ".svg",
  ".xml",
  ".webmanifest",
  ".csv",
]);

/** 逐文件压缩；返回是否压过。 */
function compressFile(absPath) {
  const raw = fs.readFileSync(absPath);
  // 阈值：小于 1 KiB 的文件压缩收益不足（brotli 头部开销 + 解压代码路径），保持明文更省事。
  // ponytail: 固定 1 KiB 阈值，若将来产物里有大量小文件再按实测调。
  if (raw.length < 1024) return null;
  return zlib.brotliCompressSync(raw, {
    params: {
      [zlib.constants.BROTLI_PARAM_QUALITY]: QUALITY,
      // 文本类默认模式（BROTLI_MODE_TEXT）；对 HTML/JS 比通用模式略优。
      [zlib.constants.BROTLI_PARAM_MODE]: zlib.constants.BROTLI_MODE_TEXT,
    },
  });
}

/** 递归列出相对路径（文件，不含目录）。 */
function listFiles(root) {
  const out = [];
  const walk = (dir, prefix) => {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const rel = prefix ? `${prefix}/${entry.name}` : entry.name;
      if (entry.isDirectory()) walk(path.join(dir, entry.name), rel);
      else if (entry.isFile()) out.push(rel);
    }
  };
  walk(root, "");
  return out.sort();
}

function main() {
  if (!fs.existsSync(SRC)) {
    console.error(`[build-ui-embed] 源产物目录不存在：${SRC}（先跑 bun scripts/build-ui-export.mjs）`);
    process.exit(1);
  }

  const files = listFiles(SRC);
  if (files.length === 0) {
    console.error(`[build-ui-embed] 源产物目录为空：${SRC}`);
    process.exit(1);
  }

  fs.rmSync(DST, { recursive: true, force: true });
  fs.mkdirSync(DST, { recursive: true });

  const entries = {};
  let rawTotal = 0;
  let storedTotal = 0;
  let compressedCount = 0;
  let keptCount = 0;

  const started = Date.now();
  for (const rel of files) {
    const srcAbs = path.join(SRC, rel);
    const dstAbs = path.join(DST, rel);
    const rawSize = fs.statSync(srcAbs).size;
    rawTotal += rawSize;

    const ext = path.extname(rel).toLowerCase();
    const shouldCompress = !KEEP_RAW && COMPRESSIBLE.has(ext);
    const compressed = shouldCompress ? compressFile(srcAbs) : null;

    fs.mkdirSync(path.dirname(dstAbs), { recursive: true });
    if (compressed) {
      fs.writeFileSync(dstAbs, compressed);
      entries[rel] = { encoding: "br", rawSize, storedSize: compressed.length, quality: QUALITY };
      storedTotal += compressed.length;
      compressedCount += 1;
    } else {
      // 原样复制（二进制，或 --keep-raw 档，或小于阈值的小文件）。
      fs.copyFileSync(srcAbs, dstAbs);
      entries[rel] = { encoding: "identity", rawSize, storedSize: rawSize };
      storedTotal += rawSize;
      keptCount += 1;
    }
  }

  const manifest = {
    // 版本号供 uiapp 校验「二进制里的元数据格式与代码期望一致」；不认识就拒绝启动。
    version: 1,
    generatedAt: new Date().toISOString(),
    quality: QUALITY,
    keepRaw: KEEP_RAW,
    counts: { files: files.length, compressed: compressedCount, identity: keptCount },
    bytes: { raw: rawTotal, stored: storedTotal },
    // 键是**逻辑路径**（与 URL 路径一一对应），不是磁盘上的存储名。
    entries,
  };
  fs.writeFileSync(path.join(DST, MANIFEST), `${JSON.stringify(manifest)}\n`);
  // .gitkeep 保留（产物目录靠 .gitignore 忽略，仅 .gitkeep 入 git）。
  fs.writeFileSync(path.join(DST, ".gitkeep"), "");
  if (BUILD_ID) {
    // 明文写（几十字节，远低于压缩阈值），uiapp 直接读它当构建号。
    fs.writeFileSync(path.join(DST, "BUILD_ID"), `${BUILD_ID}\n`);
  }

  const MiB = (n) => (n / (1 << 20)).toFixed(1);
  const seconds = ((Date.now() - started) / 1000).toFixed(1);
  console.log(
    `[build-ui-embed] ${files.length} 文件：raw ${MiB(rawTotal)} MiB -> stored ${MiB(storedTotal)} MiB ` +
      `(${(rawTotal / storedTotal).toFixed(2)}x)，压缩 ${compressedCount} / 原样 ${keptCount}，用时 ${seconds}s`,
  );
  console.log(`[build-ui-embed] 产物已收进 ${path.relative(ROOT, DST)}/（含 ${MANIFEST}）`);
}

main();
