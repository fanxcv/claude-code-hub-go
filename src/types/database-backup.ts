// 数据库备份相关类型定义

/**
 * 数据库状态信息
 */
export interface DatabaseStatus {
  isAvailable: boolean;
  containerName: string;
  databaseName: string;
  databaseSize: string;
  tableCount: number;
  postgresVersion: string;
  error?: string;
}

/**
 * 导入选项
 */
export interface ImportOptions {
  /** 导入前是否清除现有数据（覆盖模式） */
  cleanFirst: boolean;
}

/**
 * 导入进度事件
 */
export interface ImportProgressEvent {
  type: "progress" | "complete" | "error";
  message: string;
  exitCode?: number;
}

/**
 * 执行结果
 */
export interface ExecutionResult {
  success: boolean;
  message: string;
  exitCode?: number;
  error?: string;
}
