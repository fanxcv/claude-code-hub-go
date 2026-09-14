package egress

// Owner 表示一次请求由谁承载。
//
// 它现在是 **`internal/pctx` 的取值契约**（`Context.SetOwner` 会校验 owner 必须取自本类型的
// 两个具名值），而不是归属仲裁的结果——本进程承载全部路径，归属判定已随 Node 回退缝一并删除
type Owner string

const (
	// OwnerGo 表示由本进程承载。数据面与模型面在建管线上下文时恒为此值
	// （`internal/dataplane/dataplane.go`、`internal/dataplane/models.go`）。
	OwnerGo Owner = "go"
	// OwnerNode 表示由 Node 后端承载。
	//
	// **已无生产者**：cchd 不再有任何「把请求交给 Node」的能力——反代实现、回退目标
	// （`CCH_INTERNAL_PORT`）、双跑模板与前门回退处理器均已删除。本常量只作为 pctx 取值
	// 契约的词汇保留；若将来要删它，须同时改 `internal/pctx` 的 `SetOwner` 校验与
	// `internal/pctx` 的测试。
	OwnerNode Owner = "node"
)
