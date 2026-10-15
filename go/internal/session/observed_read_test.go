package session

import "testing"

// 会话列表读数的上限：追加必须截到 MaxSessionListIDs，且到达上限后不再增长。
//
// 这条钉住的是「一次请求最多拉多少身份」——没有它，`session:*:info` 的 SCAN 与 ZSET 的
// ZRANGE 都会随库增长（见 observed_read.go 的说明）。
func TestAppendBoundedStopsAtMaxSessionListIDs(t *testing.T) {
	// 未超限：原样追加。
	small := appendBounded([]string{"a"}, []string{"b", "c"})
	if len(small) != 3 || small[0] != "a" || small[2] != "c" {
		t.Fatalf("未超限时应原样追加，收到 %v", small)
	}

	// 超限：截到上限，保留批次前缀（丢弃的是批次尾部）。
	full := make([]string, MaxSessionListIDs-1)
	for index := range full {
		full[index] = "kept"
	}
	capped := appendBounded(full, []string{"tail-1", "tail-2"})
	if len(capped) != MaxSessionListIDs || capped[len(capped)-1] != "tail-1" {
		t.Fatalf("超限应截到 %d 且保留前缀，收到 len=%d 末项=%q",
			MaxSessionListIDs, len(capped), capped[len(capped)-1])
	}

	// 已满：不增长。
	if again := appendBounded(capped, []string{"tail-3"}); len(again) != MaxSessionListIDs {
		t.Fatalf("已满时应原样返回，收到 %d", len(again))
	}
}
