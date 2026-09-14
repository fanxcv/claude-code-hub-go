package dataplane

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// 本文件是**源码结构性钉子**：断言「供应商自定义头」真的从库存行接到了转发投影上。
//
// 为什么必须有它：这条链的三段各自都有测试——
//   - 列读取（store.FindEnabledProviders 走 `SELECT *`，字段有 json 标签即被填充）；
//   - 容错解码（store.DecodeCustomHeaders 有单测）；
//   - 施加与剥离（forward.BuildUpstreamHeaders 的覆盖顺序用例已覆盖自定义头、鉴权覆盖、
//     保留名与内部头剥离）。
//
// 但「dataplane 到底有没有把 row.CustomHeaders 填进 forward.Provider」这一件事谁都盖不住：
// 摘掉这一行时，上面三组用例**全绿**（实测），功能却静默失效——库里有配置、界面上显示已配置、
// 出站请求上什么都没有。这正是「测试全绿但功能没接」的盲区，故用源码发现钉死。
var customHeadersWiring = regexp.MustCompile(
	`(?m)^\s*CustomHeaders:\s+store\.DecodeCustomHeaders\(row\.CustomHeaders\),\s*$`)

func TestUpstreamWiringPropagatesProviderCustomHeaders(t *testing.T) {
	path := filepath.Join("upstream.go")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", path, err)
	}
	if !customHeadersWiring.Match(source) {
		t.Fatalf("upstream.go 里找不到「row.CustomHeaders → forward.Provider.CustomHeaders」的接线：\n" +
			"  期望形如 `CustomHeaders: store.DecodeCustomHeaders(row.CustomHeaders),`\n" +
			"  该行缺失时，供应商自定义头在出站请求上静默失效（三段单测都不会红）。")
	}
}
