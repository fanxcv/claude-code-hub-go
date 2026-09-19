package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
)

// AdmissionErrorCode 复刻 src/drizzle/admitted-client.ts 的
// DB_POOL_ADMISSION_ERROR_CODE，调用方据此把「池准入被拒」与「上游故障」区分开。
const AdmissionErrorCode = "DB_POOL_ADMISSION_EXCEEDED"

// PoolLifecycleErrorCode 是池已关闭时的错误码，对应 TS 侧
// `Database pools are <state>` 的抛出点。
const PoolLifecycleErrorCode = "DB_POOLS_CLOSED"

// AdmissionError 复刻 DbPoolAdmissionError：准入计数达到上限时立即失败，不排队等待。
type AdmissionError struct {
	lane           config.Lane
	maxOutstanding int
}

func (e *AdmissionError) Error() string {
	// 与 TS 的错误类文案一致（Error 构造里的那句）。
	return fmt.Sprintf("Database pool %s exceeded %d outstanding operations", e.lane, e.maxOutstanding)
}

// Code 返回可供调用方判别的错误码。
func (e *AdmissionError) Code() string { return AdmissionErrorCode }

// Lane 返回触发准入拒绝的逻辑分道。
func (e *AdmissionError) Lane() config.Lane { return e.lane }

// MaxOutstanding 返回该分道的准入上限。
func (e *AdmissionError) MaxOutstanding() int { return e.maxOutstanding }

// SafeMessage 复刻 findDbPoolAdmissionError 产出的那句可对外文案，日志与错误体
// 都用它，避免把内部细节直接暴露给客户端。
func (e *AdmissionError) SafeMessage() string {
	return fmt.Sprintf(
		"Database pool admission exceeded (pool=%s, maxOutstanding=%d)",
		e.lane, e.maxOutstanding,
	)
}

// Pools 是分道的连接池集合。数据面用 data（或 control）分道，终态写入在异步写模式下
// 必须走 writer 分道——与 TS 侧 getDb() / getMessageWriterDb() 的划分一致。
type Pools struct {
	budget      config.PoolBudget
	appNameBase string
	dsn         string
	timeouts    config.DBTimeouts

	mu       sync.Mutex
	state    poolLifecycle
	lanes    map[config.Lane]*Pool
	closeErr error
}

type poolLifecycle int32

const (
	poolOpen poolLifecycle = iota
	poolClosing
	poolClosed
)

func (s poolLifecycle) String() string {
	switch s {
	case poolClosing:
		return "closing"
	case poolClosed:
		return "closed"
	default:
		return "open"
	}
}

// Options 是 Open 的入参。DSN 只在本进程内使用，绝不写进日志或错误信息。
type Options struct {
	DSN                 string
	Budget              config.PoolBudget
	Timeouts            config.DBTimeouts
	ApplicationNameBase string
}

// Open 建立所需的物理池。池按物理分道惰性创建：预算为 0 的逻辑分道复用另一条道
// （见 config.PoolBudget.PhysicalLane），因此物理连接数上限等于总预算，而不是各道之和。
func Open(ctx context.Context, opts Options) (*Pools, error) {
	if opts.DSN == "" {
		return nil, errors.New("store: DSN 未设置")
	}
	appBase := opts.ApplicationNameBase
	if appBase == "" {
		appBase = "claude-code-hub"
	}
	pools := &Pools{
		budget:      opts.Budget,
		appNameBase: appBase,
		dsn:         opts.DSN,
		timeouts:    opts.Timeouts,
		lanes:       make(map[config.Lane]*Pool),
	}
	return pools, nil
}

// Budget 返回分道预算，供调用方核算准入上限。
func (p *Pools) Budget() config.PoolBudget { return p.budget }

// Lane 返回逻辑分道对应的池；预算为 0 的分道按 PhysicalLane 复用，池在此刻惰性建立。
func (p *Pools) Lane(lane config.Lane) (*Pool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state != poolOpen {
		return nil, &LifecycleError{state: p.state}
	}

	physical := p.budget.PhysicalLane(lane)
	if existing, ok := p.lanes[physical]; ok {
		return existing, nil
	}

	created, err := p.createPool(p.physicalLaneContext(), physical)
	if err != nil {
		return nil, err
	}
	p.lanes[physical] = created
	return created, nil
}

// Data 返回数据分道（对应 TS 的 getDb() 在 data scope 下的取值）。
func (p *Pools) Data() (*Pool, error) { return p.Lane(config.LaneData) }

// Control 返回控制分道（对应 TS 的 getDb() 在非 data scope 下的取值）。
func (p *Pools) Control() (*Pool, error) { return p.Lane(config.LaneControl) }

// Writer 返回终态写分道（对应 TS 的 getMessageWriterDb()）。
func (p *Pools) Writer() (*Pool, error) { return p.Lane(config.LaneWriter) }

func (p *Pools) physicalLaneContext() context.Context { return context.Background() }

// millisParam 把超时时长写成 PostgreSQL 认的毫秒字符串。
// runtimeParams 返回可写的连接运行时参数表（首次调用时建表）。
//
// 抽出来是因为 jit 与 application_name 都要写，两处池构造各自维护一份 nil 判空会漂移。
func runtimeParams(cfg *pgxpool.Config) map[string]string {
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	return cfg.ConnConfig.RuntimeParams
}

// JIT 一律关闭。
//
// 实测依据（2026-09，生产库 PG 18、2 vCPU，表 usage_ledger 93.4 万行）：
//
//   - `/dashboard/overview` 那条 9 子查询聚合被规划器**高估 236 倍**（估 84202 行 / 实 357 行），
//     计划代价 201296 越过 jit_above_cost（默认 100000）⇒ 触发 JIT；
//   - 同一查询：JIT 开 **97~119 ms**，JIT 关 **7.6~9.7 ms**（约 12 倍）。其中 JIT 编译自身
//     占 78 ms（Emission 67 ms），而查询只处理 357 行——编译成本是纯亏。
//   - 对**不触发** JIT 的查询无影响：排行榜全表聚合（代价 61694，本就未触发）开关两态
//     实测 948/923 ms 对 1001/931 ms，差异在噪声内。
//
// 为什么不用「调高 jit_above_cost 保留重查询的 JIT」：该阈值比的是**规划器估算的代价**，
// 而本仓的估算法已被证伪（同一个查询偏 236 倍）——用一个不可靠的估算值当开关，本身不稳。
// 若将来出现真正 CPU 密集的重查询（例如单次处理千万行且表达式繁重），届时以实测
// CPU 占比为依据重新评估，而不是凭猜测把 JIT 打开。
const jitOff = "off"

// applyConnectionRuntimeParams 写入两处池共有的运行时参数：应用名与关 JIT。
// 集中在一处，是为了让「关 JIT」有单元可测的落脚点（钉子在 pool_config_test.go）。
func applyConnectionRuntimeParams(cfg *pgxpool.Config, appName string) {
	params := runtimeParams(cfg)
	params["application_name"] = appName
	params["jit"] = jitOff
}

func millisParam(d time.Duration) string {
	return fmt.Sprintf("%d", d.Milliseconds())
}

// OpenDedicatedConn 建一条**独立于分道预算**的连接，供长时占用的用途（advisory 锁）。
//
// 为什么不能从分道池里借：池的容量就是 DB_POOL_MAX / 分道预算，而 advisory 锁一旦持有就要
// 占着连接直到任务结束。从池里借会让第二个等锁的实例**阻塞在借连接上**（而不是快速拿到
// 「锁被占了」的结论），在预算为 1 的分道里直接死锁。Node 的 withAdvisoryLock 就是另开一个
// `postgres(..., { max: 1 })` 客户端，这里保持同一形态。
//
// 调用方必须在结束（无论成败）后 Close；连接断开时 PG 会释放该会话的全部 advisory 锁。
// 返回连接与它所属的单连接池：归还连接后还需关池，故两者一并交给调用方。
func (p *Pools) OpenDedicatedConn(ctx context.Context) (*pgxpool.Conn, *pgxpool.Pool, error) {
	return p.openDedicatedConn(ctx, true)
}

// openDedicatedConn 是两种专用连接的共同构造。recycling 为假时关掉池的回收参数，
// 供 DedicatedSession 的常驻会话使用（理由见 DedicatedSession 的注释）。
func (p *Pools) openDedicatedConn(ctx context.Context, recycling bool) (*pgxpool.Conn, *pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(p.dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("store: 解析 DSN 失败: %w", err)
	}
	// 单连接池：既拿到 pgxpool.Conn 的释放语义，又不进入任何分道的准入计数。
	cfg.MaxConns = 1
	cfg.MinConns = 1
	if !recycling {
		// 常驻会话不能被池自行回收：回收即关连接，而 PG 会在会话结束时释放该会话持有的
		// 全部 advisory 锁——那会让「锁随会话持续有效」的假设静默失效。
		cfg.MaxConnLifetime = 0
		cfg.MaxConnIdleTime = 0
	}
	if p.timeouts.ConnectTimeout > 0 {
		cfg.ConnConfig.ConnectTimeout = p.timeouts.ConnectTimeout
	}
	applyConnectionRuntimeParams(cfg, p.appNameBase+"-dedicated")

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("store: 建立独立连接失败: %w", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("store: 取得独立连接失败: %w", err)
	}
	return conn, pool, nil
}

// DedicatedSession 建一条**常驻**的独立连接（单连接池）。调用方拥有它，用完自己 Close。
//
// 与 OpenDedicatedConn 的分工：那个每次新建并关闭，一次 TCP + SCRAM 认证；本方法返回的会话
// 可以长期留住复用。差别在生产上不是省一点，而是常驻 CPU：专用连接目前只用于 advisory 锁，
// 而锁的取放与任务同频——生产实测「可用性投影消费」每 200ms 取一次 ⇒ 每秒 5 条新连接、
// 每条都要做一遗 SCRAM-SHA-256（pbkdf2 4096 轮），实测占到空闲期 CPU 的六成上下。
//
// **会话一律不共享**（所以这里是工厂而不是缓存）：一条 pgx 连接只允许一个写入者，
// 两个 goroutine 并发在同一个 conn 上发查询会让 pgx 内部 panic
// （实测踩过 `BUG: slow write timer already active`）。要几条就建几条。
//
// 池的回收参数**故意关掉**（MaxConnLifetime/MaxConnIdleTime = 0）：会话被回收时 PG 会连同它
// 持有的 advisory 锁一起释放，而调用方是按「同一会话持续可用」来设计的。真被服务端（如
// idle_session_timeout）断开时，下一次取锁会失败一次，调用方 Invalidate 后重建。
func (p *Pools) DedicatedSession(ctx context.Context) (*DedicatedSession, error) {
	p.mu.Lock()
	state := p.state
	p.mu.Unlock()
	if state != poolOpen {
		return nil, &LifecycleError{state: state}
	}
	conn, pool, err := p.openDedicatedConn(ctx, false)
	if err != nil {
		return nil, err
	}
	return &DedicatedSession{pool: pool, conn: conn}, nil
}

// DedicatedSession 是一条常驻的专用连接（不与他人共享）。
type DedicatedSession struct {
	mu     sync.Mutex
	pool   *pgxpool.Pool
	conn   *pgxpool.Conn
	closed bool
}

// Conn 返回底层连接；调用方不得归还或关闭它。
func (s *DedicatedSession) Conn() *pgxpool.Conn {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	return s.conn
}

// Valid 报告会话是否仍持有可用连接。
func (s *DedicatedSession) Valid() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && s.conn != nil
}

// Invalidate 丢弃当前会话（连接可能已被服务端断开或处于未知状态）。
// 关闭连接即等于释放该会话持有的全部 advisory 锁（PG 的会话结束语义）。
func (s *DedicatedSession) Invalidate() { s.Close() }

// Close 释放会话与它所属的单连接池；重复调用安全。
func (s *DedicatedSession) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.conn != nil {
		s.conn.Release()
		s.conn = nil
	}
	if s.pool != nil {
		s.pool.Close()
		s.pool = nil
	}
}

// closeLockConn 归还连接并关掉它所属的单连接池。
func closeLockConn(conn *pgxpool.Conn, pool *pgxpool.Pool) {
	if conn != nil {
		conn.Release()
	}
	if pool != nil {
		pool.Close()
	}
}

// createPool 建一个分道池。
func (p *Pools) createPool(ctx context.Context, lane config.Lane) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(p.dsn)
	if err != nil {
		// 不回显 DSN 本身，只说明解析失败。
		return nil, fmt.Errorf("store: 解析 DSN 失败: %w", err)
	}

	maxConns := p.budget.Size(lane)
	if maxConns < 1 {
		// 预算为 0 的分道不该走到这里（PhysicalLane 已复用他道）；保底 1 以免建出无效池。
		maxConns = 1
	}
	cfg.MaxConns = int32(maxConns)
	if p.timeouts.IdleTimeout > 0 {
		cfg.MaxConnIdleTime = p.timeouts.IdleTimeout
	}
	if p.timeouts.ConnectTimeout > 0 {
		cfg.ConnConfig.ConnectTimeout = p.timeouts.ConnectTimeout
	}
	applyConnectionRuntimeParams(cfg, config.LaneApplicationName(lane))
	if p.timeouts.StatementExpiry > 0 {
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = millisParam(p.timeouts.StatementExpiry)
	}
	if p.timeouts.LockExpiry > 0 {
		cfg.ConnConfig.RuntimeParams["lock_timeout"] = millisParam(p.timeouts.LockExpiry)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: 建立 %s 分道连接池失败: %w", lane, err)
	}

	return newPool(lane, pool, p.budget.MaxOutstanding(lane)), nil
}

// Close 关闭全部分道；重复调用幂等。
func (p *Pools) Close() error {
	p.mu.Lock()
	if p.state != poolOpen {
		err := p.closeErr
		p.mu.Unlock()
		return err
	}
	p.state = poolClosing
	lanes := make([]*Pool, 0, len(p.lanes))
	for _, lane := range p.lanes {
		lanes = append(lanes, lane)
	}
	p.mu.Unlock()

	// 专用会话不在这里关：它由持有者（jobs 的锁注册表）拥有，且一条会话一个持有者。

	for _, lane := range lanes {
		lane.raw.Close()
	}

	p.mu.Lock()
	p.state = poolClosed
	p.mu.Unlock()
	return nil
}

// LifecycleError 对应 TS 侧 `Database pools are <state>` 的抛出：池关闭后任何使用都失败，
// 而不是静默重连。
type LifecycleError struct{ state poolLifecycle }

func (e *LifecycleError) Error() string {
	return fmt.Sprintf("Database pools are %s", e.state)
}

// Code 返回可供调用方判别的错误码。
func (e *LifecycleError) Code() string { return PoolLifecycleErrorCode }

// Pool 是单条物理分道：原始 pgx 池 + 准入计数。
// 准入语义复刻 admitted-client：计数的是瞬时在途操作数，达到上限即立刻返回
// AdmissionError，不排队；这与「每请求查询数」无关。
type Pool struct {
	lane           config.Lane
	raw            *pgxpool.Pool
	maxOutstanding int
	outstanding    atomic.Int64
}

func newPool(lane config.Lane, raw *pgxpool.Pool, maxOutstanding int) *Pool {
	return &Pool{lane: lane, raw: raw, maxOutstanding: maxOutstanding}
}

// Lane 返回该池的物理分道名，便于日志与错误归因。
func (p *Pool) Lane() config.Lane { return p.lane }

// MaxOutstanding 返回准入上限。
func (p *Pool) MaxOutstanding() int { return p.maxOutstanding }

// Outstanding 返回当前在途操作数，供监控与测试断言。
func (p *Pool) Outstanding() int64 { return p.outstanding.Load() }

// Raw 暴露底层池，仅供确实需要 pgx 原生能力（如 CopyFrom）的调用点使用；
// 使用它不经过准入计数，调用方须自行核算。
func (p *Pool) Raw() *pgxpool.Pool { return p.raw }

func (p *Pool) acquire() (func(), error) {
	// CAS 循环而不是「先读后加」：两次操作之间会被并发调用者插进来，
	// 结果是多个请求同时越过上限，准入形同虚设（瞬时在途数可以超过 maxOutstanding）。
	for {
		current := p.outstanding.Load()
		if int(current) >= p.maxOutstanding {
			return nil, &AdmissionError{lane: p.lane, maxOutstanding: p.maxOutstanding}
		}
		if p.outstanding.CompareAndSwap(current, current+1) {
			break
		}
	}
	var once sync.Once
	return func() { once.Do(func() { p.outstanding.Add(-1) }) }, nil
}

// Exec 在准入计数下执行语句；达到上限时返回 *AdmissionError。
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	release, err := p.acquire()
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer release()
	return p.raw.Exec(ctx, sql, args...)
}

// Query 在准入计数下查询。准入计数随返回的 Rows 关闭而释放（与 TS 侧把释放挂在
// 结果消费完成为止的语义一致）。
func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	release, err := p.acquire()
	if err != nil {
		return nil, err
	}
	rows, err := p.raw.Query(ctx, sql, args...)
	if err != nil {
		release()
		return nil, err
	}
	return &admittedRows{Rows: rows, release: release}, nil
}

// QueryRow 在准入计数下查询单行。释放时机是 Row 被 Scan 或查询失败，
// 因此调用方必须 Scan（不要取得 Row 后丢弃）。
func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	release, err := p.acquire()
	if err != nil {
		return &errorRow{err: err}
	}
	return &admittedRow{Row: p.raw.QueryRow(ctx, sql, args...), release: release}
}

// Begin 在准入计数下开启事务；释放时机是事务 Commit 或 Rollback。
func (p *Pool) Begin(ctx context.Context) (pgx.Tx, error) {
	release, err := p.acquire()
	if err != nil {
		return nil, err
	}
	tx, err := p.raw.Begin(ctx)
	if err != nil {
		release()
		return nil, err
	}
	return &admittedTx{Tx: tx, release: release}, nil
}

// Ping 探活，不做准入（就绪检查不应被负载挤掉）。
func (p *Pool) Ping(ctx context.Context) error { return p.raw.Ping(ctx) }

type admittedRows struct {
	pgx.Rows
	release func()
}

func (r *admittedRows) Close() {
	r.Rows.Close()
	r.release()
}

type admittedRow struct {
	pgx.Row
	release func()
}

func (r *admittedRow) Scan(dest ...any) error {
	defer r.release()
	return r.Row.Scan(dest...)
}

type admittedTx struct {
	pgx.Tx
	release func()
}

func (t *admittedTx) Commit(ctx context.Context) error {
	defer t.release()
	return t.Tx.Commit(ctx)
}

func (t *admittedTx) Rollback(ctx context.Context) error {
	defer t.release()
	return t.Tx.Rollback(ctx)
}

type errorRow struct{ err error }

func (r *errorRow) Scan(...any) error { return r.err }

// IsAdmissionError 便于调用方在不 import errors 的情况下判别准入拒绝。
func IsAdmissionError(err error) bool {
	var admission *AdmissionError
	return errors.As(err, &admission)
}
