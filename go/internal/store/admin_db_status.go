package store

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 本文件是 /api/admin/database/status 需要的库侧元数据读取。
//
// Node 侧（src/app/api/admin/database/status/route.ts:36-72）通过 `docker-executor` 在
// **数据库容器内**跑 psql 取 size/tableCount/version；Go 没有那类容器内执行能力，故改从连接
// 本身查同一份事实（来源不同、语义相同）：
//   databaseSize    → pg_size_pretty(pg_database_size(current_database()))，与 Node 的人类可读串同形
//   tableCount      → public schema 下的基表数（Node 的 getDatabaseInfo 同为「表的数量」）
//   postgresVersion → server_version（Node 从 psql 输出取同一字段）

// DBStatusInfo 是 /api/admin/database/status 的库侧事实。
type DBStatusInfo struct {
	// DatabaseName 是当前连接指向的库名。
	DatabaseName string
	// SizePretty 是人类可读的库体积（如 "7921 MB"）。
	SizePretty string
	// TableCount 是 public schema 下的基表数量。
	TableCount int64
	// ServerVersion 是 PostgreSQL 服务端版本（如 "18.0"）。
	ServerVersion string
}

// ReadDBStatusInfo 查一次库侧元数据；连接不可用时返回错误。
func (p *Pools) ReadDBStatusInfo(ctx context.Context) (DBStatusInfo, error) {
	pool, err := p.Data()
	if err != nil {
		return DBStatusInfo{}, err
	}
	var info DBStatusInfo
	row := pool.QueryRow(ctx, `
		SELECT current_database(),
		       pg_size_pretty(pg_database_size(current_database())),
		       (SELECT count(*) FROM information_schema.tables
		         WHERE table_schema = 'public' AND table_type = 'BASE TABLE'),
		       current_setting('server_version')
	`)
	if err := row.Scan(&info.DatabaseName, &info.SizePretty, &info.TableCount, &info.ServerVersion); err != nil {
		return DBStatusInfo{}, err
	}
	return info, nil
}

// DSNEndpoint 返回 DSN 里的 host:port 与库名（Node 的 DatabaseStatus.containerName 取
// 同一形式 `<host>:<port>`）。DSN 解析失败时返回空串：调用方照常作答，只是这两项为空，
// 而不是让整条状态接口 500。
func (p *Pools) DSNEndpoint() (string, string) {
	cfg, err := pgxpool.ParseConfig(p.dsn)
	if err != nil || cfg.ConnConfig == nil {
		return "", ""
	}
	host := cfg.ConnConfig.Host
	if cfg.ConnConfig.Port != 0 {
		host += ":" + strconv.FormatUint(uint64(cfg.ConnConfig.Port), 10)
	}
	return host, cfg.ConnConfig.Database
}
