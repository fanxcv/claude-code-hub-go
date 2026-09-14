package cfgsync

import "testing"

func TestSnapshotLifecycle(t *testing.T) {
	snapshot := New()
	if snapshot.Loaded() {
		t.Fatal("新建快照必须是未装载状态")
	}
	if !snapshot.LoadedAt().IsZero() {
		t.Fatalf("未装载时 LoadedAt 应为零值，收到 %v", snapshot.LoadedAt())
	}
	if snapshot.Version() != "" {
		t.Fatalf("未装载时 Version 应为空，收到 %q", snapshot.Version())
	}

	snapshot.MarkLoaded("v1")
	if !snapshot.Loaded() {
		t.Fatal("MarkLoaded 之后必须是已装载状态")
	}
	if snapshot.Version() != "v1" {
		t.Fatalf("Version 应为 v1，收到 %q", snapshot.Version())
	}
	if snapshot.LoadedAt().IsZero() {
		t.Fatal("MarkLoaded 必须记录时间")
	}
}
