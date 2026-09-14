/**
 * 用 Node 侧的 CPT 转换器产出 golden，供 Go 侧逐模型比对。
 *
 * 用法（仓库根）：
 *   bun scripts/cpt-convert-golden.ts <fixture.json> <out.json>
 *
 * 为什么需要它：`internal/jobs/cpt_convert.go` 是 `cpt-convert.ts` 的移植，
 * 而「移植是否等价」不能靠读代码断言——必须让**同一份 CPT 输入**过两个实现，
 * 再逐字段比。本脚本负责 Node 那一侧，输出即为 Go 测试的期望值。
 */
import { readFileSync, writeFileSync } from "node:fs";
import { convertCptTable } from "@/lib/price-sync/cpt-convert";
import { parseCptTableValue } from "@/lib/price-sync/cpt-schema";

const [fixturePath, outputPath] = process.argv.slice(2);
if (!fixturePath || !outputPath) {
  console.error("用法: bun scripts/cpt-convert-golden.ts <fixture.json> <out.json>");
  process.exit(2);
}

const parsed = JSON.parse(readFileSync(fixturePath, "utf8")) as unknown;
const validated = parseCptTableValue(parsed);
if (!validated.ok) {
  console.error(`CPT 校验失败: ${validated.error}`);
  process.exit(1);
}

const converted = convertCptTable(validated.data);

// JSON.stringify 的键序即 Node 对象的插入顺序；Go 侧比对时按语义（解成 any 再规范化）比，
// 不依赖键序，但这里保留原文便于人读差异。
writeFileSync(
  outputPath,
  `${JSON.stringify(
    {
      source: "src/lib/price-sync/cpt-convert.ts convertCptTable",
      fixture: fixturePath,
      version: converted.version,
      currency: converted.currency,
      refreshedAt: converted.refreshedAt,
      vendors: converted.vendors,
      providers: converted.providers,
      models: converted.models,
    },
    null,
    2
  )}\n`
);

console.log(
  `[golden] models=${Object.keys(converted.models).length} vendors=${converted.vendors.length} → ${outputPath}`
);
