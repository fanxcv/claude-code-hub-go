// Package pricing 承载 model-prices 三兄弟端点（upload / syncLitellmCheck / syncLitellm）
// 所需的**入参解析**与**写入分类**。
//
// 为什么单独成包而不是塞进 internal/adminapi：
//   - 它依赖 internal/jobs 的 CPT 转换器与 internal/store 的价格读写面；放进 adminapi
//     会把「管理面 HTTP 层」与「价格管线」绑死，而管线本身已经被后台任务
//     （internal/jobs 的 PriceSyncer）复用。
//   - 转换器与写入分类是**纯/近纯逻辑**，便于按 Node 产 golden 逐条比对。
//
// 唯一真源：
//   - 上传格式嗅探与转换：src/actions/model-prices.ts:249-283 uploadPriceTable
//   - CPT 校验：src/lib/price-sync/cpt-schema.ts parseCptTableValue
//   - 旧版 TOML：src/lib/price-sync/cloud-price-table.ts parseCloudPriceTableToml
//   - 写入分类：src/actions/model-prices.ts:112 processPriceTableInternal
package pricing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
)

// cptSchemaID 对应 cpt-schema.ts:12 的 CPT_SCHEMA_ID。
const cptSchemaID = "cchp.pricing-table/v1"

// metadataFieldSampleSpec 对应 processPriceTableInternal 的 METADATA_FIELDS
// （actions/model-prices.ts:142）。它是价格表里的元数据字段，不是模型。
const metadataFieldSampleSpec = "sample_spec"

// Entry 是「待写入的一个模型」：名字 + 该模型的 priceData 原文。
//
// Data 保持 json.RawMessage 而不是解成 any：写入前的相等判定与落库都按 JSON 语义走，
// 中间解成 any 再重新编码会引入 float64 精度与键序的重编码差异。
type Entry struct {
	Name string
	Data json.RawMessage
}

// UnsupportedTOML 是旧版 TOML 上传路径的降级错误（Go 侧无 TOML 库，见包注释与报告）。
//
// Node 侧 parseCloudPriceTableToml 会接受旧版 TOML 价格表；Go 侧目前**不解析 TOML**，
// 因此「上传合法 TOML」在 Go 上会失败（HTTP 形状仍是 400 + model_price.action_failed，
// 与 Node 的 TOML 解析失败同形，只有可观察行为不同）。这条降级登记在
// 并已向协调者报备（补库即可闭合）。
var UnsupportedTOML = errors.New(
	"价格表 TOML 解析失败: Go 侧暂不支持旧版 TOML 价格表（仅支持 CPT v1 JSON 与内部 JSON 格式）",
)

// ParseUploadContent 复刻 uploadPriceTable 的格式嗅探（actions/model-prices.ts:262-283）：
//
//  1. 先按 JSON 解析；若解析成功且是 CPT v1 表（schema 匹配）→ 按 cpt-schema 校验并转换；
//  2. JSON 解析成功但不是 CPT → 当作**内部格式**（model_name → priceData 的对象）原样使用；
//  3. JSON 解析失败 → 按旧版 TOML 解析（Go 侧降级为 UnsupportedTOML）。
//
// 返回的 Entry 顺序 = 表格文档顺序（CPT 路径见 ConvertedTableEntries 的说明）。
// 错误文案与 Node 逐字同形（`价格表格式无效：…` / `价格表 JSON 解析失败: …`），
// 便于对拍与排障；HTTP 层只暴露公开 detail（problem.go 的 WriteActionError）。
func ParseUploadContent(content string) ([]Entry, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		// Node：JSON.parse("") 抛错 → 落到 TOML 分支 → TOML.parse("") 亦抛错。
		return nil, UnsupportedTOML
	}

	if json.Valid([]byte(trimmed)) {
		if isCptTableLike([]byte(trimmed)) {
			table, err := jobs.ParseCptTable([]byte(trimmed))
			if err != nil {
				return nil, err
			}
			converted := jobs.ConvertCptTable(table)
			if len(converted.Models) == 0 {
				// Node 侧此处不判空：转换结果为空会走到 processPriceTableInternal 的
				// 「models 为空」判定并返回失败（见 WriteEntries）。保持一致，不提前拦截。
				return nil, nil
			}
			return ConvertedTableEntries(converted), nil
		}
		return legacyEntriesFromObject([]byte(trimmed))
	}

	return nil, UnsupportedTOML
}

// isCptTableLike 对应 cpt-schema.ts:97 的 isCptTableLike：根是对象、schema 命中、models 是数组。
func isCptTableLike(raw []byte) bool {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil || root == nil {
		return false
	}
	schemaRaw, ok := root["schema"]
	if !ok {
		return false
	}
	var schema string
	if err := json.Unmarshal(schemaRaw, &schema); err != nil || schema != cptSchemaID {
		return false
	}
	modelsRaw, ok := root["models"]
	if !ok {
		return false
	}
	var models []json.RawMessage
	if err := json.Unmarshal(modelsRaw, &models); err != nil {
		return false
	}
	return true
}

// legacyEntriesFromObject 读内部格式价格表（顶层 model_name → priceData 对象），保留文档顺序。
//
// Node 的 processPriceTableInternal 用 Object.entries(priceTable) 迭代，顺序即 JSON 文档顺序；
// Go 的 map 无序，故这里用流式解码取键序，避免结果数组顺序与 Node 无谓地不同。
func legacyEntriesFromObject(raw []byte) ([]Entry, error) {
	pairs, err := orderedObjectPairs(raw)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(pairs))
	for _, pair := range pairs {
		if strings.TrimSpace(pair.key) == "" {
			continue
		}
		if pair.key == metadataFieldSampleSpec {
			continue
		}
		entries = append(entries, Entry{Name: pair.key, Data: pair.value})
	}
	return entries, nil
}

type orderedPair struct {
	key   string
	value json.RawMessage
}

// orderedObjectPairs 按 JSON 文档顺序取出顶层对象的键值对。
func orderedObjectPairs(raw []byte) ([]orderedPair, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("价格表 JSON 解析失败: %w", err)
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		// 非对象：Node 的 processPriceTableInternal 会返回「价格表必须是一个JSON对象」。
		return nil, errNotJSONObject
	}

	pairs := make([]orderedPair, 0, 64)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("价格表 JSON 解析失败: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errNotJSONObject
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("价格表 JSON 解析失败: %w", err)
		}
		pairs = append(pairs, orderedPair{key: key, value: value})
	}
	return pairs, nil
}

// errNotJSONObject 对应 processPriceTableInternal 的「价格表必须是一个JSON对象」。
var errNotJSONObject = errors.New("价格表必须是一个JSON对象")

// ErrNotJSONObject 暴露给调用方做状态映射（Node 该错误同样落到 400 action_failed）。
func ErrNotJSONObject() error { return errNotJSONObject }

// ConvertedTableEntries 把 CPT 转换结果摊平成 Entry 列表。
//
// **顺序说明（有意偏离）**：Node 的 converted.models 是对象，迭代顺序 = 插入顺序
// （canonical 模型按 CPT 文档序，随后按同样的序追加别名）。Go 的 jobs.ConvertCptTable
// 返回 map，插入顺序不可得，故这里**按模型名排序**——集合与 Node 完全一致，只有结果
// 数组（added/updated/unchanged/failed）的顺序可能不同。该差异登记在报告的白名单里，
// 与后台同步任务（internal/jobs 的 PriceSyncer 亦用排序）保持同一口径。
func ConvertedTableEntries(converted *jobs.ConvertedCptTable) []Entry {
	if converted == nil {
		return nil
	}
	names := make([]string, 0, len(converted.Models))
	for name := range converted.Models {
		names = append(names, name)
	}
	sortStrings(names)

	entries := make([]Entry, 0, len(names))
	for _, name := range names {
		encoded, err := json.Marshal(converted.Models[name])
		if err != nil {
			// 转换产物来自 map[string]any，Marshal 只可能因极端值失败；跳过而不是写入半截数据。
			continue
		}
		entries = append(entries, Entry{Name: name, Data: encoded})
	}
	return entries
}

// sortStrings 是包内最小排序（避免为一个比较函数引入 sort 包的泛型包装）。
func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
