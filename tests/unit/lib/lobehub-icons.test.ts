/**
 * @vitest-environment happy-dom
 */

import { readFileSync } from "node:fs";
import path from "node:path";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import * as icons from "@/lib/lobehub-icons/provider-types";
import { PROVIDER_TYPE_CONFIG } from "@/lib/provider-type-utils";
import { getPublicStatusVendorIconComponent } from "@/lib/public-status/vendor-icon";

/**
 * 图标引入面钉子。
 *
 * 为什么需要它：`@lobehub/icons` 的根 barrel 会把每个图标的 Mono/Text/Avatar 变体
 * 无条件挂到同一对象上，打包器无法逐变体裁掉——实测三处静态 import 会把首屏 icons
 * chunk 顶到 833.5 KiB。改走 `es/<Icon>/components/<Variant>` 子路径后降到约十分之一。
 * 一旦有人「顺手」改回根 barrel，体积会静默回到 833.5 KiB 而测试仍全绿，
 * 故在此钉住引入面，并用真实渲染证明换源后图标组件仍可用。
 */

const CONSUMERS: { file: string; specifier: string }[] = [
  {
    file: "src/lib/provider-type-utils.tsx",
    specifier: "@/lib/lobehub-icons/provider-types",
  },
  { file: "src/lib/model-vendor-icons.tsx", specifier: "@/lib/lobehub-icons/vendor" },
  {
    file: "src/lib/public-status/vendor-icon.ts",
    specifier: "@/lib/lobehub-icons/public-status",
  },
];

const ICON_FACES = ["provider-types", "vendor", "public-status"];

function sourceOf(relativePath: string): string {
  return readFileSync(path.join(process.cwd(), relativePath), "utf8");
}

function renderIcon(icon: unknown): string {
  return renderToStaticMarkup(createElement(icon as React.ComponentType));
}

describe("图标引入面", () => {
  it("三个消费方各自走对应的本地图标面，不得回引根 barrel", () => {
    for (const { file, specifier } of CONSUMERS) {
      const source = sourceOf(file);
      expect(source, file).toContain(`from "${specifier}"`);
      expect(source, file).not.toContain('from "@lobehub/icons"');
    }
  });

  it("本地图标面只用变体子路径，不引入根 barrel", () => {
    for (const face of ICON_FACES) {
      const source = sourceOf(`src/lib/lobehub-icons/${face}.ts`);
      // 只扫 import 行：文件头注释里为说明「为什么不用根 barrel」会原样写一次该包名
      const importLines = source.split("\n").filter((line) => line.startsWith("import "));
      const specifiers = importLines.map((line) => /from "([^"]*)"/.exec(line)?.[1] ?? "");
      expect(specifiers.length, face).toBeGreaterThan(0);
      for (const specifier of specifiers) {
        expect(specifier.startsWith("@lobehub/icons/es/"), face).toBe(true);
      }
    }
  }, 15000);

  it("三个图标面各自独立成模块（不同消费方不共用整套图标）", () => {
    const counts = ICON_FACES.map(
      (face) => sourceOf(`src/lib/lobehub-icons/${face}.ts`).match(/^export const /gm)?.length ?? 0
    );
    // 供应商类型面只需 4 个；若有人把三面合并回一个模块，这条会立刻变红
    expect(counts[0]).toBe(4);
    expect(counts[1]).toBeGreaterThan(70);
    expect(counts[2]).toBeGreaterThan(25);
  });

  it("变体各自独立，未被串成同一个组件", () => {
    expect(icons.Claude.Color).not.toBe(icons.Gemini.Color);
    expect(icons.Anthropic.Avatar).not.toBe(icons.Claude.Color);
  });

  it("供应商类型配置的图标仍可渲染出 svg", () => {
    for (const [type, config] of Object.entries(PROVIDER_TYPE_CONFIG)) {
      const markup = renderIcon(config.icon);
      expect(markup, `provider type ${type} 应渲染出 svg`).toContain("<svg");
    }
  });

  it("公开状态页的供应商图标仍可渲染出 svg", () => {
    const { Icon, iconKey } = getPublicStatusVendorIconComponent({
      modelName: "gpt-4o",
      vendorIconKey: "openai",
    });
    expect(iconKey).toBe("openai");
    expect(renderIcon(Icon)).toContain("<svg");
  });
});
