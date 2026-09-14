package forward

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 本文件把「链上 reason 词表必须与 Node 同词」钉住。
//
// 为什么需要钉子：写入侧的词是**跨语言数据契约**（Node 与 Go 写同一列、同一批消费者按
// 精确词判定）。写错一个词不会让任何形状断言变红——列照写、类型照对，只是值不在词表里，
// 消费者于是把它归进「失败」兜底。实测代价：`success` 让可用率恒 0。这类缺陷靠对拍形状
// 永远抓不到，只能靠词表本身对账。
//
// 判据（双向都可复核）：
//  1. Go 侧词表（chainreason.go 的常量 + 本包内所有写链字面量）⊆ 冻结的 Node 词表；
//  2. 冻结的 Node 词表 = `ProviderChainItem.reason` 联合类型 ∪ Node 写链处的字面量
//     （后者必要：`local_overload` 就只出现在写链处、不在联合类型里——Node 的类型漏登记，
//     但**写入值才是事实**，我们跟写入值对齐）。
//
// 为何现在是冻结快照而不是实时抽取：Node 数据面已退役（src/app/v1 删除），实时抽取的
// 真源不存在了；而手抄一份期望值仍不可取——它与「从真源抽取后冻结」（本做法）的区别在于
// 后者由**原抽取逻辑**生成并当场验证过闸门为绿，即快照就是退役时的 Node 事实。

// nodeChainReasonsFixture 是冻结的 Node 链原因词表（一行一个词，头注登记冻结时点与语义降级）。
//
// 语义降级（2026-09-13，Node 数据面退役）：原实现从 Node 源码两处实时抽取——①
// src/types/message.ts 的 reason 联合类型；② src/app/v1/_lib/proxy 与 src/lib 的写链字面量。
// 其中 ② 的 src/app/v1 路径已随数据面删除，且 src/lib 子树也将在退役后续批次删除，
// 故整表冻结为快照（只冻结已删路径会让本闸门在下一批删除时再次转红）：
//  1. 仍能抓到：Go 侧误改/新增词表——这是本闸门的主要价值。词是**跨语言数据契约**
//     （两侧写同一列、消费者按精确词判定），写错一个词不会让任何形状断言变红；
//  2. 不再能抓到：Node 侧将来若发生变动（Node 已退役，不会再有变动）。
//
// 本闸门拒绝静默跳过——读不到夹具必须硬报红。
const nodeChainReasonsFixture = "../../testdata/node_chain_reasons.txt"

// nodeChainReasonsFrozenCount 是冻结词表的词数。夹具是快照，不应变动；
// 若确需变动（例如 Go 新增合法词并同步契约），必须显式改这个数——正是不允许静默漂移。
const nodeChainReasonsFrozenCount = 102

// frozenNodeChainReasonVocabulary 读取冻结的 Node 链原因词表。
func frozenNodeChainReasonVocabulary(t *testing.T) map[string]struct{} {
	t.Helper()
	raw, err := os.ReadFile(nodeChainReasonsFixture)
	if err != nil {
		t.Fatalf("读冻结词表失败（%s）: %v；本闸门读不到真源就形同虚设，不允许静默通过",
			nodeChainReasonsFixture, err)
	}
	vocabulary := map[string]struct{}{}
	for _, line := range strings.Split(string(raw), "\n") {
		word := strings.TrimSpace(line)
		if word == "" || strings.HasPrefix(word, "#") {
			continue
		}
		vocabulary[word] = struct{}{}
	}
	if len(vocabulary) != nodeChainReasonsFrozenCount {
		t.Fatalf("冻结词表解析出 %d 个词，期望 %d：夹具被改动或解析失效",
			len(vocabulary), nodeChainReasonsFrozenCount)
	}
	return vocabulary
}

// goChainReasonVocabulary 抽出 Go 侧写链词表：常量定义 + 本包内写链字面量。
func goChainReasonVocabulary(t *testing.T) map[string]string {
	t.Helper()
	words := map[string]string{}

	constFile := "chainreason.go"
	source, err := os.ReadFile(constFile)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", constFile, err)
	}
	constRe := regexp.MustCompile(`(?m)^\s*Reason[A-Za-z]*\s*=\s*"([a-z_]+)"`)
	for _, match := range constRe.FindAllStringSubmatch(string(source), -1) {
		words[match[1]] = constFile
	}
	if len(words) == 0 {
		t.Fatalf("%s 里没抽到任何 reason 常量：常量命名或正则失效", constFile)
	}

	// 写链字面量必须为零：本包只允许写词表里的常量（一致性靠这个约束维持）。
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("列举本包文件失败: %v", err)
	}
	literalRe := regexp.MustCompile(`(?:Reason\s*[:=]\s*|reason\s*=\s*)"([a-z_]+)"`)
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || file == constFile {
			continue
		}
		content, readErr := os.ReadFile(file)
		if readErr != nil {
			t.Fatalf("读 %s 失败: %v", file, readErr)
		}
		for _, match := range literalRe.FindAllStringSubmatch(string(content), -1) {
			words[match[1]] = file
		}
	}
	return words
}

func TestChainReasonVocabularyMatchesNode(t *testing.T) {
	node := frozenNodeChainReasonVocabulary(t)
	goWords := goChainReasonVocabulary(t)

	var offenders []string
	for word, source := range goWords {
		if _, ok := node[word]; !ok {
			offenders = append(offenders, word+"（来自 "+source+"）")
		}
	}
	if len(offenders) > 0 {
		t.Fatalf(
			"以下 Go 链上 reason 在 Node 侧不存在——写链即污染消费者分类（如 `success` 会让可用率恒 0）：\n  %s\n"+
				"修法：改用 Node 的同义词并更新 chainreason.go 的词表说明；不要靠消费者侧归一。",
			strings.Join(offenders, "\n  "),
		)
	}
}

// TestChainReasonVocabularyCoversNodeSuccessReasons 反向哨兵：Node 的成功词表必须在
// Go 词表里有着落——否则「Go 从不写 Node 认的成功词」这种整类缺陷（可用率恒 0）无人拦。
func TestChainReasonVocabularyCoversNodeSuccessReasons(t *testing.T) {
	goWords := goChainReasonVocabulary(t)
	// 与 internal/pubstatus/rollup.go 的 successReasons 同源（Node 的 SUCCESS_REASONS）。
	nodeSuccess := []string{"request_success", "retry_success", "hedge_winner"}
	covered := 0
	for _, word := range nodeSuccess {
		if _, ok := goWords[word]; ok {
			covered++
		}
	}
	if covered == 0 {
		t.Fatalf("Go 词表与 Node 成功词表无交集（%v）：成功将无法被判为成功", nodeSuccess)
	}
}
