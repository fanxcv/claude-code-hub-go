package pubstatus

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// fakeProjectionRedis 是投影写侧的测试替身：同时实现 ProjectionRedis（worker 用）与
// RollupWriter（事件写入用），因为两者读写同一批键——分两个替身会让「同一个键两处语义不同」
// 这类缺陷测不出来。
//
// dump 的输出形制与 golden 一致：哈希字段拉平成 `<key>\u0000<field>`，TTL 以**秒**记
// （Node 的 `expire` 单位就是秒）。
type fakeProjectionRedis struct {
	raw  map[string]string
	hash map[string]map[string]string
	ttls map[string]float64
}

func newFakeProjectionRedis() *fakeProjectionRedis {
	return &fakeProjectionRedis{
		raw:  map[string]string{},
		hash: map[string]map[string]string{},
		ttls: map[string]float64{},
	}
}

func (f *fakeProjectionRedis) seedRaw(key, value string) { f.raw[key] = value }

// seedHash 接受 golden 的 `<key>\u0000<field>` 形制。
func (f *fakeProjectionRedis) seedHash(key, value string) {
	parts := strings.SplitN(key, "\u0000", 2)
	if len(parts) != 2 {
		f.raw[key] = value
		return
	}
	fields := f.hash[parts[0]]
	if fields == nil {
		fields = map[string]string{}
		f.hash[parts[0]] = fields
	}
	fields[parts[1]] = value
}

func (f *fakeProjectionRedis) seedTTL(key string, seconds float64) { f.ttls[key] = seconds }

func (f *fakeProjectionRedis) Ready(context.Context) bool { return true }

func (f *fakeProjectionRedis) Get(_ context.Context, key string) (string, bool) {
	value, ok := f.raw[key]
	return value, ok
}

func (f *fakeProjectionRedis) PTTL(_ context.Context, key string) (time.Duration, error) {
	seconds, ok := f.ttls[key]
	if !ok {
		return -2, nil
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func (f *fakeProjectionRedis) SetEX(_ context.Context, key, value string, ttl time.Duration) error {
	f.raw[key] = value
	f.ttls[key] = ttl.Seconds()
	return nil
}

func (f *fakeProjectionRedis) SetPX(_ context.Context, key, value string, ttl time.Duration) error {
	f.raw[key] = value
	f.ttls[key] = ttl.Seconds()
	return nil
}

func (f *fakeProjectionRedis) Set(_ context.Context, key, value string) error {
	f.raw[key] = value
	return nil
}

func (f *fakeProjectionRedis) Del(_ context.Context, keys ...string) error {
	for _, key := range keys {
		delete(f.raw, key)
		delete(f.hash, key)
		delete(f.ttls, key)
	}
	return nil
}

func (f *fakeProjectionRedis) AcquireLock(_ context.Context, key, value string, ttlMs int) (bool, error) {
	if _, exists := f.raw[key]; exists {
		return false, nil
	}
	if _, exists := f.hash[key]; exists {
		return false, nil
	}
	f.raw[key] = value
	f.ttls[key] = float64(ttlMs) / 1000
	return true, nil
}

func (f *fakeProjectionRedis) ReleaseLock(_ context.Context, key, value string) error {
	if f.raw[key] == value {
		delete(f.raw, key)
		delete(f.ttls, key)
	}
	return nil
}

func (f *fakeProjectionRedis) HGetAll(_ context.Context, key string) (map[string]string, error) {
	fields := f.hash[key]
	out := make(map[string]string, len(fields))
	for field, value := range fields {
		out[field] = value
	}
	return out, nil
}

// Scan 只支持 `<前缀>*` 形态（本节只用重建提示那一个模式）。
func (f *fakeProjectionRedis) Scan(_ context.Context, pattern string, limit int) ([]string, error) {
	prefix := strings.TrimSuffix(pattern, "*")
	keys := make([]string, 0, 4)
	for key := range f.raw {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	for key := range f.hash {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	return keys, nil
}

// ---- RollupWriter ----

func (f *fakeProjectionRedis) HIncrByFloat(_ context.Context, key, field string, increment float64) error {
	fields := f.hash[key]
	if fields == nil {
		fields = map[string]string{}
		f.hash[key] = fields
	}
	current := 0.0
	if raw, ok := fields[field]; ok {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err == nil {
			current = parsed
		}
	}
	fields[field] = formatFloat(current + increment)
	return nil
}

// SetNX 复刻 Redis 语义：键已存在（无论类型）都不写，返回 false。
func (f *fakeProjectionRedis) SetNX(_ context.Context, key, value string) (bool, error) {
	if _, exists := f.raw[key]; exists {
		return false, nil
	}
	if _, exists := f.hash[key]; exists {
		return false, nil
	}
	f.raw[key] = value
	return true, nil
}

func (f *fakeProjectionRedis) Expire(_ context.Context, key string, ttlSeconds int) error {
	f.ttls[key] = float64(ttlSeconds)
	return nil
}

// formatFloat 复刻 Redis 的 HINCRBYFLOAT 文本输出（最短往返表示），与 Node 假 Redis 的
// `String(number)` 同形。
func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// dump 输出与 golden 同形的两份表：键值（哈希拉平）与 TTL（秒）。
func (f *fakeProjectionRedis) dump() (map[string]string, map[string]float64) {
	keys := map[string]string{}
	for key, value := range f.raw {
		keys[key] = value
	}
	for key, fields := range f.hash {
		for field, value := range fields {
			keys[key+"\u0000"+field] = value
		}
	}
	ttls := map[string]float64{}
	for key, value := range f.ttls {
		ttls[key] = value
	}
	return keys, ttls
}
