// codex-router 是原 Node + LiteLLM 栈的 Go 单二进制重写。
// 命令实现与分发在 internal/app/cli（CLI 也是对外表面，归 app 层）；
// 这里只剩版本注入与进程入口。
package main

import (
	"github.com/loyd/codex-router/internal/app/cli"
)

// version 在构建时通过 -ldflags 注入（-X main.version 打在 package
// main 上 —— cli.Main 以参数接收，包内不设全局）。
var version = "dev"

func main() {
	cli.Main(version)
}
