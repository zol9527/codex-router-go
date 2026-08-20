# opencode v2 稳定性设计借鉴研究

> 调研日期：2026-08-16
> 基线：本仓库 `go-rewrite` 分支（含未部署的 arguments 镜像修复）
> 方法：router 侧事实来自本仓库源码与运维记录；opencode 侧只采信一手来源（sst/opencode dev 分支源码，commit `976c1851`，与 opencode.ai 官方文档），逐条附出处。
> 落点：`docs/research/`（既有调研笔记目录约定）。
>
> **历史快照说明（2026-08-17）**：本文记录的是调研当日的实现基线。后续 Router 已删除上游自动重试、协作/视觉缓存和 tool-result spill；涉及这些机制的“现状”描述不再代表当前代码。2026-08-20 起 internal/ 已按 ADR-0005 四层重组（如 `internal/server/routed.go` → `internal/app/server/routed.go`），文中旧路径按快照理解。

## 1. 背景与问题定义

Model Router 是本地 Codex 模型网关：接住 Codex 的 OpenAI Responses 请求，命中注册表的模型经协议翻译（Responses → chat-completions）转发外部供应商，未命中则原生透传。单二进制 Go 实现（~16k 行），数据面常驻，稳定性问题在真实使用中持续暴露。

过去一年的稳定性事故全史（按根因分类）：

| 类别 | 事故 | 现状 |
|---|---|---|
| 上游静默黑洞 | WS 透传 30s 精确挂死（Codex 复用死连接）；HTTP SSE 92k ctx 零字节挂 6m46s 后 500 | fail-fast 三闸已部署（header 300s / SSE idle 180s / WS 静默 60s，env 可调） |
| 历史改写杀缓存 | tool-result aging 的 frontier 滚动翻转前缀缓存，命中率崩 | 已被 spill 取代（内容纯函数截断） |
| 上游协议缺陷 | Z.ai 丢弃 `tool_calls[].function.arguments`（模型看不到自己的命令/patch，写文档任务无限重写 7 份） | 镜像补丁已实现待部署（`MirrorToolCallArguments`） |
| 流协议边缘 | EOF 无 `[DONE]` 哨兵 → 重试风暴；custom tool call 轮不发 delta → Codex 5 连 retry | 均已修（EOF 哨兵 + `finishFlushWith` 兜底） |
| 客户端侧缺陷 | Codex `cached_websocket_session` 复用死连接、`prewarm_websocket` 无超时 | 只能重启 router 清缓存（Codex 侧问题，router 无解） |
| usage 观测缺口 | 上游报 `input_tokens:0`（已做补零替换 #95）；cached 明细不落账；arguments 不计入（上游口径） | 部分修复 |
| 目录数据 | catalog 反馈环、克隆显示名撞名、克隆抄来的 context_window 与实际不符（1M 声明 vs `[1m]` 后缀路由不可用） | 均已修或实测澄清 |

## 2. router 现有保护机制盘点（对照基准）

请求方向管道（`serveRouted`，`internal/server/routed.go`）：

```
协作密文中继(agentrelay, LRU+TTL 缓存)
→ 视觉桥(vision, 贴图代读)
→ spill(确定性工具结果截断, ≥32KB 落盘+sha256 回执)
→ codex app 工具合并 + namespace 拍平
→ 协议翻译(wire/chatcompletion → translate)
→ glm-thinking profile(effort 钳制/采样剥离/arguments 镜像[待部署])
→ zstd 压缩(>16KB) → 上游
```

响应方向：SSE 增量重组为 Responses 事件流；空补全守卫（整流判定 + 同字节隐形重试一次）；usage 补零替换（3.3 B/tok 估算，只落在显式零上）。

上游交互（`internal/httpx`）：

- **重试只在首字节前**：`FetchWithRetry` 默认 2 次、250ms 起 3 倍退避 5s 预算；可重试状态码刻意排除 429（Retry-After 透传）与 500（源站已运行），只收 502/503/504/520-524 与连接类错误（`connect.go` 移植 Node 版 RETRYABLE_ERROR_CODES 全集）
- **idle 看门狗**（`idle.go`）：响应体连续无字节即断开，错误链带 `ErrUpstreamIdle` 哨兵映射 504——"无限挂起"变"可识别失败"，对齐 AI SDK 的 fail-fast 语义
- **请求体上限**：解码后超限 413，拒绝而非截断（截断会制造不可解析 JSON）

会话压缩（`compaction.go`）：整段对话重放给同 provider 生成交接摘要，v1/v2 双形态；spill 在压缩重放中同样生效（摘要器不需要旧工具输出中段）。

架构约束（与 opencode 的根本差异）：**router 是无状态翻译网关**——会话历史、压缩阈值决策、turn 级重试的主导权都在 Codex 客户端。router 能掌控的是翻译保真、上游防御、错误分类透传。

## 3. opencode v2 对应设计（一手源码调研）

版本线：v2 位于 dev 分支 `packages/core`（含 `v1/` 兼容层）+ 全新 `packages/llm`（自研协议层）+ `packages/schema` + `packages/http-recorder`。**v2 的核心决策是抛弃 Vercel AI SDK 作为协议层**——`packages/llm/src/protocols/` 下自研 anthropic-messages / openai-chat / openai-responses / gemini / bedrock-converse / openai-compatible-chat 六套协议，AI SDK 仅作遗留兼容壳（`packages/core/src/aisdk.ts`）。**与 router 自己翻译协议是同构选择**；v2 的稳定性不来自 AI SDK，而来自自研协议层的显式状态机 + 错误分类学。

### 3.1 上下文/历史管理

**工具结果落盘（tool-output-store.ts）**：与 router 的 spill 同源设计——判定纯函数（`MAX_LINES=2000`、`MAX_BYTES=50KB`、无时间/位置依赖）；`flag:"wx"` 独占创建 + ULID 单调 ID 命名；头尾双端预览（行数超限取前/后各半，marker 自身计入预算 `maxLines-4`/`maxBytes-markerBytes-4`，保证"截断后永不超限"是严格不变量）；回执 `... output truncated; full content saved to ${path} ...` 只嵌路径无 hash；7 天保留 + 每小时清扫；config 可覆盖 `tool_output.max_lines/max_bytes`。

**压缩（session/compaction.ts）**：阈值 = `contextWindow − max(maxTokens, 20k buffer)`；token 估算纯函数 `len/4`。选择算法是 **head/recent 分割**：从尾向前累计 8k token 为 recent（**原文逐字保留**），之前的 head 才被摘要；摘要用同模型但 `tools:[]`、输出限 4k；**增量合并**（旧 summary + 新 head 双标签进 prompt，摘要不滚雪球）；摘要 prompt 是锚定结构模板（Objective/Work State/Next Move/Relevant Files，要求保留精确路径与错误串）。**失败安全**：摘要流中任何错误或空摘要 → `return false`，绝不丢原始历史；两阶段事件（Started→Ended），崩溃在中间等于没压缩过。溢出被动恢复：收到 context-overflow 错误且 assistant 未开始输出时触发，只允许一次。

**前缀缓存友好（三处机制）**：① `cache-policy.ts` 编译期在**最后一个 tool 定义、最后一个 system part、最新一条 user 消息**各放一个 CacheHint 断点（turn 内的 assistant/tool 往返全部落在断点之后，多轮工具循环每步命中前缀）；② **系统提示永不按位置重写**——系统上下文变化走 Context Epoch 机制，以 Mid-Conversation System Messages **追加**注入而非改写 system prompt；③ 历史重放确定性（reasoning 元数据仅同模型且无错误才回放，换模型降级纯文本）；④ OpenAI 侧显式 `promptCacheKey = session.id` 会话级缓存路由。

### 3.2 流式健壮性

**超时**：v2 原生协议路由**没有** idle watchdog（`transport/http.ts` 裸流直通——这是它的缺口，router 的三闸反超）；AI SDK 兼容路径有 `wrapSSE()`：每次 `reader.read()` 与 `setTimeout` 竞速，三个正交超时键（`headerTimeout`/`chunkTimeout`/`timeout` 总时长）用 `AbortSignal.any` 合并，且显式关掉底层 fetch 默认超时防双计时。v1 的 WebSocket 传输有 idleTimeout。

**终止语义（防 EOF 重试风暴的核心）**：协议层显式声明终止谓词（`Stream.takeUntil(protocol.stream.terminal)`）；`onHalt` 兜底把已累积的半成品状态冲刷成事件（补发已累计 tool call、用已知 finish_reason 收尾）；**最强闸门**：fold 完成后拿不到 terminal finish 直接失败（`"Provider stream ended without a terminal finish event"`）——**静默 EOF 归类为不可重试错误而非成功**，半截输出不落库。`[DONE]` 在 framing 层当 keep-alive 丢弃，状态机不依赖它存在与否。

**重试与幂等**：HTTP 层 2 次、500ms 起、10s 封顶抖动指数退避；仅 429/503/504/529 可重试，尊重 `retry-after-ms`/`retry-after`（秒与 HTTP-date 双格式）；只重试流开始前的失败。**已经开始输出的 turn 绝不重放**（副作用风险优先于可用性）；溢出恢复只允许一次；provider error 到达后后续事件全部丢弃 + failAssistant 物化。

### 3.3 Provider/模型抽象

注册表四层：models.dev 远端 JSON + 5 分钟磁盘缓存（flock 锁、抓取 10s 超时、瞬时错误 2 次退避重试、**原子写** tmp+rename、坏缓存自动删除重拉）+ 编译期内置快照兜底 + config 逐模型 `limit.context/output` 覆盖。协议差异隔离在每协议一个 body-builder + 流状态机，公共机制（ToolStream 累积器、usage 映射、parseToolInput）下沉 shared。用户 `http.body` overlay 有**协议字段黑名单**（messages/model/tools 等，直接 `InvalidRequest`）防配置踩踏。

### 3.4 错误处理

**分类学（errors.ts）**：10 种 reason 的 tagged union，每种自带静态 `retryable` 属性——RateLimit/ProviderInternal 可重试；**Transport（含 Timeout）不可自动重试**；InvalidProviderOutput 不可重试。分类逻辑按 status + body 正则；**context-overflow 识别是 28 条正则的模式库**（含限流排除项防误判）。错误呈现带脱敏 HttpContext（两遍脱敏：字段名正则 + 请求密钥字面量替换）。

**上游丢 tool 参数的防御**：空参数串当 `"{}"`（源码注释直言 "providers occasionally finish a tool call without ever emitting input deltas"）；Responses 协议 `output_item.done` 的权威最终 arguments 覆盖累计值（"the final value wins"）；**未结算工具统一物化为 `Tool.Failed("Provider did not return a tool result")`**——上游丢失内容时模型下一轮看到显式错误文本而非空洞（与 router 的 arguments 镜像同一思想的输出侧变体）。

### 3.5 测试策略

`packages/http-recorder`：VCR 式 cassette 录制回放——CI 强制 replay、本地有则 replay 无则 record；**回放严格记账**（"used X of Y interactions" 不匹配即失败，抓住"实现少发了一次请求"的回归）；请求匹配用规范化 JSON 快照（key 排序整体比对）；cassette 内建脱敏；HTTP+WS 双传输。`packages/llm/test`：**同一 golden 场景（streams tool call / drives a tool loop 等）× 多 provider × 多协议 × 双传输的矩阵回放**，cassette 按 `provider-protocol-transport` 前缀落盘。

## 4. 对照结论：已达成 vs 差距

### 4.1 router 已等价或反超的（无需动）

| 机制 | router 现状 | opencode v2 | 判定 |
|---|---|---|---|
| 工具结果确定性截断 | spill：内容寻址 sha256 + 头尾双端预览（`safeHead`/`safeTail`）+ 7 天 janitor | 路径回执无 hash | **router 反超**（hash 支持内容完整性自校验） |
| 三正交超时 | header 300s / SSE idle 180s / WS 静默 60s，env 可调 0=关 | 原生路由**无** watchdog（缺口） | **router 反超**（唯一缺"总时长"键，但 reasoning 长 turn 误杀风险大于收益，不加） |
| 重试窗口 | 仅首字节前，2 次 3 倍退避 5s 预算；429 透传 Retry-After | 同语义（2 次、抖动退避、仅流开始前） | 等价 |
| 已输出不重放 | 空补全守卫只重试零输出流 | assistant 已开始即不重放 | 等价 |
| 模型注册表 | models.dev + 磁盘缓存 + user-models.json 覆盖 + SIGUSR1 热重载 | 同四层结构 | 等价 |
| 系统提示恒定 | instructions 原样透传，翻译层不注入动态内容 | Context Epoch 追加式 | 等价（router 无会话状态，天然合规） |
| 上游丢 tool 参数 | arguments 镜像进 tool output（待部署） | 空串→`{}` + done 权威覆盖 + 失败物化 | 等价思想，router 针对的是丢**历史**参数（上游组装侧），opencode 针对的是丢**增量**参数（流解析侧），互补 |

### 4.2 真实差距（按优先级落地）

#### P0-1 上游行为探针命令化（`control probe`）

今天 arguments 事故的诊断成本极高（usage 口径交叉实验法）。opencode 的 cassette 矩阵防的是**自家翻译回归**，防不了**上游行为漂移**（Z.ai 哪天修了/换了实现）。把今天的手工诊断固化成命令：

```
control probe upstream zai-coding
  ✓ arguments needle   ：16KB args 埋密码 → 镜像形态与原始形态各打一发，验证模型可见性
  ✓ prompt_tokens 口径 ：阶梯尺寸 arguments 看计数是否随内容增长
  ✓ 终止行为          ：截断 SSE（无 [DONE]）看上游是否仍产出 finish_reason
  ✓ 大输入完整性      ：600KB 纯文本 needle
```

实现落点：`cmd/` 下新增子命令，复用 httpx 与凭据解析；结果写 `~/.codex-router/probe-latest.json` 供 tray 显示。这是把"上游当不可信边界"的运营化。

#### P0-2 流终止闸门全路径审计

opencode 的最强闸门（无 terminal finish 即失败）在 router 的对应物是空补全守卫，但它只覆盖"整流无输出"。需审计 `response_chat.go` 的流重组：**流中途断开且已有部分输出**时，是否可能拼出一个"看起来完整"的 completed 事件交给 Codex（那会让 Codex 落库半截 assistant 消息）。验收标准：`[DONE]` 且 finish_reason 齐全 = 唯一成功路径；其余一律以可识别错误收尾。

#### P1-1 错误分类学 + context-overflow 模式库

现 `errtranslate.go` 是 ad-hoc 翻译。抄 opencode 的形状：定义 `UpstreamError` tagged struct（Retryable() 静态方法），把 `provider-error.ts` 的 28 条 context-overflow 正则整表移植（各家供应商措辞：`context length`/`maximum context`/`too many tokens`…，含限流排除项）。**router 的特殊收益**：识别出 overflow 后可以在错误透传里带上结构化标记，让 Codex 的 compact 逻辑准确触发，而不是把溢出当普通 4xx 展示。

#### P1-2 会话缓存路由调查（prompt_cache_key）

翻译层现 `delete(out, "prompt_cache_key")`（`request_chat.go:76`，防御未知字段拒绝）。opencode 对 OpenAI 系显式发 `promptCacheKey = session.id`。**动作**：实测 Z.ai chat 端点是否接受该字段（或等价的 header）；若接受，改为透传 Codex 的 cache key——多会话并行时避免跨会话缓存串线，命中率更稳。

#### P2-1 compaction 增量摘要合并

router 压缩是整段重放（成本随会话线性涨）。opencode 的 head/recent 分割 + 增量合并可搬一半：**摘要缓存**（按 prompt_cache_key 或会话指纹记旧 summary，新请求只重放 旧摘要 + 压缩点后的新增）。代价是引入会话级状态（LRU+TTL，同 agentrelay 缓存的既有模式）。收益：长会话压缩成本从 O(全史) 降为 O(增量)，摘要质量稳定不滚雪球。

#### P2-2 spill 回执严格不变量

现回执（head+tail 各 1024 rune + marker）可能比阈值略大。opencode 的做法是 marker 计入预算（`-4` 余量）。微改 `receipt()`：预览长度改为 `maxBytes 对齐` 的动态值，保证回执字节 ≤ 原文。防御性收紧，非急。

#### P3 cassette 录制回放矩阵

借 `packages/http-recorder` 思想：录制真实上游 SSE 为 cassette（含脱敏），golden 场景（tool loop / 长 patch 镜像 / compaction / vision 代读）× provider（zai-coding / opencode-go）× 协议（chat / responses 直通）。CI 强制 replay + 严格记账。这是翻译层的字节级回归网——今天镜像补丁这类改动就有自动化护栏了（当前只有形状断言）。

### 4.3 明确不抄的

- **总时长 timeout 键**：reasoning 模型长 turn 正常可达数分钟，总时长闸门误杀风险大于收益。
- **换模型自动降级**：opencode v2 自己也没做（runner TODO 明列）；降级决策属 Codex/用户，网关保持 dumb pipe。
- **会话级重试编排**（v1 的 5 次会话重试 + UI 倒计时）：turn 重试主导权在 Codex，router 的隐形单次重试已够；双层重试放大风暴。

## 5. 来源清单

**sst/opencode dev@976c1851（2026-08-16，sparse clone `/tmp/ocrepo` 与 raw 下载交叉校验一致）**

- 上下文管理：`packages/core/src/tool-output-store.ts`（L13-15, 50-104, 119-136, 138-174, 176-211）；`packages/core/src/session/compaction.ts`（L12-15, 137-158, 160-174, 199-220, 231-242）；`packages/core/src/session/history.ts`（L24-53）；`packages/core/src/session/context-epoch.ts`（L72-76, 122-139）；`packages/core/src/session/runner/to-llm-message.ts`（L70-113, 147-165）
- 流式健壮性：`packages/core/src/aisdk.ts`（L26-72, 85-97）；`packages/opencode/src/provider/provider.ts`（L1737-1768）；`packages/opencode/src/plugin/openai/ws.ts`（L146-200）；`packages/llm/src/route/client.ts`（L284-292, 382-391）；`packages/llm/src/route/framing.ts`（L10-12）；`packages/llm/src/protocols/openai-chat.ts`（L462-470）
- 重试/幂等：`packages/llm/src/route/executor.ts`（L36-38, 91, 93-106, 345-351）；`packages/opencode/src/session/retry.ts`（L21-33, 78-79）；`packages/core/src/session/runner/llm.ts`（L235, 237, 361）
- Provider 抽象：`packages/core/src/models-dev.ts`（L150-215）；`packages/core/src/plugin/models-dev.ts`（L100-114）；`packages/llm/src/route/client.ts`（L303-320）；`packages/llm/src/route/transport/http.ts`（L31-86）
- 错误处理：`packages/llm/src/schema/errors.ts`（L34-192）；`packages/llm/src/provider-error.ts`（L4-43）；`packages/llm/src/route/executor.ts`（L225-275）；`packages/llm/src/protocols/utils/tool-stream.ts`（L100-124, 166-181）
- 测试：`packages/http-recorder/src/recorder.ts`（L8-14, 20-58）；`packages/http-recorder/src/matching.ts`（L20-60）；`packages/llm/test/recorded-golden.ts`、`recorded-runner.ts`（L55-66）
- 缓存：`packages/llm/src/cache-policy.ts`（全文）；`packages/core/src/session/runner/llm.ts`（L204-207）

**文档**：https://opencode.ai/docs/config/（compaction.auto/prune/reserved、provider timeout/chunkTimeout/setCacheKey）；仓库 `CONTEXT.md`（V2 wire contract、Context Epoch）

**router 侧**：本仓库 `internal/httpx/{httpx,idle,connect}.go`、`internal/server/{routed,compaction,errtranslate}.go`、`internal/translate/{request_chat,profile,history}.go`、`internal/spill/spill.go`；运维记录见 `~/.codex-router/router.log` 与事故诊断记录（arguments 丢弃事故 2026-08-16）。
