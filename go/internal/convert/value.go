package convert

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Value 是保序 JSON 值。
//
// 为什么不用 map[string]any + encoding/json：
//   - 语料要求与 TS 侧输出逐字节一致，而 encoding/json 会把对象键排序，TS 侧保留
//     编码器的属性插入顺序；
//   - encoding/json 默认把 <, >, & 转义成 \u003c 等形式，JS 的 JSON.stringify 不转义；
//   - 数字经 float64 往返会丢失字面量形态（如 1e21、超长整数）。
//
// 故本类型自己持有成员顺序与数字字面量，并自带与 JS 语义一致的序列化。
type Value struct {
	kind    valueKind
	str     string
	literal string // 数字的原样字面量
	boolean bool
	members []Member // 对象成员，保序
	items   []*Value // 数组元素，保序
}

type valueKind uint8

const (
	kindNull valueKind = iota
	kindBool
	kindNumber
	kindString
	kindArray
	kindObject
)

// Member 是对象的一个键值对。
type Member struct {
	Key   string
	Value *Value
}

// NewNull 返回 JSON null。
func NewNull() *Value { return &Value{kind: kindNull} }

// NewBool 返回 JSON 布尔。
func NewBool(v bool) *Value { return &Value{kind: kindBool, boolean: v} }

// NewString 返回 JSON 字符串。
func NewString(v string) *Value { return &Value{kind: kindString, str: v} }

// NewNumber 以字面量构造 JSON 数字（十进制字面量，不做浮点往返）。
func NewNumber(literal string) *Value { return &Value{kind: kindNumber, literal: literal} }

// NewNumberInt 以整数构造 JSON 数字。
func NewNumberInt(v int64) *Value { return &Value{kind: kindNumber, literal: strconv.FormatInt(v, 10)} }

// NewArray 构造数组，nil 元素视为 null。
func NewArray(items ...*Value) *Value {
	out := &Value{kind: kindArray, items: make([]*Value, 0, len(items))}
	for _, item := range items {
		out.items = append(out.items, orNull(item))
	}
	return out
}

// NewObject 构造空对象。
func NewObject() *Value { return &Value{kind: kindObject} }

// Kind 名称，供测试断言可读。
func (v *Value) Kind() string {
	if v == nil {
		return "nil"
	}
	switch v.kind {
	case kindNull:
		return "null"
	case kindBool:
		return "bool"
	case kindNumber:
		return "number"
	case kindString:
		return "string"
	case kindArray:
		return "array"
	case kindObject:
		return "object"
	default:
		return "unknown"
	}
}

// IsNull 判断是否 JSON null（含 nil 接收者）。
func (v *Value) IsNull() bool { return v == nil || v.kind == kindNull }

// IsObject 判断是否对象。
func (v *Value) IsObject() bool { return v != nil && v.kind == kindObject }

// IsArray 判断是否数组。
func (v *Value) IsArray() bool { return v != nil && v.kind == kindArray }

// IsString 判断是否字符串。
func (v *Value) IsString() bool { return v != nil && v.kind == kindString }

// String 取字符串值；非字符串返回 (零值, false)。
func (v *Value) String() (string, bool) {
	if v == nil || v.kind != kindString {
		return "", false
	}
	return v.str, true
}

// Bool 取布尔值。
func (v *Value) Bool() (bool, bool) {
	if v == nil || v.kind != kindBool {
		return false, false
	}
	return v.boolean, true
}

// NumberLiteral 取数字字面量。
func (v *Value) NumberLiteral() (string, bool) {
	if v == nil || v.kind != kindNumber {
		return "", false
	}
	return v.literal, true
}

// Int64 取整数值；非整数返回 (0, false)。
func (v *Value) Int64() (int64, bool) {
	if v == nil || v.kind != kindNumber {
		return 0, false
	}
	parsed, err := strconv.ParseInt(v.literal, 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// Float64 取浮点值。
func (v *Value) Float64() (float64, bool) {
	if v == nil || v.kind != kindNumber {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(v.literal, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// Members 返回对象成员；非对象返回 nil。
func (v *Value) Members() []Member {
	if v == nil || v.kind != kindObject {
		return nil
	}
	return v.members
}

// Items 返回数组元素；非数组返回 nil。
func (v *Value) Items() []*Value {
	if v == nil || v.kind != kindArray {
		return nil
	}
	return v.items
}

// Len 返回数组长度；非数组返回 0。
func (v *Value) Len() int {
	if v == nil {
		return 0
	}
	return len(v.items)
}

// Has 判断对象是否含该键。
func (v *Value) Has(key string) bool {
	_, ok := v.Get(key)
	return ok
}

// Get 取对象成员；键不存在返回 (nil, false)。
func (v *Value) Get(key string) (*Value, bool) {
	if v == nil || v.kind != kindObject {
		return nil, false
	}
	for i := range v.members {
		if v.members[i].Key == key {
			return v.members[i].Value, true
		}
	}
	return nil, false
}

// StringField 取字段的字符串值；缺失或非字符串返回 (零值, false)。
func (v *Value) StringField(key string) (string, bool) {
	child, ok := v.Get(key)
	if !ok {
		return "", false
	}
	return child.String()
}

// ArrayField 取字段的数组；缺失或非数组返回 nil。
func (v *Value) ArrayField(key string) []*Value {
	child, ok := v.Get(key)
	if !ok || !child.IsArray() {
		return nil
	}
	return child.items
}

// ObjectField 取字段的对象；缺失或非对象返回 nil。
func (v *Value) ObjectField(key string) *Value {
	child, ok := v.Get(key)
	if !ok || !child.IsObject() {
		return nil
	}
	return child
}

// Set 设置成员；键已存在时原位替换（保持首次插入位置，与 JS 对象一致）。
func (v *Value) Set(key string, value *Value) *Value {
	if v.kind != kindObject {
		v.kind = kindObject
		v.members = nil
	}
	for i := range v.members {
		if v.members[i].Key == key {
			v.members[i].Value = orNull(value)
			return v
		}
	}
	v.members = append(v.members, Member{Key: key, Value: orNull(value)})
	return v
}

// Delete 删除成员；不存在则无操作。
func (v *Value) Delete(key string) *Value {
	if v == nil || v.kind != kindObject {
		return v
	}
	for i := range v.members {
		if v.members[i].Key == key {
			v.members = append(v.members[:i], v.members[i+1:]...)
			return v
		}
	}
	return v
}

// Append 追加数组元素。
func (v *Value) Append(items ...*Value) *Value {
	if v.kind != kindArray {
		v.kind = kindArray
		v.items = nil
	}
	for _, item := range items {
		v.items = append(v.items, orNull(item))
	}
	return v
}

// Clone 深拷贝。
func (v *Value) Clone() *Value {
	if v == nil {
		return nil
	}
	out := &Value{kind: v.kind, str: v.str, literal: v.literal, boolean: v.boolean}
	for _, member := range v.members {
		out.members = append(out.members, Member{Key: member.Key, Value: member.Value.Clone()})
	}
	for _, item := range v.items {
		out.items = append(out.items, item.Clone())
	}
	return out
}

// MarshalCompact 按 JS JSON.stringify 的语义序列化：紧凑、无多余空格、键序保留、
// 非 ASCII 原样输出、控制字符用 \u00XX。
func (v *Value) MarshalCompact() string {
	var builder strings.Builder
	v.writeInto(&builder)
	return builder.String()
}

func (v *Value) writeInto(out *strings.Builder) {
	if v == nil {
		out.WriteString("null")
		return
	}
	switch v.kind {
	case kindNull:
		out.WriteString("null")
	case kindBool:
		if v.boolean {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case kindNumber:
		out.WriteString(v.literal)
	case kindString:
		writeJSONString(out, v.str)
	case kindArray:
		out.WriteByte('[')
		for i, item := range v.items {
			if i > 0 {
				out.WriteByte(',')
			}
			item.writeInto(out)
		}
		out.WriteByte(']')
	case kindObject:
		out.WriteByte('{')
		for i := range v.members {
			if i > 0 {
				out.WriteByte(',')
			}
			writeJSONString(out, v.members[i].Key)
			out.WriteByte(':')
			v.members[i].Value.writeInto(out)
		}
		out.WriteByte('}')
	default:
		out.WriteString("null")
	}
}

const hexDigits = "0123456789abcdef"

// writeJSONString 复刻 JS JSON.stringify 的字符串转义：
// 只转义 ", \, 以及 < 0x20 的控制字符；非 ASCII 原样输出；不转义 <, >, &。
func writeJSONString(out *strings.Builder, value string) {
	out.WriteByte('"')
	for i := 0; i < len(value); i++ {
		b := value[i]
		switch b {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if b < 0x20 {
				out.WriteString(`\u00`)
				out.WriteByte(hexDigits[b>>4])
				out.WriteByte(hexDigits[b&0x0f])
				continue
			}
			out.WriteByte(b)
		}
	}
	out.WriteByte('"')
}

// ---------------------------------------------------------------------------
// 解析
// ---------------------------------------------------------------------------

// ParseJSON 解析 JSON 并保留对象键顺序与数字字面量。
func ParseJSON(data []byte) (*Value, error) {
	parser := &jsonParser{input: data}
	parser.skipWhitespace()
	value, err := parser.parseValue()
	if err != nil {
		return nil, err
	}
	parser.skipWhitespace()
	if parser.pos != len(parser.input) {
		return nil, fmt.Errorf("JSON 末尾存在多余内容，位置 %d", parser.pos)
	}
	return value, nil
}

// TopLevelBoolTrue 判定「整份输入是合法 JSON，且顶层对象里 key 成员的最终值为布尔 true」，
// 并**避免为此构造 Value 树**。
//
// 动机（生产剖析，2026-09）：调用方（forward 的流式判定）只需要一个顶层布尔，却要为它
// 把整份请求正文建成树，同一请求还会问三次 —— 这一项占全服务累计内存分配的 15.2%。
//
// 语义与 `ParseJSON(data) → Get(key) → Bool()` **严格等价**（对拍见
// TestTopLevelBoolTrueMatchesParseJSON）：
//
//  1. 整份输入必须合法：末尾有多余内容同样算非法（ParseJSON 的判据）；
//  2. 顶层必须是对象，且该成员必须是**布尔字面量**（`"true"` 字符串、`1` 数字都不算）；
//  3. key 重复时**后者覆盖前者**（Value.Set 是覆盖，不是保留首个）；
//  4. 空输入、非法 JSON、非对象顶层等边界一律判否。
//
// 快速路径遇到自己未覆盖的形态（目前只有「成员名含转义」）时**回退到 ParseJSON**，
// 故正确性由权威实现兜底；回退只发生在真实请求里不出现的形态上。
func TopLevelBoolTrue(data []byte, key string) bool {
	if result, decided := probeTopLevelBoolTrue(data, key); decided {
		return result
	}
	value, err := ParseJSON(data)
	if err != nil {
		return false
	}
	field, ok := value.Get(key)
	if !ok {
		return false
	}
	requested, _ := field.Bool()
	return requested
}

// probeTopLevelBoolTrue 是 TopLevelBoolTrue 的零分配快速路径。
//
// 返回的第二值表示「是否已确定」；为 false 时调用方必须回退到 ParseJSON。
func probeTopLevelBoolTrue(data []byte, key string) (result bool, decided bool) {
	parser := &jsonParser{input: data}
	parser.skipWhitespace()
	if parser.pos < len(parser.input) && parser.input[parser.pos] == '{' {
		result, decided, err := parser.probeTopLevelMembers(key)
		if err != nil {
			return false, true // 非法输入：与 ParseJSON 一致地判否，无需回退
		}
		if !decided {
			return false, false
		}
		parser.skipWhitespace()
		if parser.pos != len(parser.input) {
			return false, true // 末尾多余内容
		}
		return result, true
	}
	// 顶层不是对象：仍须完整校验合法性（非法 ⇒ false，对象判定恒 false）。
	if err := parser.skipValue(); err != nil {
		return false, true
	}
	parser.skipWhitespace()
	if parser.pos != len(parser.input) {
		return false, true
	}
	return false, true
}

// probeTopLevelMembers 遍历顶层对象的成员：成员名等于 key 时记下其值是否为布尔 true
// （后者覆盖前者，与 Value.Set 一致），其余成员一律跳过。
//
// decided=false 表示遇到快速路径不处理的形态（成员名含转义 ⇒ 需解码才能比较），
// 调用方必须回退到 ParseJSON。
func (p *jsonParser) probeTopLevelMembers(key string) (result bool, decided bool, err error) {
	p.pos++ // {
	p.skipWhitespace()
	if p.pos < len(p.input) && p.input[p.pos] == '}' {
		p.pos++
		return false, true, nil
	}
	for {
		p.skipWhitespace()
		equal, escaped, err := p.skipStringCompare(key)
		if err != nil {
			return false, false, err
		}
		if escaped {
			return false, false, nil
		}
		p.skipWhitespace()
		if p.pos >= len(p.input) || p.input[p.pos] != ':' {
			return false, false, fmt.Errorf("对象内期望 :，位置 %d", p.pos)
		}
		p.pos++
		p.skipWhitespace()
		if !equal {
			if err := p.skipValue(); err != nil {
				return false, false, err
			}
		} else if p.pos < len(p.input) && p.input[p.pos] == 't' {
			// 只需知道它是不是字面量 true：是则跳过它，否则整个跳过（跳过后 result=false）。
			if err := p.skipLiteral("true"); err != nil {
				return false, false, err
			}
			result = true
		} else {
			if err := p.skipValue(); err != nil {
				return false, false, err
			}
			result = false
		}
		p.skipWhitespace()
		if p.pos >= len(p.input) {
			return false, false, fmt.Errorf("对象未闭合")
		}
		switch p.input[p.pos] {
		case ',':
			p.pos++
		case '}':
			p.pos++
			return result, true, nil
		default:
			return false, false, fmt.Errorf("对象内期望 , 或 }，位置 %d", p.pos)
		}
	}
}

// skipValue 按 parseValue 的接受域跳过（校验）一个 JSON 值，**不构造 Value**。
//
// 每个分支逐条对齐 parseValue 及其子方法：字面量比对字节前缀、数字沿用同一字符集与
// ParseFloat 校验、字符串沿用同一转义白名单、数组与对象沿用同一分隔符判据。
func (p *jsonParser) skipValue() error {
	if p.pos >= len(p.input) {
		return fmt.Errorf("JSON 意外结束")
	}
	switch c := p.input[p.pos]; {
	case c == '{':
		return p.skipObject()
	case c == '[':
		return p.skipArray()
	case c == '"':
		return p.skipString()
	case c == 't':
		return p.skipLiteral("true")
	case c == 'f':
		return p.skipLiteral("false")
	case c == 'n':
		return p.skipLiteral("null")
	default:
		return p.skipNumber()
	}
}

// skipLiteral 与 parseLiteral 同一判据，只是不产出 Value。
func (p *jsonParser) skipLiteral(word string) error {
	if len(p.input)-p.pos < len(word) {
		return fmt.Errorf("期望 %q，位置 %d", word, p.pos)
	}
	for index := 0; index < len(word); index++ {
		if p.input[p.pos+index] != word[index] {
			return fmt.Errorf("期望 %q，位置 %d", word, p.pos)
		}
	}
	p.pos += len(word)
	return nil
}

// skipNumber 与 parseNumber 同一字符集与同一 ParseFloat 判据，只是不产出 Value。
func (p *jsonParser) skipNumber() error {
	start := p.pos
	for p.pos < len(p.input) {
		c := p.input[p.pos]
		if (c >= '0' && c <= '9') || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' {
			p.pos++
			continue
		}
		break
	}
	if start == p.pos {
		return fmt.Errorf("位置 %d 处不是合法 JSON 值", start)
	}
	if _, err := strconv.ParseFloat(string(p.input[start:p.pos]), 64); err != nil {
		return fmt.Errorf("非法数字字面量")
	}
	return nil
}

// skipString 跳过（校验）一个 JSON 字符串：闭合、转义白名单与 \uXXXX 的合法性
// 都按 parseString / parseStringEscaped 的同一判据，但不组装文本（故零分配）。
//
// 注意快路径的既有宽松处**刻意保留**：parseString 的快路径只找 " 与 \，不校验裸控制字符，
// 本函数同样不校验，否则接受域会比权威实现更严。
func (p *jsonParser) skipString() error {
	if p.pos >= len(p.input) || p.input[p.pos] != '"' {
		return fmt.Errorf("期望字符串起始引号，位置 %d", p.pos)
	}
	p.pos++
	for p.pos < len(p.input) {
		switch c := p.input[p.pos]; c {
		case '"':
			p.pos++
			return nil
		case '\\':
			p.pos++
			if p.pos >= len(p.input) {
				return fmt.Errorf("字符串转义意外结束")
			}
			switch escaped := p.input[p.pos]; escaped {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				// 合法单字符转义：parseStringEscaped 的同一条白名单。
			case 'u':
				if p.pos+4 >= len(p.input) {
					return fmt.Errorf("\\u 转义不完整")
				}
				// 等价于 parseStringEscaped 的 strconv.ParseUint(s, 16, 32)：
				// 定长四字符下它成功当且仅当四个都是十六进制位（无符号、base 16 不接受下划线）。
				for offset := 1; offset <= 4; offset++ {
					if !isHexDigit(p.input[p.pos+offset]) {
						return fmt.Errorf("非法 \\u 转义")
					}
				}
				p.pos += 4
			default:
				return fmt.Errorf("未知转义 \\%c", escaped)
			}
			p.pos++
		default:
			p.pos++
		}
	}
	return fmt.Errorf("字符串未闭合")
}

// isHexDigit 判定十六进制位（大小写皆可）。
func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// skipStringCompare 跳过（校验）一个 JSON 字符串，并就地与 word 比较。
//
// escaped 为真表示字符串含转义序列：此时比较结果不可信（key 需要解码才能判定相等），
// 调用方必须回退到 ParseJSON。带转义的成员名在真实请求里不出现，用它换掉
// 「构造 key 字符串」的那次分配并不划算，故直接交回权威实现。
func (p *jsonParser) skipStringCompare(word string) (equal bool, escaped bool, err error) {
	if p.pos >= len(p.input) || p.input[p.pos] != '"' {
		return false, false, fmt.Errorf("期望字符串起始引号，位置 %d", p.pos)
	}
	p.pos++
	start := p.pos
	for p.pos < len(p.input) {
		switch p.input[p.pos] {
		case '"':
			text := p.input[start:p.pos]
			p.pos++
			if len(text) != len(word) {
				return false, false, nil
			}
			for index := 0; index < len(word); index++ {
				if text[index] != word[index] {
					return false, false, nil
				}
			}
			return true, false, nil
		case '\\':
			return false, true, nil
		default:
			p.pos++
		}
	}
	return false, false, fmt.Errorf("字符串未闭合")
}

// skipArray 与 parseArray 同一判据，用 skipValue 代替 parseValue。
func (p *jsonParser) skipArray() error {
	p.pos++ // [
	p.skipWhitespace()
	if p.pos < len(p.input) && p.input[p.pos] == ']' {
		p.pos++
		return nil
	}
	for {
		p.skipWhitespace()
		if err := p.skipValue(); err != nil {
			return err
		}
		p.skipWhitespace()
		if p.pos >= len(p.input) {
			return fmt.Errorf("数组未闭合")
		}
		switch p.input[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return nil
		default:
			return fmt.Errorf("数组内期望 , 或 ]，位置 %d", p.pos)
		}
	}
}

// skipObject 与 parseObject 同一判据，用 skipString/skipValue 代替 parseString/parseValue。
func (p *jsonParser) skipObject() error {
	p.pos++ // {
	p.skipWhitespace()
	if p.pos < len(p.input) && p.input[p.pos] == '}' {
		p.pos++
		return nil
	}
	for {
		p.skipWhitespace()
		if err := p.skipString(); err != nil {
			return err
		}
		p.skipWhitespace()
		if p.pos >= len(p.input) || p.input[p.pos] != ':' {
			return fmt.Errorf("对象内期望 :，位置 %d", p.pos)
		}
		p.pos++
		p.skipWhitespace()
		if err := p.skipValue(); err != nil {
			return err
		}
		p.skipWhitespace()
		if p.pos >= len(p.input) {
			return fmt.Errorf("对象未闭合")
		}
		switch p.input[p.pos] {
		case ',':
			p.pos++
		case '}':
			p.pos++
			return nil
		default:
			return fmt.Errorf("对象内期望 , 或 }，位置 %d", p.pos)
		}
	}
}

type jsonParser struct {
	input []byte
	pos   int
}

func (p *jsonParser) skipWhitespace() {
	for p.pos < len(p.input) {
		switch p.input[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *jsonParser) parseValue() (*Value, error) {
	if p.pos >= len(p.input) {
		return nil, fmt.Errorf("JSON 意外结束")
	}
	switch c := p.input[p.pos]; {
	case c == '{':
		return p.parseObject()
	case c == '[':
		return p.parseArray()
	case c == '"':
		text, err := p.parseString()
		if err != nil {
			return nil, err
		}
		return NewString(text), nil
	case c == 't':
		return p.parseLiteral("true", NewBool(true))
	case c == 'f':
		return p.parseLiteral("false", NewBool(false))
	case c == 'n':
		return p.parseLiteral("null", NewNull())
	default:
		return p.parseNumber()
	}
}

func (p *jsonParser) parseLiteral(word string, value *Value) (*Value, error) {
	// 逐字节比对，**不用** `strings.HasPrefix(string(p.input[p.pos:]), word)`：
	// 后者会把**整个剩余输入**拷成字符串，而 true/false/null 在真实请求体里到处都是
	// ⇒ 单次解析的分配量是 O(n^2)。实测（64 KiB 体、每消息一个 null+false）：
	// 修前 26.6 MB/次分配、2.88 ms；256 KiB 体则 398 MB/次、53.6 ms（生产剖析里
	// 本函数占全服务累计分配的 31.9%）。
	if len(p.input)-p.pos < len(word) {
		return nil, fmt.Errorf("期望 %q，位置 %d", word, p.pos)
	}
	for index := 0; index < len(word); index++ {
		if p.input[p.pos+index] != word[index] {
			return nil, fmt.Errorf("期望 %q，位置 %d", word, p.pos)
		}
	}
	p.pos += len(word)
	return value, nil
}

func (p *jsonParser) parseNumber() (*Value, error) {
	start := p.pos
	for p.pos < len(p.input) {
		c := p.input[p.pos]
		if (c >= '0' && c <= '9') || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' {
			p.pos++
			continue
		}
		break
	}
	if start == p.pos {
		return nil, fmt.Errorf("位置 %d 处不是合法 JSON 值", start)
	}
	literal := string(p.input[start:p.pos])
	if _, err := strconv.ParseFloat(literal, 64); err != nil {
		return nil, fmt.Errorf("非法数字字面量 %q", literal)
	}
	return NewNumber(literal), nil
}

func (p *jsonParser) parseString() (string, error) {
	// 上界必须显式判：本函数是 parseObject 读成员名的入口，而对象在读到成员名之前只做过
	// 空白与外层分隔符检查——输入 `{`（或 `{"a":1,`）走到这里时 pos 已等于长度。
	// 此前缺这道守卫，于是 ParseJSON(截断输入) 会**越界 panic** 而不是返回错误；
	// 客户端正文直接来自网络，故这条路径是可达的（写快速路径对拍时抓到）。
	if p.pos >= len(p.input) || p.input[p.pos] != '"' {
		return "", fmt.Errorf("期望字符串起始引号，位置 %d", p.pos)
	}
	p.pos++
	start := p.pos
	// 快路径：不含转义的字符串占绝大多数（角色、内容、模型名、id），直接切片返回，
	// 省掉 Builder 的成长分配与逐字节写入。实测 StringBuilder 两处合计占全服务
	// 累计分配的 25.6%。命中 `\` 时回到下面的慢路径重扫（转义字符串很少，多扫一遍划算）。
	for p.pos < len(p.input) {
		switch p.input[p.pos] {
		case '"':
			p.pos++
			return string(p.input[start : p.pos-1]), nil
		case '\\':
			p.pos = start
			return p.parseStringEscaped(start)
		default:
			p.pos++
		}
	}
	return "", fmt.Errorf("字符串未闭合")
}

// parseStringEscaped 是含转义的慢路径：从 start 起用 Builder 组装。
// measureEscapedLength 先扫一遍量出输出长度的**上界**（不组装文本），供调用方一次 Grow 到位。
//
// 为何要预先量而不是直接 Grow(剩余输入)：剩余输入确实是上界，但真实请求里一个含转义的
// 短字符串后面往往还跟着几十 KiB 正文——照它预分配会把这个短字符串的 Builder 一次撑到
// 整条正文大小（实测：一个 14 KiB 正文里 200 个短转义字符串，分配从 108 KiB 涨到 822 KiB）。
//
// 为何是上界而非精确值：Grow 只要求「不小于实际需求」（不足时 Builder 自己会增长，
// 正确性不受影响），而精确复算 \u 转义的 UTF-8 长度与代理对合成需要复制一段易错逻辑；
// 这里对 \u 一律按 4 字节计（单码点 UTF-8 最多 3 字节、代理对合成 4 字节），即为上界。
func (p *jsonParser) measureEscapedLength(start int) (int, error) {
	p.pos = start
	total := 0
	for p.pos < len(p.input) {
		switch c := p.input[p.pos]; c {
		case '"':
			p.pos++
			return total, nil
		case '\\':
			p.pos++
			if p.pos >= len(p.input) {
				return 0, fmt.Errorf("字符串转义意外结束")
			}
			switch escaped := p.input[p.pos]; escaped {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				total++
			case 'u':
				if p.pos+4 >= len(p.input) {
					return 0, fmt.Errorf("\\u 转义不完整")
				}
				for offset := 1; offset <= 4; offset++ {
					if !isHexDigit(p.input[p.pos+offset]) {
						return 0, fmt.Errorf("非法 \\u 转义")
					}
				}
				total += 4
				p.pos += 4
			default:
				return 0, fmt.Errorf("未知转义 \\%c", escaped)
			}
			p.pos++
		default:
			total++
			p.pos++
		}
	}
	return 0, fmt.Errorf("字符串未闭合")
}

func (p *jsonParser) parseStringEscaped(start int) (string, error) {
	// 先量一遍再 Grow：一次分配到位，避免 Builder 从 0 几何增长（总量约 2× 输出）。
	// 量出的是上界，不足时 Builder 也会自己增长，正确性不受影响；量的那一遍同时完成
	// 全部语法校验（与下面填充的一遍同判据），故填充时不会再遇到错误。
	length, err := p.measureEscapedLength(start)
	if err != nil {
		return "", err
	}
	p.pos = start
	var out strings.Builder
	out.Grow(length)
	for p.pos < len(p.input) {
		c := p.input[p.pos]
		switch c {
		case '"':
			p.pos++
			return out.String(), nil
		case '\\':
			p.pos++
			if p.pos >= len(p.input) {
				return "", fmt.Errorf("字符串转义意外结束")
			}
			escaped := p.input[p.pos]
			switch escaped {
			case '"':
				out.WriteByte('"')
			case '\\':
				out.WriteByte('\\')
			case '/':
				out.WriteByte('/')
			case 'b':
				out.WriteByte('\b')
			case 'f':
				out.WriteByte('\f')
			case 'n':
				out.WriteByte('\n')
			case 'r':
				out.WriteByte('\r')
			case 't':
				out.WriteByte('\t')
			case 'u':
				if p.pos+4 >= len(p.input) {
					return "", fmt.Errorf("\\u 转义不完整")
				}
				code, err := strconv.ParseUint(string(p.input[p.pos+1:p.pos+5]), 16, 32)
				if err != nil {
					return "", fmt.Errorf("非法 \\u 转义")
				}
				p.pos += 4
				if code >= 0xd800 && code <= 0xdbff {
					// 代理对高位：尝试与后随低位合并为一个码点。
					if p.pos+6 < len(p.input) && p.input[p.pos+1] == '\\' && p.input[p.pos+2] == 'u' {
						low, err := strconv.ParseUint(string(p.input[p.pos+3:p.pos+7]), 16, 32)
						if err == nil && low >= 0xdc00 && low <= 0xdfff {
							p.pos += 6
							out.WriteRune(rune(0x10000 + (code-0xd800)<<10 + (low - 0xdc00)))
							p.pos++
							continue
						}
					}
					out.WriteRune(utf8.RuneError)
				} else {
					out.WriteRune(rune(code))
				}
			default:
				return "", fmt.Errorf("未知转义 \\%c", escaped)
			}
			p.pos++
		default:
			out.WriteByte(c)
			p.pos++
		}
	}
	return "", fmt.Errorf("字符串未闭合")
}

func (p *jsonParser) parseArray() (*Value, error) {
	p.pos++ // [
	out := NewArray()
	p.skipWhitespace()
	if p.pos < len(p.input) && p.input[p.pos] == ']' {
		p.pos++
		return out, nil
	}
	for {
		p.skipWhitespace()
		item, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		out.items = append(out.items, item)
		p.skipWhitespace()
		if p.pos >= len(p.input) {
			return nil, fmt.Errorf("数组未闭合")
		}
		switch p.input[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return out, nil
		default:
			return nil, fmt.Errorf("数组内期望 , 或 ]，位置 %d", p.pos)
		}
	}
}

func (p *jsonParser) parseObject() (*Value, error) {
	p.pos++ // {
	out := NewObject()
	p.skipWhitespace()
	if p.pos < len(p.input) && p.input[p.pos] == '}' {
		p.pos++
		return out, nil
	}
	for {
		p.skipWhitespace()
		key, err := p.parseString()
		if err != nil {
			return nil, err
		}
		p.skipWhitespace()
		if p.pos >= len(p.input) || p.input[p.pos] != ':' {
			return nil, fmt.Errorf("对象内期望 :，位置 %d", p.pos)
		}
		p.pos++
		p.skipWhitespace()
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		out.Set(key, value)
		p.skipWhitespace()
		if p.pos >= len(p.input) {
			return nil, fmt.Errorf("对象未闭合")
		}
		switch p.input[p.pos] {
		case ',':
			p.pos++
		case '}':
			p.pos++
			return out, nil
		default:
			return nil, fmt.Errorf("对象内期望 , 或 }，位置 %d", p.pos)
		}
	}
}

func orNull(value *Value) *Value {
	if value == nil {
		return NewNull()
	}
	return value
}
