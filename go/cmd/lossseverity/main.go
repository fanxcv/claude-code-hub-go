// lossseverity 渲染界面侧的损失档位表（src/lib/utils/loss-severity.gen.ts）。
//
// 用途：界面侧要为库里不带 severity 字段的历史损失条目推导档位，而档位真源在
// go/internal/convert（LossSeverityOf）。本命令把那份真源原样渲染成 TS，避免两侧各存一份表。
//
// 刷新方式（在 go/ 目录下）：
//
//	go run ./cmd/lossseverity -out ../src/lib/utils/loss-severity.gen.ts
//
// 未经 -out 时写到标准输出，便于肉眼比对。
// 生成物必须提交入库（前端构建不依赖 Go 工具链）；它与真源是否一致由
// go/internal/convert 的 TestLossSeverityGeneratedTableIsUpToDate 钉住。
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

func main() {
	out := flag.String("out", "", "输出路径；留空写标准输出")
	flag.Parse()

	rendered := convert.RenderLossSeverityTS()

	if *out == "" {
		fmt.Print(rendered)
		return
	}

	if err := os.WriteFile(*out, []byte(rendered), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "写入 %s 失败: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Printf("已写入 %s（%d 字节）\n", *out, len(rendered))
}
