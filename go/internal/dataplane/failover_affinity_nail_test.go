package dataplane

import (
	"regexp"
	"strings"
	"testing"
)

// 本文件是**源码结构性钉子**：断言故障转移的生产端调用点真的走了「不咨询亲和」的入口。
//
// 为什么需要它：语义本身由 `route.SelectFailover` 的两个用例证明（含一条直接数亲和 Redis
// 访问次数的用例），但「生产端到底调用了哪个入口」盖不住——把 `SelectFailover` 改回 `Select`、
// 或把 `AffinityBody` 重新塞回请求，那两组用例照样全绿（route 包内根本看不到 upstream.go）。
// 这正是「测试全绿但接线回退」的盲区，与 settle_wiring_nail_test.go 同一机理。
//
// 与那条既有钉子的区别：这条只钉**一个**调用点，故不做函数体提取——`Failover` 方法体内
// 不得出现 `AffinityBody`，且必须出现 `SelectFailover`，两条断言合起来就足以排除回退。
var (
	// 行首允许 tab 缩进；匹配的是调用语句本身。
	failoverSelectCall     = regexp.MustCompile(`(?m)^\s*result, err := s\.router\.selector\.SelectFailover\(ctx, route\.Request\{\s*$`)
	failoverAffinityAssign = regexp.MustCompile(`(?m)^\s*AffinityBody:`)
)

func TestFailoverWiringNeverConsultsAffinity(t *testing.T) {
	source := readUpstreamSource(t)
	body := funcBody(t, source, "func (s *candidateSource) Failover(")

	if !failoverSelectCall.MatchString(body) {
		t.Fatal("Failover 没有走 route.SelectFailover：故障转移会重新咨询前缀亲和（Node parity 分叉回退）")
	}
	if failoverAffinityAssign.MatchString(body) {
		t.Fatal("Failover 仍在请求里带 AffinityBody：该路径不该参与亲和，传着不用会给回退留复活机会")
	}
}

// TestFailoverWiringNailIsNotVacuous 防「钉子自己变成空跑」：先证明函数体真的取到了。
func TestFailoverWiringNailIsNotVacuous(t *testing.T) {
	source := readUpstreamSource(t)
	body := funcBody(t, source, "func (s *candidateSource) Failover(")
	if len(body) < 400 {
		t.Fatalf("Failover 函数体只有 %d 字节，提取显然失效", len(body))
	}
	if !strings.Contains(body, "ExcludeIDs") {
		t.Fatal("Failover 函数体里没有 ExcludeIDs：可能取到了别的函数体")
	}
}
