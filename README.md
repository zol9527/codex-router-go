# Model Router

> 基于 [duolahypercho/codex-router](https://github.com/duolahypercho/codex-router)；
> 归属与许可说明见 [NOTICE.md](NOTICE.md)。

把 Codex（App 与 CLI）接到你自己的模型订阅上：**Z.ai GLM Coding Plan**、
**opencode Go/Zen**、自托管 **LiteLLM**，外加**原生 ChatGPT 订阅直通**。路由器以 Responses API
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
# 1. 构建 App（默认直接产出 ~/Applications/Model Router.app，
#    Go 服务二进制内嵌在 bundle 里）
./scripts/build-macos-tray-app.sh
open ~/Applications/"Model Router.app"      # 服务随 App 启动

# 2. CLI 工具（产物统一落 dist/）
make cli

# 3. 发布到 Codex（写 config.toml 标记块 + 模型 catalog，幂等）
./dist/codex-router install

# 4. 填凭证（见下一节），然后体检
./dist/codex-router doctor
```

完全退出并重开 Codex，新建任务，picker 里选路由模型。

> Voice 不走 Responses router。安装会在受管根级块中写入 Codex 的原生
> WebRTC 与 WebSocket 端点，避免 Voice 把 `/live` 请求发到本地 `4202`
> 后得到 404。若你已在 `~/.codex/config.toml` 自行设置这两个 Realtime
> 端点，router 会保留该设置且卸载时不改动它。

## 凭证：`~/.codex-router/config.toml`

Claude Code 式的配置文件，改完**下一回合请求即生效**，无需重启：

```toml
[zai-coding]
api_key = "sk-..."

[opencode-go]
api_key = "..."
# 也支持环境变量引用：api_key = "{ZAI_API_KEY}"
# 未设置的变量展开为空（=未配置）；{非变量形状} 按字面量保留

[litellm]
api_key = "sk-..."
# base_url = "https://your-litellm.example/v1"  # 必填；也可 export LITELLM_BASE_URL

[env]
# 可选：dotenv 兜底文件。GUI App 拉起的服务继承 launchd 环境，
# 看不到登录 shell 里的变量；{VAR} 查不到进程环境时从这里兜底。
# 支持 ~ 路径；只取 KEY=VALUE / export KEY=VALUE 行，其余语法跳过。
file = "~/.secrets/env"
```

- 生成带注释模板：`./dist/codex-router control config init`
- App 设置页「凭证配置 → 打开配置」直达编辑
- 解析顺序：**环境变量 > config.toml > `*.secret` 文件 > macOS Keychain**
- 命令行写入（隐藏输入）：`./dist/codex-router control credential zai-coding`
- 坏文件 fail-closed：解析不了的配置按"未配置"处理，`doctor` 与
  `control reload` 会给出带行号的错误
- 刷新 catalog 与集成块而不重启服务：`./dist/codex-router control reload`

## control 命令面

App 的每个按钮背后就是这些命令（`./dist/codex-router control …`）：

```
control --json                          tray 快照（providers/models/presence）
control service start|stop|restart|status
control providers list [--json]         provider 与凭证状态
control providers enable ID [ID...]     追加启用
control set ID on|off                   托盘开关路径（整体重写选择）
control apply                           重发布 catalog + 集成块（= reload）
control credential PROVIDER             stdin 写入 api_key 到 config.toml
control credential PROVIDER --remove    删除对应表
control config init                     生成注释模板
control reload                          重读配置+刷新 catalog，不重启
control subagents status|mode <m>|select-all|unselect-all|declare <slug>|undeclare <slug>|set <slug> on|off|provider <id> on|off
control picker set <slug> show|hide | provider <id> show|hide | all show|hide | status
control models sync [PROVIDER]|list|remove <slug>|add PROVIDER <upstream-id> [--efforts a,b] [--default-effort x] [--context-window N]
control presence set always|follow-codex
control account --json | provider-usage --json    配额与用量
control probe PROVIDER [MODEL]          上游行为探针（models / args 可见性 / 计数口径）
control vision-bridge on|off | status | effort <level|default>
```

## 管线能力

| 能力 | 说明 |
|---|---|
| 空补全守卫 | 上游 200 但零 token：按住响应头，结束时返回明确失败；是否重试交给 Codex |
| Prompt-token 补零 | 上游报 `input_tokens: 0` 时以偏高估算替换，防 Codex 永不压缩上下文 |
| Namespace 拍平 | Codex 的 `<ns>__<tool>` 展开为真工具，响应精确映射回 |
| Compaction v1/v2 | 80KB 尾部摘要 / kcr1 base64 压缩 |
| Subagent relay | Fernet 密文检测，外部模型的子代理载荷经原生端点中继 |
| **视觉桥** | 文本模型也能读图：结构化转写（六段证据合同）、单引擎单次读取、本地 Ollama |
| Rate-limit 收割 | 从上游响应头攒限流窗口信息 |
| 用量计量 | 每回合一行 JSONL：token、首 token 延迟；App 用量卡片的数据源 |
| **协作子代理** | Codex v2 协作的分身候选：注册表 `multiAgentVersion` 证明标记（真实探针通过才标）→ catalog 发布 + agents 目录按名 spawn 定义；`control subagents` 三模式管理（proven/selected/all），本地只能收窄不能放大 |
| codex shim | 可选的 PATH 包装器，启动前确认路由器就绪（`codex-router shim install`） |

## 上游超时与看门狗

上游可能"接受请求后既不吐字节也不报错也不断开"。连接建立层装固定档闸
（拨号 30s / TLS 握手 15s，不开 env），流式层装三道可调 fail-fast 闸，把
无限挂起变成可重试的快速失败（Codex 对 5xx 自带重试接管）：

| 闸 | 默认 | 语义 | 关闭 |
|---|---|---|---|
| 响应头超时 `CODEX_ROUTER_HEADER_TIMEOUT_SEC` | 300 | 上游多久不回响应头判死（502） | 设 `0` |
| 流空闲看门狗 `CODEX_ROUTER_IDLE_TIMEOUT_SEC` | 180 | SSE 流上多久零字节判死：头未提交回 504 `upstream_idle_timeout`；已提交只截断（调用方整轮重试） | 设 `0` |
| WS 静默看门狗 `CODEX_ROUTER_WS_SILENT_TIMEOUT_SEC` | 60 | 客户端发过请求帧而上游此后零回帧超窗口 → 主动拆管（跨 turn 空闲不拆） | 设 `0` |
| WS keepalive `CODEX_ROUTER_WS_KEEPALIVE_SEC` | 60 | 空闲管道周期向上游发 ping，防中间设备（NAT/TUN）按空闲超时砍断长连接 | 设 `0` |
| 慢请求日志 `CODEX_ROUTER_SLOW_REQUEST_LOG_SEC` | 120 | 请求在途超窗口补一行 `slow request pending`（只记录、不拆流，收尾日志照常） | 设 `0` |

失败一律落 `router.log`（含 model/provider/status/duration/错误摘要）与
`usage-events.jsonl`（`upstreamIdle`/`streamAborted` 字段）。

## 动态模型注册（discover + models.dev）

```sh
./dist/codex-router discover zai-coding       # 只读：实时拉 provider /v1/models 全量列表
./dist/codex-router control models sync       # 发现 + 自动注册新模型（install 也会自动跑）
./dist/codex-router control models list       # 查看动态注册的模型（user-models.json）
./dist/codex-router control models remove <slug>
./dist/codex-router control models add PROVIDER <upstream-id> \
    [--efforts minimal,high] [--default-effort high] [--context-window 1048576]
```

- 参数来自 **models.dev** 开源库（上下文窗口/推理/视觉/描述，精确值），
  未收录的模型回落同家族克隆（effort 档位始终来自家族）；
  `models add` 采信上游自报元数据，拉不到再回落本地猜测链
- 写入 `~/.codex-router/user-models.json` 覆盖层 —— 升级二进制不丢；
  与内嵌注册表撞车时内嵌优先（正式收录永远赢）
- 注册后向服务进程发 SIGUSR1 **热重载注册表**，即时可路由；
  picker 显示需重开 Codex
- App 设置页「模型同步 → 立即同步」是同一件事的按钮入口

## 扩展协议

provider 在注册表里声明协议（`internal/domain/registry/config/`），
`internal/domain/wire/` 是协议抽象层：**新协议 = 新增一个包实现
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

## 卸载

```sh
./dist/codex-router uninstall          # 还原 config.toml、清 CLI 工件；状态保留
./dist/codex-router uninstall --purge  # 连凭证与用量历史一起销毁（显式选择）
```

App 本体（`~/Applications/Model Router.app`）由你手动拖出删除。

## 开发

```sh
make help        # 全部目标一览
make cli         # 编译 CLI → dist/codex-router
make app         # 构建 App → dist/Model Router.app
make test        # Go 全量测试（含 internal/arch 分层车道护栏）
make test-app    # Swift 包测试
make install-cli # 原子替换 ~/bin/codex-router（部署）
make install-app # App 换到 ~/Applications（部署；App 在跑会拒绝）
```

`internal/` 按语义四层组织（ADR-0005，依赖边只朝下，`internal/arch`
测试钉住）：`cmd/codex-router/`（薄入口）+ `internal/app/cli/`（命令
实现）· `app/`（server、controlplane、codexconfig —— 对外表面）·
`engine/`（routing、nativebackend、catalog、discover —— 任务执行）·
`domain/`（registry、translate、wire、state、cred、usage、vision ——
领域数据与接缝）· `lib/`（httpx、tomlconf、modelmeta —— 共享实现
单点归宿）· `apps/macos/ModelRouterTray/`（Swift App）。

深入阅读：[docs/architecture/README.md](docs/architecture/README.md)
（架构调研与请求生命周期）· [docs/adr/](docs/adr/)（架构决策记录）·
[SECURITY.md](SECURITY.md)（安全模型）· [NOTICE.md](NOTICE.md)（归属）。

状态目录 `~/.codex-router`。
