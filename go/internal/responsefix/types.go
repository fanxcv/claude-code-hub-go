package responsefix

// Result 逐字对应 Node 的 `FixResult<T>`：修复结果 + 是否真的改过 + 细节串。
//
// Details 只在修复器给出理由时非空（Node 侧是 `details?: string`，undefined 不会进审计
// JSON；Go 侧用空串表示同一件事，转审计条目时省略空串键——见 auditEntry）。
type Result struct {
	Data    []byte
	Applied bool
	Details string
}

// 各修复器写入 Details 的取值，与 Node 逐字一致（审计条目按原文比对）。
const (
	detailRemovedUTF8BOM          = "removed_utf8_bom"
	detailRemovedUTF16BOM         = "removed_utf16_bom"
	detailRemovedNullBytes        = "removed_null_bytes"
	detailLossyUTF8DecodeEncode   = "lossy_utf8_decode_encode"
	detailExceededMaxSize         = "exceeded_max_size"
	detailRepairFailed            = "repair_failed"
	detailValidateRepairedFailed  = "validate_repaired_failed"
	detailFilteredInertCompletion = "filtered_inert_chat_completion_chunk"
)
