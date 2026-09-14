// cchd 是 CCH 的 Go 数据面进程。
//
// 进程职责：独占公共 PORT，对进入 `/v1`、`/v1beta` 的请求做一次性归属判定——判给 Go 的
// 交给本进程的数据面处理器，判给 Node 的原样反代到 127.0.0.1:CCH_INTERNAL_PORT；
// 探针 `/readyz`、`/v1/_ping` 不经前门，冷启动期也照答（`/readyz` 报 503 说明未就绪）。
//
// 装配与关闭流程见 boot.go，规则订阅与就绪门见 rules.go。
//
// 退出码：0 为正常关闭；1 为启动失败、服务异常或排空超时（在途请求被强退属审计事件，
// 不能让编排层误以为一切正常）。
package main

import (
	"context"
	"fmt"
	"os"
)

func main() {
	if err := runWith(context.Background(), newStartup()); err != nil {
		fmt.Fprintf(os.Stderr, "cchd 退出：%v\n", err)
		os.Exit(1)
	}
}
