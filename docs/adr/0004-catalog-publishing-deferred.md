# ADR 0004: catalog 计算与发布分离——暂缓

- 状态: 已接受（2026-08-20，暂缓实施）
- 背景: 架构评审候选 5（Speculative）。`catalog.Refresh` 把原生抓取
  （`codex debug models`）、状态读取、子代理裁决、构建、写盘与
  agents 目录同步装在一个函数里；`subagents.go` 直接写 Codex agents 目录。

## 决策

保持现状，不拆 Build(inputs) / publisher / source 适配器。

理由：评审结论是"仅在 CLI surface 持续膨胀时才动"（Build 已是有用的
纯核心，磁盘/进程副作用集中在 Refresh 一个入口）。当前无膨胀证据：
catalog 近 180 天 10 次变更，低于候选 1-3 的热点（21-122 次），
且每次变更都在 Refresh 内部完成、未扩散。

## 后果

- 触发重评条件（满足其一）：
  1. Refresh 需要第二种发布目标（如非 Codex 的 agents 目录布局）；
  2. 原生抓取需要第二个 source（如离线 fixture 回放）进入测试；
  3. catalog 相关变更开始跨越 refresh/sync 两个以上调用方反复修改。
- 在此之前，改动 catalog 一律在 Refresh 内部深化，不引入新目录。

## 注记（2026-08-20）

ADR-0005 把 catalog 整包迁至 `internal/engine/catalog`。这不属于本 ADR
禁止的"引入新目录"——该措辞的意图是阻止 Build/publisher/source 概念
拆分；整包搬家零行为变更，Refresh 内部深化的纪律不变。
