package dataplane

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// 本文件是**源码结构性钉子**：断言「首字后停滞探测阈值」真的从库存行接到了转发投影上。
//
// 为什么必须有它：这条链的两段各自都有测试——
//   - 判据与门控（gate.probeReadTimeout 的纯函数用例、gate 的 6 条探测用例、slowrate 的配置读取）；
//   - 判废后的换家与归因（forward 的 Failure.ProbeSlow / AttemptOutcome.ProbeElapsedMS）。
//
// 但「dataplane 到底有没有把该列填进 forward.StreamOptions.ProbeAfterFirstByteFor」谁都盖不住：
// 摘掉这一行时上面两组用例**全绿**，功能却静默失效——库里有配置、管理面显示已配置、
// 实际请求上探测永不触发（阈值恒为 0，probeReadTimeout 退回 IdleTimeout 分支）。
// 这正是「测试全绿但功能没接」的盲区（同类教训见同目录 custom_headers_wiring_nail_test.go），
// 故用源码发现钉死。
var slowProbeWiring = regexp.MustCompile(
	`(?m)^\s*ProbeAfterFirstByteFor:\s+idleTimeouts\.probeAfterFirstByte,\s*$`)

// 提交前速率闸的两条接线同理：摘掉任何一行，配置面全部单测仍绿，而闸在真实请求上永不生效
// （阈值恒为 0 ⇒ gate 认为「不启用」；影子开关恒为 false ⇒ 上线即改变首字时延，
// 而三档阈值尚未标定）。故同样用源码发现钉死。
var precommitRateWiring = regexp.MustCompile(
	`(?m)^\s*PrecommitRateFor:\s+idleTimeouts\.precommitRate,\s*$`)

var precommitShadowWiring = regexp.MustCompile(
	`(?m)^\s*PrecommitShadow:\s+precommitShadowEnabled\(\),\s*$`)

func TestStreamWiringPropagatesPrecommitRateGate(t *testing.T) {
	source := readAssembleSource(t)
	if !precommitRateWiring.Match(source) {
		t.Fatalf("assemble.go 里找不到「providers.slow_rate_precommit_min_bytes_per_second → " +
			"forward.StreamOptions.PrecommitRateFor」的接线：\n" +
			"  期望形如 `PrecommitRateFor: idleTimeouts.precommitRate,`\n" +
			"  该行缺失时，提交前速率闸在真实请求上永不触发（阈值恒为 0）。")
	}
	if !precommitShadowWiring.Match(source) {
		t.Fatalf("assemble.go 里找不到影子开关的接线：\n" +
			"  期望形如 `PrecommitShadow: precommitShadowEnabled(),`\n" +
			"  该行缺失时默认值退化为 false ⇒ 上线即改变首字时延，跳过影子取证期。")
	}
}

func readAssembleSource(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join("assemble.go")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", path, err)
	}
	return source
}

func TestStreamWiringPropagatesSlowProbeThreshold(t *testing.T) {
	source := readAssembleSource(t)
	if !slowProbeWiring.Match(source) {
		t.Fatalf("assemble.go 里找不到「providers.slow_rate_probe_after_first_byte_seconds → " +
			"forward.StreamOptions.ProbeAfterFirstByteFor」的接线：\n" +
			"  期望形如 `ProbeAfterFirstByteFor: idleTimeouts.probeAfterFirstByte,`\n" +
			"  该行缺失时，首字后停滞探测在真实请求上永不触发（两段单测都不会红）。")
	}
}
