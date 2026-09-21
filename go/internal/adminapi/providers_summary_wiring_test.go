package adminapi

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住「读投影带出的列必须真的走到响应里」。
//
// 为什么需要它：providerSummary 是**另建**的汇总结构（不复用 store.AdminProvider，因为
// schema 明确不含 key/description 等列），于是每加一列就有两处要登记——读投影与汇总结构。
// 只登前者、漏后者，症状是「库里写了、接口不回」：管理面拿不到值，表单回显空白，
// 而单测与编译**全都不报错**（结构体字段没人读也合法）。
//
// 2026-09-21 上线 1.9.11 时低速降级的七列正是这样漏的：store.AdminProvider 与
// adminProviderColumns 都登了，providerSummary 漏了，生产实测接口一个字段都不返回。
// 静态看代码找不出，只有打真机接口才暴露。故把「结构体标签集合」钉成断言。
//
// 断言口径：把 store.AdminProvider 与 providerSummary 的 **json 标签**各自排序后比对。
// 二者应当同集——providerSummary 只允许**少**掉 schema 明示不含的列（key/description
// 等），那些列不在下面这张豁免表里出现就算漏。

// summaryOmittedFromResponse 是 schema 明确不含、因而 providerSummary **故意**不收的列。
// 每项都要写明理由；新增项同样要写，否则这条钉子就退化成「谁都能加」的摆设。
var summaryOmittedFromResponse = map[string]string{
	"key":         "明文密钥只在写入路径使用，响应只给 maskedKey",
	"description": "schema 明示不含（Hidden legacy provider types and deprecated limit fields are omitted）",
	"tpm":         "deprecated 限额字段，schema 明示不含",
	"rpm":         "deprecated 限额字段，schema 明示不含",
	"rpd":         "deprecated 限额字段，schema 明示不含",
	"cc":          "deprecated 限额字段，schema 明示不含",
}

func jsonTagsOf(value any) []string {
	typ := reflect.TypeOf(value)
	tags := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		// 去掉 ,omitempty / ,string 之类的选项，只留键名。
		for j := 0; j < len(tag); j++ {
			if tag[j] == ',' {
				tag = tag[:j]
				break
			}
		}
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags
}

func TestProviderSummaryCoversEveryReadColumn(t *testing.T) {
	readTags := jsonTagsOf(store.AdminProvider{})
	summaryTags := jsonTagsOf(providerSummary{})

	inSummary := make(map[string]bool, len(summaryTags))
	for _, tag := range summaryTags {
		inSummary[tag] = true
	}

	missing := make([]string, 0)
	for _, tag := range readTags {
		if inSummary[tag] {
			continue
		}
		if reason, omitted := summaryOmittedFromResponse[tag]; omitted {
			if reason == "" {
				t.Errorf("豁免表里的 %q 未写明理由；豁免必须可审计", tag)
			}
			continue
		}
		missing = append(missing, tag)
	}

	if len(missing) > 0 {
		t.Errorf(
			"读投影带出、但响应结构漏掉的列：%v\n"+
				"症状是「库里写了、接口不回」——管理面拿不到值且编译与既有用例全不报错。\n"+
				"要么在 providerSummary 里加字段并在 providerSummaryPayload 里赋值，\n"+
				"要么（确实不该出现时）登记到 summaryOmittedFromResponse 并写明理由。",
			missing,
		)
	}
}

// 反向断言：豁免表不得成为遮羞布——表里的每一项都必须是**真**存在于读投影里的列，
// 否则说明该列已被移除而豁免没清理，表会越长越假。
func TestSummaryOmissionAllowlistHasNoStaleEntry(t *testing.T) {
	readTags := make(map[string]bool)
	for _, tag := range jsonTagsOf(store.AdminProvider{}) {
		readTags[tag] = true
	}
	for tag, reason := range summaryOmittedFromResponse {
		if !readTags[tag] {
			t.Errorf("豁免表里的 %q 已不在读投影里，请删除该项（理由：%s）", tag, reason)
		}
	}
}

// 正向断言：低速降级七列必须在响应里。这条比上面的集合比对更直白——
// 上线 1.9.11 时漏的正是它们，故单列一条用例，让回归时错误信息直接点名。
func TestProviderSummaryCarriesSlowRateColumns(t *testing.T) {
	const payload = `{}`
	_ = json.Valid([]byte(payload)) // 保持 encoding/json 被引用，避免未来重构时误删 import

	required := []string{
		"slowRateMonitorEnabled",
		"slowRateWindowSeconds",
		"slowRateMinSamples",
		"slowRateTriggerCount",
		"slowRateRatioPerMille",
		"slowRatePenaltyStep",
		"slowRatePenaltyMax",
	}
	present := make(map[string]bool)
	for _, tag := range jsonTagsOf(providerSummary{}) {
		present[tag] = true
	}
	for _, tag := range required {
		if !present[tag] {
			t.Errorf("providerSummary 缺少 %q：该列已落库且读投影已带出，漏掉即「库里写了、接口不回」", tag)
		}
	}
}
