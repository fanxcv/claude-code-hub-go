import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(__dirname, "..");

// ---------------------------------------------------------------------------
// Shared resolve alias
// ---------------------------------------------------------------------------

export function sharedResolve(opts?: { includeMessages?: boolean }) {
  const alias: Record<string, string> = {
    "@": path.resolve(root, "src"),
    "@lobehub/fluent-emoji/es/FluentEmoji": path.resolve(
      root,
      "node_modules/@lobehub/fluent-emoji/es/FluentEmoji/index.js"
    ),
    "@lobehub/fluent-emoji": path.resolve(root, "tests/fluent-emoji.mock.tsx"),
  };
  if (opts?.includeMessages) {
    alias["@messages"] = path.resolve(root, "messages");
  }
  return { alias };
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

export function parsePositiveInt(value: string | undefined, fallback: number): number {
  if (!value) return fallback;
  const parsed = Number.parseInt(value.trim(), 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
}

export function parseWorkerLimit(
  value: string | undefined,
  fallback: number | string
): number | string {
  if (!value) return fallback;
  const trimmed = value.trim();
  if (/^\d+%$/.test(trimmed)) return trimmed;
  const parsed = Number.parseInt(trimmed, 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
}

export const defaultTestExclude = [
  "node_modules",
  ".next",
  "dist",
  "build",
  "coverage",
  "**/*.d.ts",
];
