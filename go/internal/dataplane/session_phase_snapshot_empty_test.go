package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
)

// 本文件钉住相位快照「空结果」的上报口径：**只有真异常才走 warn**。
//
// 为什么必须有它：生产实测（2026-09-22T01:09:31Z）一起入站 client_abort 同时产出 1 条
// `dataplane.forward_failed` 与 **3 条** `dataplane.session_phase_snapshot_empty` warn——那 3 条
// 是中断的必然结果（request.after / response.before / response.after 在那时本就无内容可写），
// 而 warn 的语义是「该写却没写成」。旧口径（`len(fields) == 0` 即 warn）把「无内容」与
// 「有内容却写丢」混为一谈，真异常因此被自己的噪音淹没。
//
// 三类成因与判据见 reportPhaseSnapshotEmpty 的注释。下列三个用例各钉一类，且互为反例：
// 无内容不得 warn（用例 1）、有内容却写丢必须 warn（用例 2）、Binder 未就绪必须 warn（用例 3）。
// 三个用例都在同一条 production 路径上（sessionTelemetry.storePhaseSnapshots），不绕开装配。

const phaseSnapshotEmptyEvent = "dataplane.session_phase_snapshot_empty"

// phaseSnapshotLogs 收集日志并按级别 + 事件筛出条目。
type phaseSnapshotLogs struct {
	buffer bytes.Buffer
	logger *logx.Logger
}

func newPhaseSnapshotLogs() *phaseSnapshotLogs {
	logs := &phaseSnapshotLogs{}
	logs.logger = logx.New(&logs.buffer)
	return logs
}

// records 返回指定级别 + 事件的日志条目（按写入顺序）。
func (l *phaseSnapshotLogs) records(level string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(l.buffer.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		if record["level"] == level && record["event"] == phaseSnapshotEmptyEvent {
			out = append(out, record)
		}
	}
	return out
}

// enableDebugLogging 把日志级别拉到 debug（用例要断言 debug 足迹），结束后还原为出厂默认
// trace。级别是进程级全局量，故必须还原，否则会改变同包其他用例的观测面。
func enableDebugLogging(t *testing.T) {
	t.Helper()
	if !logx.SetLevel("debug") {
		t.Fatal("设置日志级别 debug 失败")
	}
	t.Cleanup(func() { logx.SetLevel("trace") })
}

// phaseSnapshotTestLease 是一个最小可用租约：有会话 id 才会走到写侧。
func phaseSnapshotTestLease() TelemetryLease {
	return TelemetryLease{Identity: "sess_phase_empty_test", SessionID: "sess_phase_empty_test", Sequence: 1, KeyID: 7}
}

// newUnreachableBinder 造一个「Ready 为真但所有命令都失败」的 Binder。
//
// 为什么用不可达地址而不是真 Redis：用例 2 要的正是「有内容却写不进去」这一支，而 Redis
// 连接失败就是它最直接的成因；127.0.0.1:1 立即拒绝、MaxRetries=0 不重试，不引入等待。
func newUnreachableBinder(t *testing.T) *session.Binder {
	t.Helper()
	registry, err := ratelimit.Load()
	if err != nil {
		t.Fatalf("加载脚本注册表失败: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: 0})
	t.Cleanup(func() { _ = rdb.Close() })
	client, err := ratelimit.New(rdb, registry)
	if err != nil {
		t.Fatalf("组装脚本调用层失败: %v", err)
	}
	binder := session.NewBinder(client)
	if !binder.Ready() {
		t.Fatal("本用例需要 Ready 为真的 Binder（否则测的就不是「写丢」而是「未接线」）")
	}
	return binder
}

// TestPhaseSnapshotEmptyWithoutContentDoesNotWarn 钉住客户端中断那一类：四个相位都没有内容
// 可写时**一条 warn 都不该有**（这是生产噪音的直接复现）。
func TestPhaseSnapshotEmptyWithoutContentDoesNotWarn(t *testing.T) {
	enableDebugLogging(t)
	logs := newPhaseSnapshotLogs()
	telemetry := &sessionTelemetry{binder: newUnreachableBinder(t), logger: logs.logger}

	// 客户端在建连上游之前断开时的形状：四份相位快照全空。
	telemetry.storePhaseSnapshots(context.Background(), phaseSnapshotTestLease(), ResponseArtifacts{})

	if warns := logs.records("warn"); len(warns) != 0 {
		t.Fatalf("无内容的相位快照不得刷 warn，收到 %d 条：%v", len(warns), warns)
	}
	debugs := logs.records("debug")
	if len(debugs) != 4 {
		t.Fatalf("四个相位各应留一条 debug 足迹，收到 %d 条", len(debugs))
	}
	for _, record := range debugs {
		if record["state"] != "no_content" {
			t.Errorf("debug 应带 state=no_content，收到 %v", record["state"])
		}
	}
}

// TestPhaseSnapshotEmptyWithContentWarns 钉住真异常：相位**有内容**却没写进去时必须 warn
// （写侧对超限字段是删键，写失败也返回空——两者都是数据丢失）。
func TestPhaseSnapshotEmptyWithContentWarns(t *testing.T) {
	enableDebugLogging(t)
	logs := newPhaseSnapshotLogs()
	telemetry := &sessionTelemetry{binder: newUnreachableBinder(t), logger: logs.logger}

	// 响应侧已交付头有内容，但 Binder 的 Redis 不可达 ⇒ 一个字段都没写进去。
	artifacts := ResponseArtifacts{
		ResponseAfter: session.SessionDetailPhaseSnapshot{
			Headers: map[string]string{"content-type": "application/json"},
		},
	}
	telemetry.storePhaseSnapshots(context.Background(), phaseSnapshotTestLease(), artifacts)

	warns := logs.records("warn")
	if len(warns) != 1 {
		t.Fatalf("有内容却写不进去必须 warn 一条，收到 %d 条：%v", len(warns), warns)
	}
	if warns[0]["state"] != "content_dropped" {
		t.Errorf("warn 应带 state=content_dropped，收到 %v", warns[0]["state"])
	}
	if warns[0]["kind"] != "response" || warns[0]["phase"] != "after" {
		t.Errorf("warn 应指向出问题的那一相（response/after），收到 %v/%v",
			warns[0]["kind"], warns[0]["phase"])
	}
	// 另外三相无内容 ⇒ 只留 debug，不得 warn（否则噪音没消，只是被真异常掩盖）。
	if debugs := logs.records("debug"); len(debugs) != 3 {
		t.Errorf("无内容的三个相位应各留一条 debug，收到 %d 条", len(debugs))
	}
}

// TestPhaseSnapshotEmptyWithUnreadyBinderWarns 钉住第二类真异常：Binder 未就绪（Redis 不可用）
// 时整套会话观测都在降级，四个相位各必须 warn。
func TestPhaseSnapshotEmptyWithUnreadyBinderWarns(t *testing.T) {
	enableDebugLogging(t)
	logs := newPhaseSnapshotLogs()
	// Binder 为 nil 即未接线（Ready 为假）；空快照与「未就绪」同时成立时，判据取前者。
	telemetry := &sessionTelemetry{binder: nil, logger: logs.logger}

	telemetry.storePhaseSnapshots(context.Background(), phaseSnapshotTestLease(), ResponseArtifacts{})

	warns := logs.records("warn")
	if len(warns) != 4 {
		t.Fatalf("Binder 未就绪时四个相位各应 warn 一条，收到 %d 条", len(warns))
	}
	for _, record := range warns {
		if record["state"] != "binder_not_ready" {
			t.Errorf("warn 应带 state=binder_not_ready，收到 %v", record["state"])
		}
	}
}
