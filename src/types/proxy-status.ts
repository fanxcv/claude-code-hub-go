/**
 * 代理状态追踪相关类型定义
 */

/**
 * 活跃的代理请求信息
 */
export interface ActiveRequest {
  /** 请求 ID (messageContext.id) */
  requestId: number;
  /** API Key 名称 */
  keyName: string;
  /** 供应商 ID */
  providerId: number;
  /** 供应商名称 */
  providerName: string;
  /** 模型名称 */
  model: string;
  /** 请求开始时间戳 (ms) */
  startTime: number;
}

/**
 * 最后一次请求信息
 */
export interface LastRequest {
  /** 请求 ID */
  requestId: number;
  /** API Key 名称 */
  keyName: string;
  /** 供应商 ID */
  providerId: number;
  /** 供应商名称 */
  providerName: string;
  /** 模型名称 */
  model: string;
  /** 请求结束时间戳 (ms) */
  endTime: number;
}

/**
 * 用户代理状态（内部使用）
 */
export interface UserProxyStatus {
  /** 用户 ID */
  userId: number;
  /** 用户名称 */
  userName: string;
  /** 当前活跃的请求 Map (requestId -> ActiveRequest) */
  activeRequests: Map<number, ActiveRequest>;
  /** 最后一次请求信息 */
  lastRequest: LastRequest | null;
  /** 是否已从数据库加载过（避免重复查询） */
  dbLoaded: boolean;
}

/**
 * 代理状态 API 响应格式
 */
export interface ProxyStatusResponse {
  users: Array<{
    userId: number;
    userName: string;
    /** 当前活跃请求数量 */
    activeCount: number;
    /** 活跃请求列表 */
    activeRequests: Array<{
      requestId: number;
      keyName: string;
      providerId: number;
      providerName: string;
      model: string;
      startTime: number;
      /** 当前已运行时长 (ms) */
      duration: number;
    }>;
    /** 最后一次请求信息 */
    lastRequest: {
      requestId: number;
      keyName: string;
      providerId: number;
      providerName: string;
      model: string;
      endTime: number;
      /** 距离现在的时长 (ms) */
      elapsed: number;
    } | null;
  }>;
}
