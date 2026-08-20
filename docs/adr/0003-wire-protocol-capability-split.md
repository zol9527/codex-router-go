# ADR 0003: wire 协议接口按能力拆分

- 状态: 已接受（2026-08-20）
- 背景: 架构评审候选 4（Worth exploring）。`wire.Protocol` 曾要求全部
  7 个方法：responses 直通协议对 NewStreamTranslator/TranslateNonStream
  只能 panic 占位；ApplyRequestProfile 两个实现都是空操作（chat 在
  Prepare 内部施加画像）；RunCompaction 无条件调用翻译方法，直通
  provider 一配即 panic。

## 决策

1. `wire.Protocol` 缩为请求方向三方法：Name / Prepare /
   NeedsResponseTranslation。画像施加（effort 阶梯、参数清洗）是
   Prepare 的私有阶段，不再外露为接口方法。
2. 响应翻译拆为 `wire.ResponseTranslator`（NewStreamTranslator /
   TranslateNonStream）：翻译协议实现，直通协议不实现 —— 每个协议
   只承诺自己做得到的事，panic 占位删除。
3. `wire.TranslatorFor` 断言翻译能力：NeedsResponseTranslation=true
   却未实现 ResponseTranslator 即显式错误，不静默降级。
4. RunCompaction 显式分流：直通协议的压缩响应本来就是 Responses
   形状，直接提取摘要；翻译协议才走 TranslateNonStream
   （routing/compaction_test.go 钉住）。

## 后果

- 接口诚实：新增直通协议零翻译代码；新增翻译协议漏实现会在
  运行分发点得到指名道姓的错误。
- 画像与协议翻译解耦：profile 逻辑全在 translate 层，adapter 组合。
- 消除了一个真实 panic 隐患（直通协议配 compaction 模型）。
