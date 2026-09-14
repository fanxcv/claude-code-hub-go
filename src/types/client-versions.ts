/**
 * 客户端版本统计的展示契约。
 *
 * 来源：原 `src/lib/client-version-checker.ts` 的 `ClientVersionStats`。Node 退役后该文件的实现
 * （读 `@/repository/client-versions` + Redis）已删，但两个页面（`settings/client-versions` 的
 * 列表页与统计表组件）仍以它为展示类型，故把类型下移到 `src/types/`。
 * 数据由 Go 侧提供（REST 面），本文件不含任何运行时代码。
 */
export interface ClientVersionStats {
  /**
   * 客户端类型
   *
   * 可能的值：
   * - "claude-vscode": VSCode 插件
   * - "claude-cli": 纯 CLI
   * - "claude-cli-unknown": 无法识别的旧版本
   * - "anthropic-sdk-typescript": SDK
   * - 其他客户端类型
   */
  clientType: string;
  /** 最新 GA 版本，无则为 null */
  gaVersion: string | null;
  /** 使用该客户端的总用户数 */
  totalUsers: number;
  /** 用户详情列表 */
  users: {
    userId: number;
    username: string;
    version: string;
    lastSeen: Date;
    isLatest: boolean; // 是否是最新版本
    needsUpgrade: boolean; // 是否需要升级
  }[];
}
