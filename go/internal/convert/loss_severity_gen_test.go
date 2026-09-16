package convert

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestLossSeverityGeneratedTableIsUpToDate 钉住界面侧的档位表与 Go 真源一致。
//
// 为何需要这条钉子：界面要为库里**历史**损失条目（没有 severity 字段）推导档位，用的是生成物
// （src/lib/utils/loss-severity.gen.ts）。两份表曾经手工各写一份，分叉的后果是**该显示的徽章
// 整枚不画**——真损失被降噪吞掉，而没有徽章的行用户永远不会去查（2026-09-16 生产实证）。
//
// 本用例只做一件事：把生成物与 RenderLossSeverityTS 逐字节比对。故「新增一个非 rewrite 档能力
// 却忘记重新生成」会当场转红，且失败信息给出确切的重新生成命令。
func TestLossSeverityGeneratedTableIsUpToDate(t *testing.T) {
	path := filepath.Join("..", "..", "..", filepath.FromSlash(LossSeverityGeneratedPath))

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到界面侧档位表 %s：%v\n本钉子要求整仓检出（生成物在前端目录下）", path, err)
	}

	want := RenderLossSeverityTS()
	if string(onDisk) == want {
		return
	}

	t.Fatalf("界面侧档位表与 Go 真源不一致，请重新生成：\n\tcd go && go run ./cmd/lossseverity -out ../%s\n\n%s",
		LossSeverityGeneratedPath, firstLineDifference(string(onDisk), want))
}

// firstLineDifference 报出第一处不同的行（含行号与两侧内容），只用于让失败信息可定位。
func firstLineDifference(got string, want string) string {
	gotLines := strings.Split(got, "\n")
	wantLines := strings.Split(want, "\n")

	for index := 0; index < len(gotLines) || index < len(wantLines); index++ {
		var gotLine, wantLine string
		if index < len(gotLines) {
			gotLine = gotLines[index]
		}
		if index < len(wantLines) {
			wantLine = wantLines[index]
		}
		if gotLine != wantLine {
			return "首个差异在第 " + strconv.Itoa(index+1) + " 行：\n  文件里：" + gotLine + "\n  应为　：" + wantLine
		}
	}

	return "逐行相同（差异只可能在行尾）"
}
