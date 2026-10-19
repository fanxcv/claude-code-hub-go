package cfgsync

import (
	"testing"
	"time"
)

func BenchmarkTTLMapGetHit(b *testing.B) {
	table := NewTTLMap[string, int](time.Hour, 64)
	table.Set("k", 1)
	b.ReportAllocs()
	for index := 0; index < b.N; index++ {
		if _, ok := table.Get("k"); !ok {
			b.Fatal("应命中")
		}
	}
}

func BenchmarkTTLMapGetHitParallel(b *testing.B) {
	table := NewTTLMap[string, int](time.Hour, 64)
	table.Set("k", 1)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, ok := table.Get("k"); !ok {
				b.Fatal("应命中")
			}
		}
	})
}

func BenchmarkTTLMapMixedParallel(b *testing.B) {
	table := NewTTLMap[string, int](time.Hour, 64)
	keys := make([]string, 8)
	for index := 0; index < 8; index++ {
		keys[index] = string(rune('a' + index))
		table.Set(keys[index], index)
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		slot := 0
		for pb.Next() {
			slot++
			key := keys[slot%8]
			table.Get(key)
		}
	})
}
