package adminapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 lane A1-5（sensitive-words / model-prices）两个资源模块共用的解析与作答工具。
//
// 为什么不做成公共包：批次 A 的五个 lane 同属 adminapi 包，而共享文件在 A0 已冻结；这些工具
// 只服务本 lane 的两个模块，放自己的文件里最省事，也不给别的 lane 制造耦合。
//
// 命名约定：本文件所有标识符都带 lane 前缀（adminXxx / sensitiveWordXxx / modelPriceXxx）。
// 五个 lane 并行往同包加文件，`writeJSON`、`parseBody` 这类自然名一定会撞符号。

// invalidParam 复刻 zod issue 的应答形状（error-envelope.ts:12-16）。
type invalidParam struct {
	Path    []any  `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// adminValidationBody 复刻 fromZodError / parseHonoJsonBody 失败分支的 400 信封。
//
// 为什么不复用 Deps.Problems：ProblemWriter（冻结面）只表达 status/errorCode/detail，表达不了
// title "Validation failed" 与 invalidParams。校验失败是**唯一的**需要这两个字段的路径，
// 故在本 lane 内自答，其余错误一律仍走 ProblemWriter。
type adminValidationBody struct {
	Type          string         `json:"type"`
	Title         string         `json:"title"`
	Status        int            `json:"status"`
	Detail        string         `json:"detail"`
	Instance      string         `json:"instance"`
	ErrorCode     string         `json:"errorCode"`
	InvalidParams []invalidParam `json:"invalidParams"`
}

// adminProblemWriter 取 ProblemWriter；未装配时用默认实现（与 shell.go 同法）。
func adminProblemWriter(deps Deps) ProblemWriter {
	if deps.Problems != nil {
		return deps.Problems
	}
	return NewProblems(deps.Logger)
}

// adminWriteJSON 作答 JSON 正文。
//
// 两处与写好 JSON 有关的细节都要对：Content-Type 逐字为 "application/json"（Node 的
// jsonResponse 不带 charset），且**不转义 HTML**——Go 的 json.Marshal 会把 < > & 写成 \u003c，
// 而 Node 的 JSON.stringify 不转，价格表里的模型名与描述可能含这些字符。Encoder 会补一个换行，
// 故裁掉。
func adminWriteJSON(writer http.ResponseWriter, status int, body any) {
	payload, err := adminMarshalJSON(body)
	if err != nil {
		// 正文都是定长结构体与从库里来的 JSON，序列化失败说明代码被改坏了。
		http.Error(writer, "", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(payload)
}

// adminWriteCreated 复刻 createdResponse：201 + Location + JSON 正文。
func adminWriteCreated(writer http.ResponseWriter, location string, body any) {
	writer.Header().Set("Location", location)
	adminWriteJSON(writer, http.StatusCreated, body)
}

// adminWriteNoContent 复刻 noContentResponse：204，无正文、无 Content-Type。
func adminWriteNoContent(writer http.ResponseWriter) {
	writer.WriteHeader(http.StatusNoContent)
}

func adminMarshalJSON(body any) ([]byte, error) {
	buffer := &bytes.Buffer{}
	encoder := json.NewEncoder(buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(body); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}

// adminWriteValidationFailure 按 zod 失败的信封作答。
//
// 差异（登记进对拍白名单）：invalidParams[].message 是我们自己的英文文案，不是 zod v4 的原文；
// 本仓库用的是 zod 4.4.3，其文案（"Invalid input: expected string, received number" 等）与本
// 文件的措辞不同。path 与 code 尽力对齐。
func adminWriteValidationFailure(
	writer http.ResponseWriter,
	request *http.Request,
	issues []invalidParam,
) {
	if issues == nil {
		issues = []invalidParam{}
	}
	payload, err := adminMarshalJSON(adminValidationBody{
		Type:          problemType("request.validation_failed"),
		Title:         "Validation failed",
		Status:        http.StatusBadRequest,
		Detail:        "One or more fields are invalid.",
		Instance:      problemInstance(request),
		ErrorCode:     "request.validation_failed",
		InvalidParams: issues,
	})
	if err != nil {
		http.Error(writer, "", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", problemContentType)
	writer.WriteHeader(http.StatusBadRequest)
	_, _ = writer.Write(payload)
}

// adminReadJSONObject 复刻 parseHonoJsonBody 的 Content-Type 与 JSON 解析两段。
//
// 返回 (对象, true) 或（已作答时的 nil, false）。第三段校验由各资源模块自己按 schema 做。
func adminReadJSONObject(
	writer http.ResponseWriter,
	request *http.Request,
	deps Deps,
) (map[string]json.RawMessage, bool) {
	contentType := request.Header.Get("Content-Type")
	if !strings.Contains(strings.ToLower(contentType), "application/json") {
		adminProblemWriter(deps).WriteProblem(writer, request, http.StatusUnsupportedMediaType, "",
			"Request body must use application/json.")
		return nil, false
	}

	raw, err := io.ReadAll(request.Body)
	if err != nil {
		adminProblemWriter(deps).WriteProblem(writer, request, http.StatusBadRequest,
			"request.malformed_json", "Request body is not valid JSON.")
		return nil, false
	}
	// 顶层必须是 JSON 对象：json.Unmarshal 对 `{}` 给出非 nil 空 map，对 `null` 给出 nil map，
	// 故用「trim 后首字符是 {」把顶层 null/数组/标量与「空对象」分开——前者在 Node 侧也是
	// 校验失败（Expected object, received null），只是它属于 schema 校验而不是 JSON 解析错误。
	if trimmed := strings.TrimSpace(string(raw)); !strings.HasPrefix(trimmed, "{") {
		adminWriteValidationFailure(writer, request, []invalidParam{{
			Code:    "invalid_type",
			Message: adminTypeMessage("object", json.RawMessage(trimmed)),
		}})
		return nil, false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		adminProblemWriter(deps).WriteProblem(writer, request, http.StatusBadRequest,
			"request.malformed_json", "Request body is not valid JSON.")
		return nil, false
	}
	if object == nil {
		object = map[string]json.RawMessage{}
	}
	return object, true
}

// adminObject 是严格模式（zod `.strict()`）下的对象校验器。
//
// 只实现本 lane 五个 schema 真正用到的能力（string / bool / number / stringArray / unknownKeys），
// 不做通用 zod——通用实现是另一个量级的东西，而这里只有五张表。
type adminObject struct {
	fields  map[string]json.RawMessage
	issues  []invalidParam
	allowed map[string]struct{}
}

func adminNewObject(fields map[string]json.RawMessage, allowed ...string) *adminObject {
	set := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		set[key] = struct{}{}
	}
	return &adminObject{fields: fields, allowed: set}
}

// issues0 返回累加的 issue 列表（nil 表示通过）。
func (o *adminObject) issues0() []invalidParam { return o.issues }

func (o *adminObject) fail(path []any, code, message string) {
	o.issues = append(o.issues, invalidParam{Path: path, Code: code, Message: message})
}

// RejectUnknownKeys 复刻 zod `.strict()`：出现未声明的键即 400。
//
// zod 会对每个未声明的键各报一条 unrecognized_keys，且把键名写进 message。
func (o *adminObject) RejectUnknownKeys() {
	for key := range o.fields {
		if _, ok := o.allowed[key]; !ok {
			o.fail(nil, "unrecognized_keys", fmt.Sprintf("Unrecognized key(s) in object: '%s'", key))
		}
	}
}

// adminStringSpec 描述一个字符串字段的约束（对应 zod 的一条链）。
type adminStringSpec struct {
	Required bool
	// Trim 对应 zod 的 .trim()：先 trim 再校验长度。
	Trim bool
	// MinRunes / MaxRunes 为 0 表示不检。
	MinRunes int
	MaxRunes int
	// Enum 非空时取值必须落在其中。
	Enum []string
	// SingleLine 保留：本 lane 的字符串字段都是短串，无多行语义。
}

// String 读一个字符串字段；返回 (值, 是否出现)。
//
// 未出现且非必填时返回 ("", false)，调用方据此决定「不写这一列」（Node 侧 undefined 语义）。
func (o *adminObject) String(key string, spec adminStringSpec) (string, bool) {
	raw, present := o.fields[key]
	if !present {
		if spec.Required {
			o.fail([]any{key}, "invalid_type", "Required")
		}
		return "", false
	}
	if adminJSONTypeName(raw) != "string" {
		// 必须显式判类型：json.Unmarshal 把 null 解进 string/bool/number 时是**静默无操作**，
		// 不报错也不改值。若只看 error，`"description": null` 会被当成空串放行——而 zod 的
		// `.optional()` 不接受 null（它只放行 undefined）。
		o.fail([]any{key}, "invalid_type", adminTypeMessage("string", raw))
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		o.fail([]any{key}, "invalid_type", adminTypeMessage("string", raw))
		return "", false
	}
	if spec.Trim {
		value = strings.TrimSpace(value)
	}
	if len(spec.Enum) > 0 {
		matched := false
		for _, candidate := range spec.Enum {
			if value == candidate {
				matched = true
				break
			}
		}
		if !matched {
			o.fail([]any{key}, "invalid_enum_value",
				fmt.Sprintf("Invalid enum value. Expected %s, received '%s'",
					adminEnumList(spec.Enum), value))
			return value, true
		}
	}
	if spec.MinRunes > 0 && len([]rune(value)) < spec.MinRunes {
		o.fail([]any{key}, "too_small",
			fmt.Sprintf("String must contain at least %d character(s)", spec.MinRunes))
		return value, true
	}
	if spec.MaxRunes > 0 && len([]rune(value)) > spec.MaxRunes {
		o.fail([]any{key}, "too_big",
			fmt.Sprintf("String must contain at most %d character(s)", spec.MaxRunes))
		return value, true
	}
	return value, true
}

// Bool 读一个布尔字段。取值必须是 JSON 的 true/false（zod 不做强制转换）。
func (o *adminObject) Bool(key string) (*bool, bool) {
	raw, present := o.fields[key]
	if !present {
		return nil, false
	}
	if adminJSONTypeName(raw) != "boolean" {
		o.fail([]any{key}, "invalid_type", adminTypeMessage("boolean", raw))
		return nil, false
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		o.fail([]any{key}, "invalid_type", adminTypeMessage("boolean", raw))
		return nil, false
	}
	return &value, true
}

// Number 读一个「非负有限数」字段（复刻 NonNegativePriceSchema：.number().min(0).finite()）。
func (o *adminObject) Number(key string, required bool, path []any) (*float64, bool) {
	if path == nil {
		path = []any{key}
	}
	raw, present := o.fields[key]
	if !present {
		if required {
			o.fail(path, "invalid_type", "Required")
		}
		return nil, false
	}
	if adminJSONTypeName(raw) != "number" {
		o.fail(path, "invalid_type", adminTypeMessage("number", raw))
		return nil, false
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		o.fail(path, "invalid_type", adminTypeMessage("number", raw))
		return nil, false
	}
	if value < 0 {
		o.fail(path, "too_small", "Number must be greater than or equal to 0")
		return &value, true
	}
	return &value, true
}

// StringArray 读一个字符串数组字段（overwriteManual 与别名列表用）。
//
// 与 zod 一致：元素必须是字符串，数组本身不可为 null。
func (o *adminObject) StringArray(key string) ([]string, bool) {
	raw, present := o.fields[key]
	if !present {
		return nil, false
	}
	if adminJSONTypeName(raw) != "array" {
		o.fail([]any{key}, "invalid_type", adminTypeMessage("array", raw))
		return nil, false
	}
	var items []string
	if err := json.Unmarshal(raw, &items); err != nil {
		o.fail([]any{key}, "invalid_type", adminTypeMessage("array", raw))
		return nil, false
	}
	return items, true
}

// Raw 取一个字段的原始 JSON（透传给价格数据用）。
func (o *adminObject) Raw(key string) (json.RawMessage, bool) {
	raw, present := o.fields[key]
	return raw, present
}

// adminTypeMessage 复刻 zod 的 invalid_type 文案前半段：描述实际收到的 JSON 类型。
func adminTypeMessage(expected string, raw json.RawMessage) string {
	return fmt.Sprintf("Expected %s, received %s", expected, adminJSONTypeName(raw))
}

// adminJSONTypeName 给出值在 zod 眼里的类型名。
func adminJSONTypeName(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case trimmed == "":
		return "undefined"
	case trimmed == "null":
		return "null"
	case trimmed == "true" || trimmed == "false":
		return "boolean"
	case strings.HasPrefix(trimmed, "["):
		return "array"
	case strings.HasPrefix(trimmed, "{"):
		return "object"
	case strings.HasPrefix(trimmed, "\""):
		return "string"
	default:
		return "number"
	}
}

func adminEnumList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, "'"+value+"'")
	}
	return strings.Join(quoted, " | ")
}

// adminCoerceInt 复刻 zod 的 z.coerce.number().int()：字符串先按 JS 的 Number() 语义转数，
// 再要求是整数。空串在 JS 里是 0（再由 min/max 拦下），故这里也返回 0 而非错误——由调用方
// 的取值范围决定成败，这样 page="" 与 page=0 的行为与 Node 完全一致。
//
// 差异（登记进白名单）：JS 的 Number 接受 "0x10"/"1e2" 等写法，这里只认十进制与指数（strconv）。
func adminCoerceInt(value string) (int, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, true
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, false
	}
	// zod 的 .int() 要求 Number.isInteger：1.5 不是整数，NaN/Inf 也不是。
	if parsed != float64(int64(parsed)) && parsed != 0 {
		return 0, false
	}
	return int(parsed), true
}

// adminAuditEvent 组装审计事件的公共部分（操作人、IP、UA）。
//
// pools 只用于解析审计 IP 的可配置提取链（audit_ip.go）；未装配时用默认链。Node 侧这一步
// 由 action 适配器完成（src/lib/api/v1/_shared/request-context.ts:10 的 getClientIp）。
func adminAuditEvent(
	pools *store.Pools,
	request *http.Request,
	action, targetType, targetID, targetName string,
) AuditEvent {
	principal, _ := PrincipalFrom(request.Context())
	return AuditEvent{
		Principal:  principal,
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		TargetName: targetName,
		IP:         auditClientIP(request.Context(), pools, request),
		UserAgent:  request.Header.Get("User-Agent"),
	}
}

// adminEmitAudit 发一条审计；Audit 未装配时是空操作。
func adminEmitAudit(deps Deps, request *http.Request, event AuditEvent) {
	if deps.Audit == nil {
		return
	}
	deps.Audit.Emit(request.Context(), event)
}

// adminPublishDomain 广播配置域失效；Invalidator 未装配时是空操作。
func adminPublishDomain(deps Deps, request *http.Request, domain cfgsync.Domain) {
	if deps.Invalidator == nil {
		return
	}
	deps.Invalidator.PublishDomain(request.Context(), domain)
}

// adminActionFailure 把存储层/业务层失败映射成 action 错误。
//
// 状态码为什么是 400 而不是 500：Node 侧 action 的 catch 分支返回错误文案，handler 只按
// 「不存在」/「权限」两个子串判 404/403，其余一律 400——数据库故障在 Node 侧也是 400。
// 为了与 Node 同形，这里保持一致（宁可形状相同，也不擅自"更正确"地给 500）。
func adminActionFailure(resource string, err error) *ActionError {
	return NewActionError(resource, resource+".action_failed", http.StatusBadRequest, err)
}

// adminNowMillis 是 JS Date.now()：毫秒时间戳（cache 统计里的 lastReloadTime 用它）。
func adminNowMillis() int64 { return time.Now().UnixMilli() }

// adminNowISO 产出 JS `new Date().toISOString()` 形状的当前时间（毫秒、UTC、固定 24 字符）。
//
// 用途：列可空时的兜底（Node 侧 `?? new Date()`）。名字带 admin 前缀，避免与并行 lane 的同名
// 工具撞符号。
func adminNowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// adminStringOrNow 复刻 `value ?? new Date()`：可空时间列取兜底。
func adminStringOrNow(value *string) string {
	if value == nil || *value == "" {
		return adminNowISO()
	}
	return *value
}

// adminStringOrEmpty 复刻 `value ?? ""` 一类的兜底（目录项的 updatedAt 用）。
func adminStringOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
