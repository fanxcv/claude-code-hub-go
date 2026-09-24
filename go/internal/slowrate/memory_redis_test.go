package slowrate

import (
	"context"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件提供慢速判据的内存 Redis 替身与「按环境选真库/替身」的入口。
//
// 为什么要它：W1（进入隔离事件）与 W4（冷却写读身份一致）两条判据原先挂在 CCH_TEST_REDIS_URL
// 的整组跳过上，CI 默认跑法不设该变量——把 RecordQuarantineEntered 注释掉、或把 writeCooldown
// 的 KeyID 校验放宽，测试都不会转红，判据在 CI 里不设防。
//
// 两条判据的断言全落在**命令的效果**上（事件流里多不多一条、冷却键在不在），故替身必须真做
// 键语义——尤其同一次 pipeline 内 ZAdd 之后 ZCard 要能看见——而不是「数命令」：数命令的替身
// 对上述两处改动照样会绿。未实现的方法靠内嵌 nil 接口炸出来（同 internal/route 的 failOpenRedis
// 范式）：多用一个命令即 panic，逼作者显式补齐语义。

// recoveryTestRedis 选本用例该用的 Redis：设了 CCH_TEST_REDIS_URL 就交给真库（真路径不得被
// 替身短路），未设则退回内存替身——两条判据必须在 CI 默认跑法下真的能拦。
func recoveryTestRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	if os.Getenv(testRedisEnv) != "" {
		return recoveryRedis(t)
	}
	client := newMemoryRedis()
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// memoryRedis 是只实现本包这几条判据所需命令的内存替身：字符串、哈希、ZSET、事件流四种键型。
type memoryRedis struct {
	redis.UniversalClient

	mu      sync.Mutex
	strings map[string]string
	hashes  map[string]map[string]string
	zsets   map[string]map[string]float64
	streams map[string][]string
}

func newMemoryRedis() *memoryRedis {
	return &memoryRedis{
		strings: map[string]string{},
		hashes:  map[string]map[string]string{},
		zsets:   map[string]map[string]float64{},
		streams: map[string][]string{},
	}
}

func (m *memoryRedis) Close() error { return nil }

func (m *memoryRedis) Pipeline() redis.Pipeliner { return &memoryPipeline{client: m} }

func (m *memoryRedis) Pipelined(ctx context.Context, fn func(redis.Pipeliner) error) ([]redis.Cmder, error) {
	pipe := &memoryPipeline{client: m}
	if err := fn(pipe); err != nil {
		return pipe.cmds, err
	}
	return pipe.Exec(ctx)
}

func (m *memoryRedis) Set(_ context.Context, key string, value any, _ time.Duration) *redis.StatusCmd {
	cmd := redis.NewStatusCmd(context.Background())
	m.mu.Lock()
	m.strings[key] = fmt.Sprint(value)
	m.mu.Unlock()
	cmd.SetVal("OK")
	return cmd
}

func (m *memoryRedis) Get(_ context.Context, key string) *redis.StringCmd {
	cmd := redis.NewStringCmd(context.Background())
	m.mu.Lock()
	value, ok := m.strings[key]
	m.mu.Unlock()
	if !ok {
		cmd.SetErr(redis.Nil)
		return cmd
	}
	cmd.SetVal(value)
	return cmd
}

func (m *memoryRedis) Del(_ context.Context, keys ...string) *redis.IntCmd {
	cmd := redis.NewIntCmd(context.Background())
	m.mu.Lock()
	cmd.SetVal(m.delLocked(keys...))
	m.mu.Unlock()
	return cmd
}

func (m *memoryRedis) Exists(_ context.Context, keys ...string) *redis.IntCmd {
	cmd := redis.NewIntCmd(context.Background())
	m.mu.Lock()
	found := int64(0)
	for _, key := range keys {
		if m.existsLocked(key) {
			found++
		}
	}
	m.mu.Unlock()
	cmd.SetVal(found)
	return cmd
}

// Keys 按 glob 匹配键名：清理面要删的冷却键含会话身份（hash tag 由 sessionID×keyID 派生），
// 无法逐个枚举，只能按渠道前缀扫。
func (m *memoryRedis) Keys(_ context.Context, pattern string) *redis.StringSliceCmd {
	cmd := redis.NewStringSliceCmd(context.Background())
	m.mu.Lock()
	all := make([]string, 0, len(m.strings)+len(m.hashes)+len(m.zsets)+len(m.streams))
	for key := range m.strings {
		all = append(all, key)
	}
	for key := range m.hashes {
		all = append(all, key)
	}
	for key := range m.zsets {
		all = append(all, key)
	}
	for key := range m.streams {
		all = append(all, key)
	}
	m.mu.Unlock()
	matched := make([]string, 0, len(all))
	for _, key := range all {
		if ok, err := path.Match(pattern, key); err == nil && ok {
			matched = append(matched, key)
		}
	}
	sort.Strings(matched)
	cmd.SetVal(matched)
	return cmd
}

// XRevRangeN 倒序取最近 count 条（与真 Redis 的语义一致）。
func (m *memoryRedis) XRevRangeN(_ context.Context, stream, _, _ string, count int64) *redis.XMessageSliceCmd {
	cmd := redis.NewXMessageSliceCmd(context.Background())
	m.mu.Lock()
	stored := append([]string(nil), m.streams[stream]...)
	m.mu.Unlock()
	messages := make([]redis.XMessage, 0, len(stored))
	for index := len(stored) - 1; index >= 0 && int64(len(messages)) < count; index-- {
		messages = append(messages, redis.XMessage{
			ID:     strconv.Itoa(index) + "-0",
			Values: map[string]any{"event": stored[index]},
		})
	}
	cmd.SetVal(messages)
	return cmd
}

func (m *memoryRedis) delLocked(keys ...string) int64 {
	removed := int64(0)
	for _, key := range keys {
		hit := false
		if _, ok := m.strings[key]; ok {
			delete(m.strings, key)
			hit = true
		}
		if _, ok := m.hashes[key]; ok {
			delete(m.hashes, key)
			hit = true
		}
		if _, ok := m.zsets[key]; ok {
			delete(m.zsets, key)
			hit = true
		}
		if _, ok := m.streams[key]; ok {
			delete(m.streams, key)
			hit = true
		}
		if hit {
			removed++
		}
	}
	return removed
}

func (m *memoryRedis) existsLocked(key string) bool {
	if _, ok := m.strings[key]; ok {
		return true
	}
	if _, ok := m.hashes[key]; ok {
		return true
	}
	if _, ok := m.zsets[key]; ok {
		return true
	}
	if _, ok := m.streams[key]; ok {
		return true
	}
	return false
}

// memoryPipeline 把命令按序落到替身上：命令**不在排队时执行**，而在 Exec 时按序执行——
// 这是与真 pipeline 一致的关键语义（同批内 ZAdd 之后 ZCard 必须看得见新成员，否则达阈值
// 计数会差一条，两条判据都会假红）。因此每个方法只登记一个延迟执行的 op。
type memoryPipeline struct {
	redis.Pipeliner
	client *memoryRedis
	cmds   []redis.Cmder
	ops    []func()
}

func (p *memoryPipeline) Exec(_ context.Context) ([]redis.Cmder, error) {
	p.client.mu.Lock()
	defer p.client.mu.Unlock()
	for _, op := range p.ops {
		op()
	}
	return p.cmds, nil
}

func (p *memoryPipeline) ZAdd(_ context.Context, key string, members ...redis.Z) *redis.IntCmd {
	cmd := redis.NewIntCmd(context.Background())
	p.ops = append(p.ops, func() {
		set := p.client.zsets[key]
		if set == nil {
			set = map[string]float64{}
			p.client.zsets[key] = set
		}
		added := int64(0)
		for _, member := range members {
			name := fmt.Sprint(member.Member)
			if _, ok := set[name]; !ok {
				added++
			}
			set[name] = member.Score
		}
		cmd.SetVal(added)
	})
	p.cmds = append(p.cmds, cmd)
	return cmd
}

func (p *memoryPipeline) ZRemRangeByScore(_ context.Context, key, min, max string) *redis.IntCmd {
	cmd := redis.NewIntCmd(context.Background())
	p.ops = append(p.ops, func() {
		set := p.client.zsets[key]
		lower, lowerErr := strconv.ParseFloat(min, 64)
		upper, upperErr := strconv.ParseFloat(max, 64)
		removed := int64(0)
		if lowerErr == nil && upperErr == nil {
			for name, score := range set {
				if score >= lower && score <= upper {
					delete(set, name)
					removed++
				}
			}
		}
		cmd.SetVal(removed)
	})
	p.cmds = append(p.cmds, cmd)
	return cmd
}

func (p *memoryPipeline) Expire(_ context.Context, _ string, _ time.Duration) *redis.BoolCmd {
	cmd := redis.NewBoolCmd(context.Background())
	cmd.SetVal(true)
	p.cmds = append(p.cmds, cmd)
	return cmd
}

func (p *memoryPipeline) ZCard(_ context.Context, key string) *redis.IntCmd {
	cmd := redis.NewIntCmd(context.Background())
	p.ops = append(p.ops, func() { cmd.SetVal(int64(len(p.client.zsets[key]))) })
	p.cmds = append(p.cmds, cmd)
	return cmd
}

func (p *memoryPipeline) Del(_ context.Context, keys ...string) *redis.IntCmd {
	cmd := redis.NewIntCmd(context.Background())
	p.ops = append(p.ops, func() { cmd.SetVal(p.client.delLocked(keys...)) })
	p.cmds = append(p.cmds, cmd)
	return cmd
}

func (p *memoryPipeline) HGet(_ context.Context, key, field string) *redis.StringCmd {
	cmd := redis.NewStringCmd(context.Background())
	p.ops = append(p.ops, func() {
		value, ok := p.client.hashes[key][field]
		if !ok {
			cmd.SetErr(redis.Nil)
			return
		}
		cmd.SetVal(value)
	})
	p.cmds = append(p.cmds, cmd)
	return cmd
}

func (p *memoryPipeline) HSet(_ context.Context, key string, values ...any) *redis.IntCmd {
	cmd := redis.NewIntCmd(context.Background())
	p.ops = append(p.ops, func() {
		hash := p.client.hashes[key]
		if hash == nil {
			hash = map[string]string{}
			p.client.hashes[key] = hash
		}
		added := int64(0)
		for index := 0; index+1 < len(values); index += 2 {
			field := fmt.Sprint(values[index])
			if _, ok := hash[field]; !ok {
				added++
			}
			hash[field] = fmt.Sprint(values[index+1])
		}
		cmd.SetVal(added)
	})
	p.cmds = append(p.cmds, cmd)
	return cmd
}

func (p *memoryPipeline) XAdd(_ context.Context, args *redis.XAddArgs) *redis.StringCmd {
	cmd := redis.NewStringCmd(context.Background())
	p.ops = append(p.ops, func() {
		values, _ := args.Values.(map[string]any)
		payload, _ := values["event"].(string)
		entries := append(p.client.streams[args.Stream], payload)
		// 精确裁剪（MAXLEN =N）：只留最近 N 条，与 slowlog 的写法一致。
		if args.MaxLen > 0 && int64(len(entries)) > args.MaxLen {
			entries = entries[int64(len(entries))-args.MaxLen:]
		}
		p.client.streams[args.Stream] = entries
		cmd.SetVal(strconv.Itoa(len(entries)) + "-0")
	})
	p.cmds = append(p.cmds, cmd)
	return cmd
}
