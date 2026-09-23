package guard

import (
	"os"
	"strings"
	"testing"
)

// 本文件是**源码结构性钉子**：断言「探针事实」真的从 route.Result 搬进了 pctx 槽位。
//
// 为什么必须有它：这条链的两段各自都有测试——选路侧钉住「命中租约的选路结果带 SlowProbe」
// （route 的隔离用例），数据面侧钉住「pctx 上的探针标记会关掉竞速」
// （dataplane.TestHedgeDecisionDisabledForSlowProbe）。但中间这一次搬运谁都盖不住：摘掉它，
// 上面两组用例**全绿**，而真实请求里探针永远被竞速抢走、该组合拿不到干净样本、
// 隔离阶梯永远抬不回档（同类盲区与理由见 dataplane/slow_probe_wiring_nail_test.go）。
//
// 不用行为断言的理由：要造出「探针选路结果」得让 route 的 SlowRate 读侧返回隔离态，
// 那要求本包实现 redis.UniversalClient 的替身（选路侧已有一份），代价远大于这一行搬运本身。
func TestProviderRouterCarriesSlowProbeFlag(t *testing.T) {
	source, err := os.ReadFile("adapters_route.go")
	if err != nil {
		t.Fatalf("读取 adapters_route.go 失败：%v", err)
	}
	const want = "SlowProbe: result.SlowProbe != nil,"
	if strings.Contains(string(source), want) {
		return
	}
	t.Fatalf("adapters_route.go 里找不到把 route.Result.SlowProbe 搬进 pctx.ProviderSelection 的接线：\n"+
		"  期望形如 `%s`\n"+
		"  该行缺失时，数据面的 hedgeDecision 看不到探针标记 ⇒ 探针被竞速的快家取消，\n"+
		"  该组合永远拿不到干净样本（阶梯恢复失效），而两侧单测都不会红。", want)
}
