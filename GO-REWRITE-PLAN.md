> 状态（2026-08-15）：**切换已完成并经真实流量验证。** 本机生产路由现为
> Go 单二进制（~/.local/share/codex-router-go/codex-router，launchd
> io.github.codex-router.go，单进程 ~26MB），bin/control 已指向它。
> 真实验证记录：codex exec 两轮全通（native 直通 200×N）、/health 与
> usage-events.jsonl 计量在位、doctor 核心全绿。
> 切换中修掉两个仅真实流量能暴露的 bug：input_modalities 必须是序列
>（Codex 拒载 catalog）、native 路径漏记 usage。
> **tray 面板已实测并修复对接层**（真实截图暴露）：Go 侧四个 tray 面向
> 命令的形状已对齐 Swift 解码器 —— `control --json` 发 `targets` 字典
>（旧扁平结构让面板整页「路由不可用」）、presence 补 `harnessPublished`
> 显式 false（缺键令整个快照解码失败）、`providers --json` 改发
> ProviderSetupSnapshot、`account --json` 去掉 status 包装直发
> CodexAccountUsage（补非可选 summary 键）、`provider-usage --json` 重写为
> 按 provider 合并的 ProviderUsageSnapshot（移植 Node 的中位速率/不可信
> 样本剔除/日桶语义，`internal/usage/providerusage.go`）。裁剪掉的命令
>（maintenance/apply/harness 等）给一行人话错误而非 usage 倾倒。
> 回归测试钉住全部形状（providerusage_test / presence_test）。
> **状态目录已迁至 ~/.codex-router**（不再复用原版嵌在 Codex 家目录的
> ~/.codex/codex-router）：state.Open 首次解析到新默认目录且旧目录存在时
> 一次性只读迁移（文件级 tmp+rename，caller-secret 随迁故 config.toml
> 已发布的 URL 保持有效；新目录已存在则不迁移，旧目录永不改动）。
> launchd plist 内嵌 --state 绝对路径，迁移后已重装刷新；迁移边界由
> migrate_test.go 钉住。
> **操作者待办**：`./bin/control credential zai-coding`（stdin 输入 key）
> 与 `credential opencode-go` 启用路由模型；tray 已构建安装于
> ~/Applications/Model Router.app 并在菜单栏运行，面板点「刷新」即得
> 全量数据。uninstall 零痕迹（plist/二进制目录/config 块全清，state
> 默认保留、--purge 显式销毁）——已做真实往返验证；`git checkout -- bin/control`
> 恢复旧入口。部署替换运行中二进制必须原子 mv（原地 cp 会被 macOS 代码
> 签名 SIGKILL）。
> 已落地（12 个功能提交，~9.4k 行 Go，9 包测试全绿，各里程碑有假上游端到端测试）：
> - M1 协议核心：认证 / native 直通 / Responses↔chat 双向翻译 / Responses 直通 / zstd / retry
> - M2 集成面：catalog（codex debug models）/ config.toml 标记块 / install·uninstall·doctor / launchd / control
> - M3 全部：空补全守卫（流式）/ aging / prompt-token 补零 / usage JSONL / 错误翻译 / namespace 拍平（schema 归一·整数 token·spawn_agent 白岗）/ codex-app 快照合并 / compaction v1+v2 / rate-limit 收割 / subagent relay
> - M4 核心：证据合同 / 引擎解析（pin·auto·回退·loopback 排除）/ 三路读图（registry·native 会话·本地 Ollama）/ 一图一购（缓存+in-flight）/ 重试 / state 门控
> - M5 部分：presence（effectiveMode 覆盖）/ codex shim（全安全约束）
> **M4、M5 的全部编码工作已完成**（16 个功能提交，~11.5k 行 Go，10 个测试
> 包全绿）。配额卡片已对真实环境验证：ChatGPT 账号窗口经本机登录会话读到
> （planType plus / 主窗口 34%），zai 配额端点真连通（假 key 正确回传
> "token expired or incorrect"）。
> **仅剩两项需要操作者亲自在场的收尾**：
> 1. tray 对接实测：把 apps/macos/ModelRouterTray 的 bin/control exec 目标
>    改为 Go 二进制，在图形会话里过一遍按钮面板；
> 2. launchd 切换：`install` 后卸载旧 Node 栈（按共识"功能齐全 + 操作者在场
>    验证后才切"；回滚 = plist 指回 node start.mjs，旧 checkout 原样保留）。

## 目标形态

一个 Go 二进制，launchd 直挂，只监听 `127.0.0.1:4202`，替代现有 5 进程栈
（node router + node api-forwarder + node oauth×2 + Python LiteLLM）。
无 Python、无 venv、无 npm 运行时依赖。

- Codex 端 `config.toml` 的 base URL（URL 路径 caller key 认证）**一字不改**，切换日无感。
- `~/.codex/codex-router` state 目录**复用**：两个 secret、provider key 文件
  （`.secret`，0600）、`usage-events.jsonl` 历史全部沿用；`litellm.yaml` 等死文件切换后清理。
- Go 代码标准布局：`cmd/codex-router` + `internal/`，与现有 `src/` 并存，切换后删除 Node 侧。

## Provider 集合（闭集，新增自己加）

| Provider | 协议路径 | 上游 | 认证 |
|---|---|---|---|
| zai-coding（GLM Coding 套餐） | Responses → chat completions 翻译 | `https://api.z.ai/api/coding/paas/v4` | API key（env → `.secret` → Keychain） |
| opencode-go（chat 变体） | Responses → chat completions 翻译 | `https://opencode.ai/zen/go/v1` | 同上（`opencode-go-api-key.secret`） |
| opencode-go-responses | Responses 直通（model 还原 + 头清洗 + effort 映射） | 同上 | 共享同一凭据 |
| 原生 Codex 订阅 | 未命中注册表的 slug → native 直通 | `https://chatgpt.com/backend-api/codex`，透传 FORWARD_HEADERS 会话头 | Codex 自带会话 |

不做 Anthropic messages 翻译路径。内部 hop（原 4200/4203）变为进程内函数调用。

## 保留并移植的功能

- **router 管线**：subagent `encrypted_content` 中继（Fernet `gAAAAA` 前缀判定、
  namespace 拍平、SSE/JSON 双解析）、compaction v1/v2、codex-app-tools 工具合并、
  tool-result aging、空补全守卫、prompt-token 补零替换、错误翻译、
  usage events（JSONL 原格式，token 数从 SSE 流解析）、会话名、rate-limit header 收割。
- **vision bridge 全家**：引擎三路（registry 走内部路由 / native 会话门控 / 本地 Ollama）、
  state 门控（文件存在即 operator 答案，解析失败按 off）、download、benchmark；
  Ollama runtime 拉起作为底座保留。
- **tray 全体验**：control 做成 Go 子命令（`bin/control` 改一行 exec 目标，Swift 零改动）、
  `/health` 350ms 轮询的 activity JSON 照旧、presence（"With Codex"）、
  配额卡片只覆盖 ChatGPT 账号用量 + zai/opencode 余额。
- **codex shim**（PATH 包装，保留旧 AGENTS.md 中 shim 一节的全部安全约束）。
- **catalog/config 发布（Go 简化版）**：`codex debug models` 抓取 + registry 合并 +
  `merged-models.json` + config.toml `BEGIN/END` 标记块写入；
  砍掉 announced-models 公告、multi-target、Codex 版本 clamp。
- **安装**：一个 `install` 子命令（secret 生成、launchd plist、state 初始化、首次发布）+
  残血 `doctor`（服务活、key 在、catalog 新鲜等 ~5 项）。
- `config/` JSON 注册表格式原样保留（Go `encoding/json` 直接读）。

## 砍掉的

dsh 全家（yaml-structure/dsh-*，~1.4k 行）；kimi/grok/copilot/minimax OAuth 与
session 模块（~2.5k 行）及 4201/4208 forwarder；本地模型作为对话 provider
（local-models 目录、agent-check、picker 发布、`num_ctx` 路由，~1.9k 行——
Ollama runtime + vision-download 底座除外）；Windows；legacy 迁移；
面向陌生安装者的 onboarding/curate/support-bundle/smoke-test；
34k 行 Node 测试（不移植）。

## 落地策略

**并行开发，功能齐再切**：旧 Node 栈继续在 4202 服务日常，Go 版在测试端口开发；
协议翻译器用录制的真实流量回放做黄金测试（火力集中在 SSE 分帧与 tool_calls
事件形状——LiteLLM 替我们踩坑最密集处），管线各 Transform 配针对性单测。
全部就绪后 launchd 改指向 Go 二进制；回滚 = plist 指回 `node start.mjs`，
旧 checkout 原样保留。

### 里程碑

1. **M1 协议核心**：URL 路径 caller key 认证、native 直通、
   Responses→chat completions 双向翻译器、Responses 直通、zstd、native upstream retry。
2. **M2 集成面**：catalog 简化版、`install`、launchd、control 基础
   （`--json` 快照、service、providers、credential）。
3. **M3 router 管线全家**（上表第一组）。
4. **M4 vision bridge 全家**。
5. **M5 周边 + 切换**：presence、shim、配额卡片、tray 对接验证
   （harness 相关 Swift 行删除）、launchd 切换、旧栈封存、重写薄 AGENTS.md。

总量估算 8k–12k 行 Go，按周计。

## 移植规格书（必读约束）

行为规格以旧实现及其测试为准，不凭印象重写：

- subagent relay 与 prompt-token 替换的约束见旧 AGENTS.md 对应章节 +
  `test/routing.test.mjs` / `test/response-usage.test.mjs`。
- prompt-token：只替换**显式零**、estimate 只高不低（compaction 余量 14%）、
  telemetry 保留 provider 原值另加 `estimatedInputTokens`。
- native 端点窄请求面：`store:false`、`stream:true`，十个参数 denylist
  （temperature/top_p/presence_penalty/frequency_penalty/max_tokens/
  max_output_tokens/metadata/seed/user/truncation），仅对被替换会话的 caller 归一。
- upstream retry 只在未中继任何字节前合法；502/503/504/520-524/连接错误；
  不加 429/4xx/500；预算 5s、2 次。
- vision bridge 的安全边界（native 引擎 fail-closed、凭据不落盘、
  本地引擎仅显式 pin）整节移植。
