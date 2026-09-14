package terminal

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/redis/go-redis/v9"
)

// 本文件钉住亲和写回与终态提交的**顺序**：成功写回必须发生在终态提交之后，
// 失败墓碑不依赖提交。只断言「最终键存在」会漏掉顺序错误（写回先跑、提交失败的组合）。
// 依据：Node response-handler.ts:5414-5420 把 recordAffinityWinner 放进
// postTerminalSideEffects（终态与计费之后执行），墓碑则在失败判定处立即发放。

// orderLog 记录副作用的实际执行顺序，供断言顺序而非仅断言结果。
type orderLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *orderLog) append(entry string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry)
}

func (l *orderLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.entries...)
}

// commitRecordingWriter 在终态提交成功那一刻记一条 "settle_commit"。
type commitRecordingWriter struct {
	*fakeWriter
	log *orderLog
}

func (w commitRecordingWriter) UpdateDetailsIfUnfinalized(
	ctx context.Context,
	id int64,
	patch store.DetailsPatch,
) (bool, error) {
	committed, err := w.fakeWriter.UpdateDetailsIfUnfinalized(ctx, id, patch)
	if committed {
		w.log.append("settle_commit")
	}
	return committed, err
}

// recordingWriteback 记顺序并**真的**走 Redis 写回（真实键与 generation CAS 一并验证）。
type recordingWriteback struct {
	store      *route.AffinityStore
	log        *orderLog
	scopeTag   string
	tipFP      string
	identityFP string
	generation string
}

func (w recordingWriteback) RecordWinner(ctx context.Context, providerID int64) bool {
	w.log.append("affinity_put")
	return w.store.Put(ctx, w.scopeTag, w.tipFP, providerID, w.identityFP, w.generation)
}

func (w recordingWriteback) TombstoneOnFailure(ctx context.Context, failedProviderID int64) bool {
	w.log.append("affinity_tombstone")
	return w.store.Tombstone(ctx, w.scopeTag, w.tipFP, "failover", w.identityFP, w.generation)
}

// terminalIntegrationRedis 读门控变量建 Redis 客户端；未设置时跳过（库号固定 13）。
func terminalIntegrationRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败: %v", err)
	}
	if options.DB == 0 {
		options.DB = 13
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis 不可达: %v", err)
	}
	return client
}

// affinityTestScope 造本次运行独有的 scope 并登记清理。
func affinityTestScope(t *testing.T, client redis.UniversalClient) string {
	t.Helper()
	scope := "term" + time.Now().Format("150405.000000000")
	t.Cleanup(func() {
		ctx := context.Background()
		var cursor uint64
		for {
			keys, next, err := client.Scan(ctx, cursor, "cch:pfx:{"+scope+":*", 64).Result()
			if err != nil {
				return
			}
			if len(keys) > 0 {
				_ = client.Del(ctx, keys...).Err()
			}
			if next == 0 {
				return
			}
			cursor = next
		}
	})
	return scope
}

// newAffinityTestContext 造一个带行标识与真实写回能力的上下文。
func newAffinityTestContext(t *testing.T, writeback pctx.AffinityWriteback) *pctx.Context {
	t.Helper()
	pc := newTestContext(t)
	if err := pc.SetMessageRequestID(77); err != nil {
		t.Fatalf("写入行标识失败: %v", err)
	}
	if writeback != nil {
		pc.SetAffinityWriteback(writeback)
	}
	return pc
}

// TestIntegrationAffinityWritebackFollowsTerminalCommit 是顺序钉子：
// 终态提交成功时顺序必须是 [settle_commit, affinity_put]，且键确实落到 Redis。
func TestIntegrationAffinityWritebackFollowsTerminalCommit(t *testing.T) {
	client := terminalIntegrationRedis(t)
	ctx := context.Background()
	scope := affinityTestScope(t, client)
	store := route.NewAffinityStore(route.AffinityOptions{
		Redis:             client,
		Window:            8,
		SlidingTTLSeconds: 120,
	})
	tip := "tip-" + scope
	if err := client.Set(ctx, "cch:pfx:{"+scope+"}:gen:"+tip, "v3:term", time.Hour).Err(); err != nil {
		t.Fatalf("预置 generation 失败: %v", err)
	}

	log := &orderLog{}
	writer := commitRecordingWriter{fakeWriter: &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}, log: log}
	pc := newAffinityTestContext(t, recordingWriteback{
		store: store, log: log, scopeTag: scope, tipFP: tip, identityFP: tip, generation: "v3:term",
	})

	result, err := New(writer, Options{}).SettleContext(ctx, pc, Settlement{
		StatusCode: 200,
		Affinity:   AffinityDirective{WinnerProviderID: 7},
	}, nil)
	if err != nil || !result.Committed {
		t.Fatalf("终态结算应提交: result=%+v err=%v", result, err)
	}

	order := log.snapshot()
	if len(order) != 2 || order[0] != "settle_commit" || order[1] != "affinity_put" {
		t.Fatalf("副作用顺序不符: %v（期望 [settle_commit affinity_put]）", order)
	}
	value, err := client.Get(ctx, "cch:pfx:{"+scope+"}:fp:"+tip).Result()
	if err != nil {
		t.Fatalf("写回键不存在: %v", err)
	}
	if value != "1|7|"+tip+"|v3:term" {
		t.Errorf("写回值 = %q，期望 1|7|%s|v3:term", value, tip)
	}
}

// TestIntegrationAffinityWinnerSkippedWhenTerminalNotCommitted 钉住反向：终态没赢下提交时，
// 成功写回不得执行（否则会把粘性指向一个本次并未服务成功的供应商）。
func TestIntegrationAffinityWinnerSkippedWhenTerminalNotCommitted(t *testing.T) {
	client := terminalIntegrationRedis(t)
	ctx := context.Background()
	scope := affinityTestScope(t, client)
	store := route.NewAffinityStore(route.AffinityOptions{
		Redis:             client,
		Window:            8,
		SlidingTTLSeconds: 120,
	})
	tip := "tip-" + scope

	log := &orderLog{}
	writer := commitRecordingWriter{fakeWriter: &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: false}}}, log: log}
	pc := newAffinityTestContext(t, recordingWriteback{
		store: store, log: log, scopeTag: scope, tipFP: tip, identityFP: tip, generation: "v3:term",
	})

	result, err := New(writer, Options{}).SettleContext(ctx, pc, Settlement{
		StatusCode: 200,
		Affinity:   AffinityDirective{WinnerProviderID: 7},
	}, nil)
	if !errors.Is(err, ErrNotSettled) {
		t.Fatalf("未赢得终态应返回 ErrNotSettled: result=%+v err=%v", result, err)
	}
	if order := log.snapshot(); len(order) != 0 {
		t.Fatalf("未提交时不得写回亲和: %v", order)
	}
	if client.Exists(ctx, "cch:pfx:{"+scope+"}:fp:"+tip).Val() != 0 {
		t.Error("未提交时不得写出亲和键")
	}
}

// TestIntegrationAffinityTombstoneIgnoresCommit 钉住失败墓碑与提交解耦：
// Node 在失败判定处立即 fire-and-forget（response-handler.ts:5425），与计费无关。
func TestIntegrationAffinityTombstoneIgnoresCommit(t *testing.T) {
	client := terminalIntegrationRedis(t)
	ctx := context.Background()
	scope := affinityTestScope(t, client)
	store := route.NewAffinityStore(route.AffinityOptions{
		Redis:             client,
		Window:            8,
		SlidingTTLSeconds: 120,
	})
	tip := "tip-" + scope
	if err := client.Set(ctx, "cch:pfx:{"+scope+"}:gen:"+tip, "v3:term", time.Hour).Err(); err != nil {
		t.Fatalf("预置 generation 失败: %v", err)
	}

	log := &orderLog{}
	writer := commitRecordingWriter{fakeWriter: &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: false}}}, log: log}
	pc := newAffinityTestContext(t, recordingWriteback{
		store: store, log: log, scopeTag: scope, tipFP: tip, identityFP: tip, generation: "v3:term",
	})

	if _, err := New(writer, Options{}).SettleContext(ctx, pc, Settlement{
		StatusCode: 502,
		Affinity:   AffinityDirective{TombstoneProviderID: 7},
	}, nil); err == nil {
		t.Fatalf("未赢得终态应返回错误")
	}

	order := log.snapshot()
	if len(order) != 1 || order[0] != "affinity_tombstone" {
		t.Fatalf("墓碑应独立于提交执行: %v", order)
	}
	value, err := client.Get(ctx, "cch:pfx:{"+scope+"}:fp:"+tip).Result()
	if err != nil {
		t.Fatalf("墓碑键不存在: %v", err)
	}
	if !strings.HasPrefix(value, "0|failover|") {
		t.Errorf("墓碑值 = %q，期望以 0|failover| 开头", value)
	}
}
