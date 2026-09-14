import { Link } from "@/i18n/routing";

/**
 * 页脚展示的版本号，**构建期注入**。
 *
 * 真源是发布时注入的版本：`release` 脚本把 `APP_VERSION` 交给 UI 静态导出，
 * 导出脚本再以 `NEXT_PUBLIC_APP_VERSION` 传给 `next build`，于是该值在构建阶段被写进
 * 每一页的 HTML。为什么必须构建期：生产镜像是 Go 二进制 + 嵌入产物，没有 Node、
 * 也没有仓库工作区，运行期根本读不到任何仓库文件。
 *
 * 这里曾经 `readFile(process.cwd() + "/VERSION")`：那份文件是 Node 时代的产物，导出时把
 * 0.9.5 烙进每页，而同一进程的 `/api/health` 报 0.9.0、镜像 tag 又是 1.0.1——排障时据此
 * 误判过「有两台不同的实例」。该文件已删，版本取值的唯一实现在 `go/internal/appversion`。
 *
 * 未注入时**不显示版本**，而不是写死一个兜底值：兜底常量会立刻变成第四个会漂的版本源。
 * （导出脚本永远会注入：APP_VERSION，或退回 package.json 的声明值；所以这个分支只在
 * 绕过导出脚本直接 `next build` 时才会走到，并且导出脚本会在拿不到版本时打印告警。）
 */
const injectedVersion = (process.env.NEXT_PUBLIC_APP_VERSION ?? "").trim();

function displayVersion(): string {
  const bare = injectedVersion.replace(/^v/i, "");
  return bare === "" ? "" : `v${bare}`;
}

export function Footer() {
  const year = new Date().getFullYear();
  const version = displayVersion();

  return (
    <footer className="border-t border-border bg-background/80">
      <div className="mx-auto flex w-full max-w-[100rem] flex-col items-center justify-between gap-2 px-6 py-6 text-sm text-muted-foreground sm:flex-row">
        <p className="text-center sm:text-left">
          © {year} CC Hub Go{version === "" ? "" : ` · ${version}`}
        </p>
        <Link
          href="https://github.com/fanxcv/claude-code-hub-go"
          target="_blank"
          rel="noopener noreferrer"
          className="transition-colors hover:text-primary"
        >
          GitHub
        </Link>
      </div>
    </footer>
  );
}
