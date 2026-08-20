# ADR 0001: 深化 Routing Turn 模块并建立 Native Backend 适配器

- 状态: 已接受（2026-08-20）
- 背景: 架构评审候选 1、2（Strong）。`internal/server` 曾同时承担 HTTP transport、
  协作归一化、视觉桥、工具/namespace 适配、失败策略与 usage 计量；
  native 端点、auth.json 会话与 Codex 传输细节散落在 native/agentrelay/vision 三处。

## 决策

1. `internal/routing.Runner` 拥有一次完整 Routing Turn：输入归一化（注入）、
   视觉桥（注入）、工具与 namespace 适配、wire 协议调用、流式守卫
   （StreamRelay hold/释放语义）、失败收尾与 usage 计量。主回合与压缩
   共享私有 implementation（Run / RunCompaction）。
2. server 只做 HTTP transport：认证、请求解码、把 ResponseWriter 适配为
   routing.Sink。协作中继与读图桥因依赖会话状态，以 NormalizeInput /
   BridgeVision 回调注入，routing 不直接持有 nativebackend。
3. `internal/nativebackend` 是原生 ChatGPT/Codex 后端的唯一适配器：
   endpoint、白名单头、auth.json 会话兜底、/responses 调用与图片转写
   全部私有化，对外只暴露 Client 接口（Relay/PostResponses/Describe/
   Headers/CallerHasCredential）。

## 后果

- turn 变更集中在 routing 一个模块；native 细节改动只碰 nativebackend。
- server 对上游失败/协议差异零感知（UpstreamFailure 事实 + 注入的错误翻译）。
- 既有 HTTP E2E 测试钉住迁移行为，外部 Responses 行为零变化。
- 代价：Runner 以回调注入会话依赖（NormalizeInput 等），接缝多但每个
  都是单一函数，测试可替换。
