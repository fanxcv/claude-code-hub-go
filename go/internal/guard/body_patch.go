package guard

import (
	"bytes"
	"encoding/json"
	"errors"
	"hash/maphash"
	"slices"
	"strconv"
)

// 本文件承载「守卫链改写正文后只重写被改动的字节」。
//
// 为什么必须定点改：出站字节的稳定性直接决定上游前缀缓存的命中率。而 json.Marshal 会把整份
// 正文重新塑形——键序重排为字典序、`<` `>` `&` 转成 `\u003c` 这类更宽的转义（含比较运算符的
// 正文体积 +20% 以上）、数字经 float64 重排（`1.0`→`1`、`12345678901234567890` 丢精度）。
// 于是同一会话里「客户端自带 metadata.user_id」的请求不重排、「缺它被网关注入」的请求被重排，
// 上游看到两种字节前缀，缓存段整体失效。
//
// 三层：
//
//	scanBodyLayout    纯字节扫描定位原文里每个成员的字节区间（键、值、首尾花括号）。
//	digestBodyMembers 顶层成员值的语义指纹：键序、转义、数字写法都不影响结果，故「语义没变」的
//	                  成员可以原样发出——连 `1.0` 与 `12345678901234567890` 这种写法一并保住。
//	appendPatched...  按区间拼接：未改动的成员连逗号与空白一起照抄，改动的只换值，新增的追加在
//	                  末尾，删除的整段丢弃。
//
// 已知天花板：Store 拿到的是**已被就地改过**的树，没有旧树可比，所以一个成员只要变了就整体处理
// ——对象成员按键名与键序照原文逐子键替换（只重编码值），数组成员与标量成员整体重新序列化。
// 收敛性仍然成立：改一个顶层成员不会波及任何别的顶层成员，而守卫链默认只改顶层键
// （prompt_cache_key / metadata / input）。

// bodySpan 是原文里一个对象成员的字节位置。keyStart 指向键的开引号。
type bodySpan struct {
	key        string
	keyStart   int
	valueStart int
	valueEnd   int
}

// bodyLayout 是原文对象的结构：成员位置与首尾花括号的位置。
type bodyLayout struct {
	members    []bodySpan
	openEnd    int // '{' 之后的下标；原文 raw[:openEnd] 逐字保留
	closeStart int // '}' 的下标
}

// scanBodyLayout 扫出原文对象的成员位置。
//
// 纯字节扫描，不构造任何对象：json.Decoder 也能拿到同样区间（InputOffset 减解码长度），
// 但它要为每个值解一份 RawMessage，500 KB 正文实测 2.1 ms，比整份 marshal（0.9 ms）还贵
// ——这正是本次要消灭的开销。正文此处已被 json.Unmarshal 验证过是合法 JSON 对象，故扫描器只需
// 认同一个接受域：形状不符就报错，由调用方退回整份序列化。
func scanBodyLayout(raw []byte) (bodyLayout, error) {
	var layout bodyLayout
	cursor := skipBodySpace(raw, 0)
	if cursor >= len(raw) || raw[cursor] != '{' {
		return bodyLayout{}, errors.New("guard: 正文不是 JSON 对象")
	}
	layout.openEnd = cursor + 1
	cursor = skipBodySpace(raw, layout.openEnd)
	if cursor < len(raw) && raw[cursor] == '}' {
		layout.closeStart = cursor
		return layout, nil
	}
	for {
		keyStart := cursor
		if keyStart >= len(raw) || raw[keyStart] != '"' {
			return bodyLayout{}, errors.New("guard: 正文成员定位失败")
		}
		keyEnd, ok := skipJSONString(raw, keyStart)
		if !ok {
			return bodyLayout{}, errors.New("guard: 正文键读取失败")
		}
		var key string
		if err := json.Unmarshal(raw[keyStart:keyEnd], &key); err != nil {
			return bodyLayout{}, errors.New("guard: 正文键解不出")
		}
		colon := skipBodySpace(raw, keyEnd)
		if colon >= len(raw) || raw[colon] != ':' {
			return bodyLayout{}, errors.New("guard: 正文缺冒号")
		}
		valueStart := skipBodySpace(raw, colon+1)
		valueEnd, ok := skipJSONValueEnd(raw, valueStart)
		if !ok {
			return bodyLayout{}, errors.New("guard: 正文值读取失败")
		}
		layout.members = append(layout.members, bodySpan{
			key:        key,
			keyStart:   keyStart,
			valueStart: valueStart,
			valueEnd:   valueEnd,
		})
		cursor = skipBodySpace(raw, valueEnd)
		if cursor >= len(raw) {
			return bodyLayout{}, errors.New("guard: 正文结尾读取失败")
		}
		if raw[cursor] == ',' {
			cursor = skipBodySpace(raw, cursor+1)
			continue
		}
		if raw[cursor] != '}' {
			return bodyLayout{}, errors.New("guard: 正文结尾读取失败")
		}
		layout.closeStart = cursor
		return layout, nil
	}
}

// skipJSONString 返回以 raw[cursor]（开引号）开始的字符串之后的下标。
func skipJSONString(raw []byte, cursor int) (int, bool) {
	for cursor++; cursor < len(raw); cursor++ {
		switch raw[cursor] {
		case '\\':
			// 转义序列整体跳过：" \\ \/ \b \f \n \r \t \uXXXX 的被转义字符都不是 '"'。
			cursor++
		case '"':
			return cursor + 1, true
		}
	}
	return 0, false
}

// skipJSONValueEnd 返回从 raw[cursor] 开始的 JSON 值之后的下标。
//
// 容器只数**同种**括号、字符串整体跳过（`{"a":"}"}` 这类不会数错）；容器内部的结构不逐层校验，
// 正文合法与否已由解析期把关。
func skipJSONValueEnd(raw []byte, cursor int) (int, bool) {
	if cursor >= len(raw) {
		return 0, false
	}
	switch raw[cursor] {
	case '"':
		return skipJSONString(raw, cursor)
	case '{', '[':
		opener := raw[cursor]
		closer := byte('}')
		if opener == '[' {
			closer = ']'
		}
		depth := 0
		for ; cursor < len(raw); cursor++ {
			switch raw[cursor] {
			case '"':
				end, ok := skipJSONString(raw, cursor)
				if !ok {
					return 0, false
				}
				cursor = end - 1
			case opener:
				depth++
			case closer:
				depth--
				if depth == 0 {
					return cursor + 1, true
				}
			}
		}
		return 0, false
	default:
		// 数字 / true / false / null：吃到分隔符或结构字节为止。
		start := cursor
		for cursor < len(raw) && !isJSONValueStop(raw[cursor]) {
			cursor++
		}
		if cursor == start {
			return 0, false
		}
		return cursor, true
	}
}

// isJSONValueStop 报告该字节是否终结一个裸值（数字/字面量）。
func isJSONValueStop(b byte) bool {
	switch b {
	case ',', '}', ']', ' ', '\t', '\n', '\r':
		return true
	default:
		return false
	}
}

// member 按键名取成员位置。
func (l bodyLayout) member(key string) (bodySpan, bool) {
	for _, span := range l.members {
		if span.key == key {
			return span, true
		}
	}
	return bodySpan{}, false
}

// skipBodySpace 跳过 JSON 空白，返回下一个非空白字节的下标。
func skipBodySpace(raw []byte, cursor int) int {
	for cursor < len(raw) && isJSONSpace(raw[cursor]) {
		cursor++
	}
	return cursor
}

// isJSONSpace 报告该字节是否 JSON 空白。
func isJSONSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r':
		return true
	default:
		return false
	}
}

// bodyDigester 是值指纹的计算器；buf 是数字格式化的临时缓冲，整趟遍历复用。
//
// 用 maphash 而不是自建 FNV：同一条指纹链上的两次计算（解析期基线、Store 后重算）必须用同一个
// seed，故 seed 随指纹器走。maphash 走硬件加速且没有逐字节的乘法依赖链，实测比 FNV-1a 快一倍。
type bodyDigester struct {
	seed maphash.Seed
	buf  []byte
}

// newBodyDigester 造一个带独立 seed 的指纹器。
func newBodyDigester() *bodyDigester {
	return &bodyDigester{seed: maphash.MakeSeed()}
}

// digestBodyMembers 算出正文每个顶层成员的值指纹。
func (d *bodyDigester) digestBodyMembers(tree map[string]any) map[string]uint64 {
	digests := make(map[string]uint64, len(tree))
	for key, value := range tree {
		digests[key] = d.digest(value)
	}
	return digests
}

// digest 计算一个 JSON 值的语义指纹。
//
// 对象按「键值对的无序集合」合成（sum 与 xor 两个累加器），免得为排序分配键表：一次 35 KB
// 正文有上千个 map，逐 map 排序的开销与整趟指纹同阶。两个 64 位累加器给出 128 位强度，与指纹
// 本身的碰撞概率同阶；碰撞的后果只是把一个语义变了的成员当成没变而沿用原文。
func (d *bodyDigester) digest(value any) uint64 {
	switch typed := value.(type) {
	case nil:
		return mix64(bodyDigestSalt ^ 1)
	case bool:
		if typed {
			return mix64(bodyDigestSalt ^ 2)
		}
		return mix64(bodyDigestSalt ^ 3)
	case float64:
		d.buf = strconv.AppendFloat(d.buf[:0], typed, 'g', -1, 64)
		return mix64(maphash.Bytes(d.seed, d.buf))
	case string:
		return maphash.String(d.seed, typed)
	case json.Number:
		return maphash.String(d.seed, string(typed))
	case []any:
		digest := uint64(bodyDigestSalt) ^ 7
		for _, item := range typed {
			digest = mix64(digest ^ d.digest(item))
		}
		return digest
	case map[string]any:
		var sum, xor uint64
		for key, item := range typed {
			member := mix64(maphash.String(d.seed, key) ^ d.digest(item))
			sum += member
			xor ^= member
		}
		return mix64(sum ^ mix64(xor) ^ bodyDigestSalt)
	default:
		// 守卫链一般不产生表外类型；真出现了就退回标准序列化，语义仍正确。
		encoded, err := json.Marshal(value)
		if err != nil {
			return mix64(bodyDigestSalt ^ 9)
		}
		return maphash.Bytes(d.seed, encoded)
	}
}

// bodyDigestSalt 是各类型标签的盐值，防止不同形状算出同一条指纹。
const bodyDigestSalt = 14695981039346656037

// mix64 是 splitmix64 的收尾混合：把结构相近的输入打散。
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// appendPatchedObject 把 tree 写进 dst：原文里语义未变的成员连逗号与空白照抄，变了的只重写值。
//
// changed 回答「这个成员的当前值是否可能已变」；tree 里没有的成员视为删除。删除会让相邻分隔符
// 归一为一个裸逗号（原文空白只在没有删除时逐字保留）。
func appendPatchedObject(digester *bodyDigester, dst, raw []byte, layout bodyLayout, tree map[string]any, changed func(key string) bool) ([]byte, error) {
	dst = append(dst, raw[:layout.openEnd]...)
	emitted := false // 已输出过至少一个成员
	dropped := false // 自上次输出以来丢过成员（原文分隔符含被删成员的字节，不能照抄）
	previousEnd := -1
	for _, span := range layout.members {
		value, exists := tree[span.key]
		if !exists {
			dropped = true
			continue
		}
		switch {
		case emitted && dropped:
			dst = append(dst, ',')
		case emitted:
			dst = append(dst, raw[previousEnd:span.keyStart]...)
		case !dropped:
			dst = append(dst, raw[layout.openEnd:span.keyStart]...)
		}
		dst = append(dst, raw[span.keyStart:span.valueStart]...)
		if changed(span.key) {
			patched, err := appendBodyValue(digester, dst, raw[span.valueStart:span.valueEnd], value)
			if err != nil {
				return nil, err
			}
			dst = patched
		} else {
			dst = append(dst, raw[span.valueStart:span.valueEnd]...)
		}
		emitted = true
		dropped = false
		previousEnd = span.valueEnd
	}

	// 尾部起点：最后一个成员之后没丢过东西时，那里只剩空白与 '}'，从该处照抄能连原文空白一起
	// 保住（新追加的键就落在空白之前）。丢过成员则只能从 '}' 起抄，免得把被删成员的字节带出来。
	tailStart := layout.closeStart
	if emitted && !dropped {
		tailStart = previousEnd
	}
	for _, key := range appendedKeys(layout, tree) {
		if emitted {
			dst = append(dst, ',')
		}
		dst = append(dst, '"')
		dst = appendJSONString(dst, key)
		dst = append(dst, '"', ':')
		patched, err := appendBodyValue(digester, dst, nil, tree[key])
		if err != nil {
			return nil, err
		}
		dst = patched
		emitted = true
	}
	return append(dst, raw[tailStart:]...), nil
}

// appendedKeys 列出原文里没有、tree 里有的顶层键（排序以保证出站字节稳定）。
//
// 用线性比对而不是建键集合：顶层成员数通常只有个位数到几十，省下的 map 分配比那点比较值钱。
// 注意不能按「len(tree) 与成员数相等」短路——删一个键再加一个键时两者相等，但确实有新增。
func appendedKeys(layout bodyLayout, tree map[string]any) []string {
	var keys []string
	for key := range tree {
		if _, ok := layout.member(key); ok {
			continue
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// appendBodyValue 输出一个改写后的值；original 是这一处在原文里的字节区间（新增的键传 nil）。
//
// 原文这一处是对象且新值也是对象时，就在原文上逐子键替换：子键的基线由这一处的原文现场解出
// （顶层那一路已有解析期基线，不走这里），所以兄弟子键只要语义没变就逐字保留——改
// metadata.user_id 不会把 metadata 里的大整数或 `<` 一起重编码。其余形态（数组、标量、
// 形状不符）整体序列化。
func appendBodyValue(digester *bodyDigester, dst, original []byte, value any) ([]byte, error) {
	object, isObject := value.(map[string]any)
	if !isObject || len(original) == 0 {
		return appendEncodedJSON(dst, value)
	}
	layout, layoutErr := scanBodyLayout(original)
	if layoutErr != nil {
		return appendEncodedJSON(dst, value)
	}
	patched, patchErr := appendPatchedObject(digester, dst, original, layout, object, changedInOriginal(digester, original, object))
	if patchErr != nil {
		return appendEncodedJSON(dst, value)
	}
	return patched, nil
}

// changedInOriginal 用原文解出的基线判断各子键是否被改动。原文解不出来时一律按「改过」处理：
// 宁可多编码，不可漏改动。
func changedInOriginal(digester *bodyDigester, original []byte, current map[string]any) func(key string) bool {
	var before map[string]any
	if err := json.Unmarshal(original, &before); err != nil {
		return func(string) bool { return true }
	}
	baseline := digester.digestBodyMembers(before)
	return func(key string) bool {
		value, ok := current[key]
		if !ok {
			return false
		}
		expected, known := baseline[key]
		return !known || digester.digest(value) != expected
	}
}

// appendEncodedJSON 把值整体序列化后拼到 dst 末尾。
//
// 关掉 HTML 转义：上游看到的是客户端方言，`<` `>` `&` 应当原样出去（与 JS JSON.stringify 一致），
// 而不是变宽成 \u003c 一类。
func appendEncodedJSON(dst []byte, value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, errors.New("guard: 正文序列化失败: " + err.Error())
	}
	return append(dst, bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))...), nil
}

// appendJSONString 把键名按 JSON 字符串写进 dst（最小转义）。
func appendJSONString(dst []byte, s string) []byte {
	const hex = "0123456789abcdef"
	for i := 0; i < len(s); i++ {
		switch b := s[i]; b {
		case '"', '\\':
			dst = append(dst, '\\', b)
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			if b < 0x20 {
				dst = append(dst, '\\', 'u', '0', '0', hex[b>>4], hex[b&0xf])
				continue
			}
			dst = append(dst, b)
		}
	}
	return dst
}
