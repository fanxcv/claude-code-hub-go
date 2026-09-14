package jobs

import (
	"context"
	"fmt"
	"sync"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件复刻 Node 的 withAdvisoryLock（src/lib/migrate.ts:18-66）。
//
// 语义要点（照搬，不改成事务级锁）：
//   - 锁键是 `hashtext(<name>)`——同一个字符串在两端得到同一个 int4 键，因此 Go 与 Node
//     对同一个名字互斥。用 hashtext 而不是自己算 hash，是两端互斥成立的前提。
//   - 用 session 级 `pg_try_advisory_lock`：**必须独占一条连接**，锁随会话结束而释放。
//   - 锁连接**不在分道预算内**（见 store.OpenDedicatedConn 的注释）：从池里借会让第二个
//     等锁的实例阻塞在借连接上，而不是快速得到「锁被占了」的结论。
//
// 与最初实现的差别（**这是本轮性能修复的核心**）：专用连接**不再每次取锁新建**。
// 原实现每次 AcquireLeader 都 `OpenDedicatedConn`（新建单连接池）+ 取锁 + `pool.Close()`，
// 于是每次取锁都付一遍 TCP + SCRAM-SHA-256 认证（pbkdf2 4096 轮）。锁的取放与任务同频，
// 而「可用性投影消费」的生产节奏是 200ms 一次 ⇒ 每秒 5 条新连接、每天约 43 万条，
// 实测占到空闲期 CPU 的六成上下。
// 改成复用后，**如何保住原来的互斥语义**？原实现的互斥来自「每次取锁都是一条新会话」：
// 同进程内第二次取同一个锁，因为换了会话，`pg_try_advisory_lock` 会失败 ⇒ 得到「锁被占」。
// 复用同一会话后，PG 的同会话重入会让第二次取锁**成功**（advisory 锁按会话计数），
// 语义就变了。故这里补一把**进程内按锁名的 TryLock**：拿不到就是「本进程已持有该锁」，
// 与原来的「另一条会话拿不到」同判（返回 (nil, false, nil)）。跨进程互斥仍由 PG 那把锁负责。

// LeaderLock 是一次已加锁的会话。
type LeaderLock struct {
	name string
	// entry 持有该锁名的常驻会话与进程内互斥量（见 leaderEntry）。
	entry *leaderEntry
}

// leaderEntry 是某个锁名的进程内状态：一条**该锁名独占**的常驻专用连接 + 一把进程内互斥量。
//
// 为什么一条锁名一条连接（而不是全进程共用一条）：pgx 的底层连接只允许一个写入者，
// 两个 goroutine 并发在同一 conn 上发查询会 panic（实测踩过
// `BUG: slow write timer already active`）。锁名之间本就无关，分开正好避开这件事。
// 锁名数量是个位数量级，连接开销可忽略。
type leaderEntry struct {
	// mu 是进程内互斥：复刻「每次取锁用不同会话」时 PG 给出的互斥结果。
	mu sync.Mutex
	// session 懒建；取锁或释放失败时置 nil，下一轮重建。仅由持 mu 的一方读写。
	session *store.DedicatedSession
}

// leaderEntries 按 (连接池实例, 锁名) 保存 entry。
//
// 键里带连接池指针是刻意的：测试会针对不同库各建一套池，若只按锁名缓存，第二个库会拿到
// 第一个库的会话（锁就加错库了）。键类型可比较，故可直接做 map 键。
var leaderEntries sync.Map

type leaderEntryKey struct {
	pools *store.Pools
	name  string
}

// AcquireLeader 尝试以 leader 身份获取 advisory 锁。
//
// 返回 (nil, false, nil) 表示锁被别的实例（或本进程的另一个任务）持有——skipIfLocked 语义；
// 返回错误表示连接或语句层面失败，调用方应记日志并跳过本轮而不是 panic。
func AcquireLeader(ctx context.Context, pools *store.Pools, name string) (*LeaderLock, bool, error) {
	if pools == nil {
		return nil, false, fmt.Errorf("jobs: 未配置数据库连接池")
	}
	entry, err := leaderEntryFor(ctx, pools, name)
	if err != nil {
		return nil, false, err
	}
	// 进程内互斥先拿：拿不到就等价于「另一条会话已持有该锁」，直接给结论，不去碰网络。
	if !entry.mu.TryLock() {
		return nil, false, nil
	}

	// 常驻会话可能已被服务端断开（idle_session_timeout、网络中断、PG 重启）。留一次重试：
	// 首轮失败即丢会话（关闭连接同时释放它持有的锁），下一轮用新会话重试。
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		session, sessionErr := entry.sessionFor(ctx, pools)
		if sessionErr != nil {
			entry.mu.Unlock()
			return nil, false, sessionErr
		}
		conn := session.Conn()
		if conn == nil {
			entry.dropSession()
			lastErr = fmt.Errorf("jobs: 专用会话不可用")
			continue
		}
		var acquired bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, name).Scan(&acquired); err != nil {
			entry.dropSession()
			lastErr = fmt.Errorf("jobs: 申请 advisory 锁失败: %w", err)
			continue
		}
		if !acquired {
			// 别的进程持有：本轮的进程内互斥要还回去，否则本进程再也不来试。
			entry.mu.Unlock()
			return nil, false, nil
		}
		return &LeaderLock{name: name, entry: entry}, true, nil
	}
	entry.mu.Unlock()
	return nil, false, lastErr
}

// sessionFor 取本锁名的常驻会话；没有或已失效就建一条。
// 只在持 entry.mu 时调用。
func (e *leaderEntry) sessionFor(ctx context.Context, pools *store.Pools) (*store.DedicatedSession, error) {
	if e.session != nil && e.session.Valid() {
		return e.session, nil
	}
	session, err := pools.DedicatedSession(ctx)
	if err != nil {
		return nil, err
	}
	e.session = session
	return session, nil
}

// dropSession 丢掉本锁名的会话（连接坏了或锁状态未知时）。
// 只在持 entry.mu 时调用；关闭连接即等于释放该会话持有的 advisory 锁。
func (e *leaderEntry) dropSession() {
	if e.session != nil {
		e.session.Close()
		e.session = nil
	}
}

// Release 释放锁；重复调用是安全的。
//
// 与最初实现的差别：**不关连接**（那是常驻会话），只做 pg_advisory_unlock + 还回进程内互斥。
func (l *LeaderLock) Release(ctx context.Context) error {
	if l == nil || l.entry == nil {
		return nil
	}
	entry := l.entry
	l.entry = nil
	defer entry.mu.Unlock()

	session := entry.session
	if session == nil {
		// 会话已随连接池关闭：连接随之消失，PG 已释放该会话的锁，等价于释放成功。
		return nil
	}
	conn := session.Conn()
	if conn == nil {
		return nil
	}
	// 连接可能已因网络中断失效：unlock 失败不再上报为任务失败，但必须把会话丢掉——
	// 会话结束时 PG 会释放它持有的全部锁，故丢会话即等价于释放。
	if _, unlockErr := conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext($1))`, l.name); unlockErr != nil {
		entry.dropSession()
		return fmt.Errorf("jobs: 释放 advisory 锁失败: %w", unlockErr)
	}
	return nil
}

// Name 返回锁名，便于日志与测试断言。
func (l *LeaderLock) Name() string {
	if l == nil {
		return ""
	}
	return l.name
}

// leaderEntryFor 取（或懒建）该锁名的进程内状态。会话本身在首次取锁时才建
// （见 leaderEntry.sessionFor），这里不必碰网络。
func leaderEntryFor(_ context.Context, pools *store.Pools, name string) (*leaderEntry, error) {
	if pools == nil {
		return nil, fmt.Errorf("jobs: 未配置数据库连接池")
	}
	key := leaderEntryKey{pools: pools, name: name}
	if value, ok := leaderEntries.Load(key); ok {
		return value.(*leaderEntry), nil
	}
	actual, loaded := leaderEntries.LoadOrStore(key, &leaderEntry{})
	if loaded {
		return actual.(*leaderEntry), nil
	}
	return actual.(*leaderEntry), nil
}

// Release 的实现辅助：会话句柄。
func (l *LeaderLock) entrySession() (*store.DedicatedSession, error) {
	if l.entry == nil {
		return nil, fmt.Errorf("jobs: 锁已释放")
	}
	if l.entry.session == nil {
		return nil, fmt.Errorf("jobs: 专用会话不可用")
	}
	return l.entry.session, nil
}

// 锁名与 Node 对齐：回填用 Node 的锁名，保证并存期只有一侧真正执行回填。
const (
	// availBackfillLockName 与 projection-worker.ts:16 的 BACKFILL_LOCK 同名。
	availBackfillLockName = "claude-code-hub:availability-projection-backfill"
	// priceSyncLockName 是 Go 侧自有的锁名：Node 侧云价格同步没有 advisory 锁
	// （只有进程内的 AsyncTaskManager 去重），故两端并存时无法互斥，见 README「并存期」。
	priceSyncLockName = "claude-code-hub:cloud-price-table-sync"
)
