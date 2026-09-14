// Package session 提供客户端会话的身份、绑定、租约与跟踪。
//
// 边界：限流与配额不属本包（internal/limit 持有并发检查与成本窗口）；本包只做
// 会话身份提取与规范化、Redis 绑定状态机（canonical+legacy 双镜像）、discovery
// 租约、活跃会话跟踪，以及给守卫链的 SessionBinder 适配。
//
// 键形制与 Node 逐字一致（lua/ 下的脚本与 src/lib/redis/session-binding.ts）：
// 切换期间两侧要能互相命中同一组键。所有脚本经 internal/ratelimit 执行（EVALSHA
// 带 NOSCRIPT 回退），脚本正文由仓库根 lua/ 目录守护逐字节一致。
package session
