# ADR 0002: 控制面类型化契约与生命周期所有权

- 状态: 已接受（2026-08-20）
- 背景: 架构评审候选 3（Strong）。`control --json` 曾以 map[string]any 手拼，
  Swift 端 RouterSnapshot 解码器的字段存在性只活在注释里 —— 漏一个
  presence 字段就让整个快照解码失败、面板落到「路由不可用」
  （2026-08 harnessPublished 事故）。cmd/codex-router/control.go 1052 行
  混装命令分发、契约生成与两种服务生命周期；Swift 侧 4784 行单文件
  混装契约解码、control 执行、状态机与视图。

## 决策

1. `internal/controlplane` 拥有控制面契约：类型化 Snapshot 族
   （Snapshot/Target/ProviderInfo/ModelInfo/ModelSettings）与
   `control --json` 输出的字段存在性互为锚点；presence/subagents/
   picker/visionBridge 子块以 RawMessage 注入，形状归各生成器。
2. 服务生命周期显式双路径、所有权互斥：
   - App 托管 = Swift `ServiceSupervisor`（子进程，App 退出即退出）；
   - 终端救急 = Go `controlplane.Service`（detached 启动 + pidfile 停止）。
   谁拉起的进程归谁管；StopByPidfile 对两种来源都能停，但无 pidfile
   且健康的服务拒绝盲停。
3. Swift 侧四文件拆分：契约解码（ControlContract.swift）、生命周期
   （ServiceLifecycle.swift）、control 进程执行（ControlClient.swift，
   resolveBinary 注入点）、状态机与视图（ModelRouterTrayApp.swift）。
4. provider 展示顺序用 `OrderedProviders`：固定名单锚定本 fork 默认
   支持面（zai-coding/opencode-go/litellm）；用户显式启用的白名单外
   provider 按字母序追加。注册表携带 models.dev 生态的 28 个参考
   provider 定义 —— 未启用的一律不进设置页（部署实测曾因无门槛
   追加导致快照从 3 家膨胀到 30 家，已用契约测试钉住）。
5. 契约测试钉住两个历史缺陷：空数组必须输出 `[]` 而非 `null`
   （Swift `[T]` 解码 null 必炸）；multiAgentVersion 仅 v2 裁决后出现。

## 后果

- 加契约字段改 controlplane struct + Swift 同步，编译期可见；
  顺序与 null 形状由 contract_test 钉死。
- control.go 缩为命令分发与 IO 装饰（约 870 行）。
- 命令面保持 argv 字符串（tray 的稳定外部接口），不做 command enum：
  分发天然属于 CLI 适配层，类型化收益已由 Snapshot/Service 承接。
- Swift RouterStore 状态机本体保留在主文件：调用交织度高，
  拆分收益递减，等真实变更压力再动。
