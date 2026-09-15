package store

import (
	"context"
	"errors"
	"fmt"
)

// AppendSpecialSettings 把审计条目**追加**到已建行的 message_request.special_settings。
//
// 与 UpdateDetailsIfUnfinalizedWith 的分工：那条路是终态写、带 `status_code IS NULL` 幂等谓词，
// 一行只赢一次；本函数是**终态之后**的补写，没有谓词、按行 id 定位。
//
// 为什么需要它：响应修复器的判定发生在正文已经开始交付的时刻（流式更是要等流结束才
// 知道修没修），那时终态已经落库。Node 在这一步走 updateMessageRequestDetails —— 同样是
// 终态之后的一次额外 UPDATE，语义对齐。
//
// 为什么要追加而不是覆盖：Node 手里握着整份 specialSettings 数组，故它写的是覆盖；
// Go 的请求侧条目（守卫链写的客户端审计）与响应侧条目分居两次写入，覆盖会把先写的那批
// 抹掉。存储层的追加语义（`COALESCE(col,'[]') || $n`）正是为这类「先写后补」准备的。
func (p *Pools) AppendSpecialSettings(ctx context.Context, id int64, entries []byte) error {
	if id <= 0 || len(entries) == 0 {
		return nil
	}
	pool, err := p.Writer()
	if err != nil {
		return err
	}
	return AppendSpecialSettingsWith(ctx, pool, id, entries)
}

// AppendSpecialSettingsWith 在指定分道上追加审计条目，便于调用方在事务内复用。
func AppendSpecialSettingsWith(ctx context.Context, pool *Pool, id int64, entries []byte) error {
	if pool == nil {
		return errors.New("store: 追加审计条目缺少连接分道")
	}
	const query = `UPDATE message_request SET "special_settings" = COALESCE("special_settings", '[]'::jsonb) || $2::jsonb,` +
		` "updated_at" = now() WHERE "id" = $1`
	if _, err := pool.Exec(ctx, query, id, entries); err != nil {
		return fmt.Errorf("store: 追加审计条目失败: %w", err)
	}
	return nil
}
