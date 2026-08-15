# Model Router

> 本仓库是 [duolahypercho/codex-router](https://github.com/duolahypercho/codex-router)
> 的 **Go 重写版（fork）**：砍掉 Node.js 运行时与 LiteLLM Python 网关，
> 换成**单个自包含 Go 二进制 + 一个原生 macOS App**。原项目的完整说明与
> 历史见 `git log -- README.md` 与 `CHANGELOG.md`；归属说明见 `NOTICE.md`。
> 设计基线与迁移记录见 [GO-REWRITE-PLAN.md](GO-REWRITE-PLAN.md)。

把 Codex（App 与 CLI）接到你自己的模型订阅上：**Z.ai GLM Coding Plan**、
**opencode Go/Zen**，外加**原生 ChatGPT 订阅直通**。路由器以 Responses API
的身份站在 Codex 背后，把外部模型混入 Codex 原生模型选择器——你在同一个
picker 里选 `zai-coding/glm-5.3` 和 `gpt-5.6-sol`。

```
Codex ──► 127.0.0.1:4202/_codex-router/<caller-key>/v1/responses
            │  （URL 路径内嵌 key，即本地认证的全部）
            ├─ 注册的 slug（zai-coding/…、opencode-go/…）──► 路由路径
            │     wire 协议层：Responses ↔ chat-completions 翻译
            │     或 Responses 直通（provider 注册表声明协议）
            └─ 未注册 slug（gpt-5.x）──► 原生路径（ChatGPT 订阅直通）
```

## 形态：一个 App，一个二进制

- **`Model Router.app`**（macOS）：Dock 图标 + 主窗口 + 菜单栏速览。
  Go 服务二进制**内嵌在 App bundle 里**，作为托管子进程运行——
  **打开 App 服务起，退出 App 服务停**（3 秒优雅排空后 SIGKILL，无孤儿进程）；
  红叉只关窗，服务继续跑。开机自启 = 设置页「登录时启动」开关。
- **`codex-router` CLI**：同一个二进制的命令行形态（`serve` / `install` /
  `doctor` / `control` / `shim`）。放在 `~/bin` 即可与 App 并存
  （端口唯一，后启动者退出）。
- 注册表（provider/模型定义）`go:embed` 进二进制——单文件部署，
  不需要旁边的 config 目录。

## 快速开始

前置：macOS、Go 1.22+、Xcode command line tools（构建 App 用）。

```sh
# 1. 构建 App（自动把 Go 二进制编进 bundle）
./scripts/build-macos-tray-app.sh
open ~/Applications/"Model Router.app"      # 服务随 App 启动

# 2. 发布到 Codex（写 config.toml 标记块 + 模型 catalog，幂等）
./codex-router install

# 3. 填凭证（见下一节），然后体检
./codex-router doctor
```

完全退出并重开 Codex，新建任务，picker 里选路由模型。

## 凭证：`~/.codex-router/config.toml`

Claude Code 式的配置文件，改完**下一回合请求即生效**，无需重启：

```toml
[zai-coding]
api_key = "sk-..."

[opencode-go]
api_key = "..."
# 也支持环境变量引用：api_key = "{ZAI_API_KEY}"
# 未设置的变量展开为空（=未配置）；{非变量形状} 按字面量保留
```

- 生成带注释模板：`./codex-router control config init`
- App 设置页「凭证配置 → 打开配置」直达编辑
- 解析顺序：**环境变量 > config.toml > `*.secret` 文件 > macOS Keychain**
  （后两者是原项目的历史来源，保留兼容）
- 命令行写入（隐藏输入）：`./codex-router control credential zai-coding`
- 坏文件 fail-closed：解析不了的配置按"未配置"处理，`doctor` 与
  `control reload` 会给出带行号的错误
- 刷新 catalog 与集成块而不重启服务：`./codex-router control reload`

## control 命令面

App 的每个按钮背后就是这些命令（`./codex-router control …`）：

```
control --json                          tray 快照（providers/models/presence）
control service start|stop|restart|status
control providers list [--json]         provider 与凭证状态
control providers enable ID [ID...]     追加启用
control credential PROVIDER             stdin 写入 api_key 到 config.toml
control credential PROVIDER --remove    删除对应表
control config init                     生成注释模板
control reload                          重读配置+刷新 catalog，不重启
control subagents status|mode|select-all|unselect-all|set|provider
control picker set <slug> show|hide | provider <id> | all | status
control tool-result-aging status|on|off
control presence set always|follow-codex
control account --json | provider-usage --json    配额与用量
control vision-bridge pull TAG | pull-status | benchmark | catalog
control local-runtime status|start|stop
```

## 管线功能（全部保留自原项目）

| 功能 | 说明 |
|---|---|
| 空补全守卫 | 上游 200 但零 token：按住不发头→同字节静默重试→仍空则如实报错 |
| Prompt-token 补零 | 上游报 `input_tokens: 0` 时以偏高估算替换，防 Codex 永不压缩上下文 |
| Tool-result aging | 重发历史时老化巨大的旧工具结果 |
| Namespace 拍平 | Codex 的 `<ns>__<tool>` 展开为真工具，响应精确映射回 |
| Compaction v1/v2 | 80KB 尾部摘要 / kcr1 base64 压缩 |
| Subagent relay | Fernet 密文检测，外部模型的子代理载荷经原生端点中继 |
| **视觉桥** | 文本模型也能读图：结构化转写（六段证据合同）、一图一购缓存、引擎自动选择/回退/本地 Ollama |
| Rate-limit 收割 | 从上游响应头攒限流窗口信息 |
| 用量计量 | 每回合一行 JSONL：token、首 token 延迟、重试；App 用量卡片的数据源 |
| **协作子代理** | Codex v2 协作的分身候选：注册表 `multiAgentVersion` 证明标记（真实探针通过才标）→ catalog 发布 + agents 目录按名 spawn 定义；`control subagents` 三模式管理（proven/selected/all），本地只能收窄不能放大 |
| codex shim | 可选的 PATH 包装器，启动前确认路由器就绪（`codex-router shim install`） |

## 扩展协议

provider 在注册表里声明协议（`internal/registry/config/`），
`internal/wire/` 是协议抽象层：**新协议 = 新增一个包实现
`wire.Protocol` 接口 + 注册**，服务端零改动。内置两种：

- `chat-completions`（默认）：Responses ↔ chat-completions 双向翻译
- `openai-responses`：Responses 直通（仅换模型名、剥客户端元数据）

## 安全模型（摘要）

- 认证 = URL 路径里的随机 caller key，仅监听 `127.0.0.1`
- 凭证只住 `~/.codex-router`（0700 目录 / 0600 文件），或环境变量引用
- 原生会话兜底：无自身凭据的本地客户端可复用本机 Codex 登录会话，
  token 永不出进程、永不过期使用（exp 前两分钟停止注入）
- 任何凭证不进日志、catalog、健康检查输出；托管 base URL 视为本地机密，
  输出一律打码
- 详见 [SECURITY.md](SECURITY.md)

## 动态模型注册（discover + models.dev）

```sh
./codex-router discover zai-coding       # 只读：实时拉 provider /v1/models 全量列表
./codex-router control models sync       # 发现 + 自动注册新模型（install 也会自动跑）
./codex-router control models list       # 查看动态注册的模型（user-models.json）
./codex-router control models remove <slug>
```

- 参数来自 **models.dev** 开源库（上下文窗口/推理/视觉/描述，精确值），
  未收录的模型回落同家族克隆（effort 档位始终来自家族）
- 写入 `~/.codex-router/user-models.json` 覆盖层 —— 升级二进制不丢；
  与内嵌注册表撞车时内嵌优先（正式收录永远赢）
- 注册后向服务进程发 SIGUSR1 **热重载注册表**，即时可路由；
  picker 显示需重开 Codex
- App 设置页「模型同步 → 立即同步」是同一件事的按钮入口

## 卸载

```sh
./codex-router uninstall          # 还原 config.toml、清 CLI 工件；状态保留
./codex-router uninstall --purge  # 连凭证与用量历史一起销毁（显式选择）
```

App 本体（`~/Applications/Model Router.app`）由你手动拖出删除。

## 开发

```sh
go build ./... && go test ./...          # Go 侧（12 个测试包）
./scripts/build-macos-tray-app.sh        # 重建 App（含内嵌 Go 二进制）
```

目录：`cmd/codex-router/`（CLI 入口）· `internal/wire/`（协议层）·
`internal/translate/`（纯翻译库）· `internal/server/`（HTTP 服务）·
`internal/registry/`（注册表 + 内嵌 config）· `internal/state/`、
`internal/cred/`、`internal/tomlconf/`、`internal/usage/`、`internal/vision/` ·
`apps/macos/ModelRouterTray/`（Swift App）。

状态目录 `~/.codex-router`；`GO-REWRITE-PLAN.md` 是设计与迁移的
权威记录。
