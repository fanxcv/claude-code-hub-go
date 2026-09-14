// envlist 生成 go/env-parity.txt：Go 侧受契约覆盖的环境变量清单。
//
// 用途：scripts/check-env-parity.mjs 用它和 tests/load/env-parity/env-matrix.json 对账，
// 回答「矩阵里有、Go 侧没实现」与「Go 侧多配」两类分叉。
//
// 清单由 go/internal/config 的规格表直接导出，因此不可能与装载器分叉。
// 刷新方式（在 go/ 目录下）：
//
//	go run ./cmd/envlist
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
)

const header = `# Go 侧受 env.schema.ts 契约覆盖的环境变量清单（由 go run ./cmd/envlist 生成，勿手工编辑）。
#
# 契约真源：tests/load/env-parity/env-matrix.json（由 scripts/export-env-matrix.ts 导出）。
# 对账命令：node scripts/check-env-parity.mjs --strict
#
# 本清单只含 TS 侧契约里的变量；Go 专有变量（CCH_EGRESS_PAGES、CCH_GO_MAX_INFLIGHT_BYTES、
# CCH_GO_MAX_STREAMS、GOMEMLIMIT）不在此列，否则会被对账脚本判为「多配」。
#
# 历史：CCH_INTERNAL_PORT / CCH_EGRESS_MODE / CCH_EGRESS_ROUTES 三条 Go 专有变量已随 Node
# 回退缝删除（进程不再解析它们）。
`

func main() {
	out := flag.String("out", "env-parity.txt", "输出路径（相对当前目录）")
	flag.Parse()

	names := config.ParityVariableNames()
	var builder strings.Builder
	builder.WriteString(header)
	for _, name := range names {
		builder.WriteString(name)
		builder.WriteString("\n")
	}

	if err := os.WriteFile(*out, []byte(builder.String()), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "写入 %s 失败: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Printf("已写入 %s：%d 项\n", *out, len(names))
}
