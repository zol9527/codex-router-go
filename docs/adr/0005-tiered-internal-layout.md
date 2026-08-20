# ADR 0005: internal/ 四层目录（lib / domain / engine / app）与车道护栏

- 状态: 已接受（2026-08-20）
- 背景: 架构评审（四候选报告）确认 internal/ 17 包平铺一层，真实
  依赖图是无环 DAG 但分层只活在 import 语句里，目录与包名不表达它；
  `configfile` 名字语义误导（管的是 Codex 侧 ~/.codex/config.toml，
  不是本路由器的配置）；TOML 转义与上游地址解析规则各存在双份实现。

## 决策

1. internal/ 按语义四层组织，依赖边只允许朝下（或留在层内）：
   - `lib/` 工具层（httpx、tomlconf、modelmeta）：共享技术实现的
     单点归宿，不 import 任何 internal 包；
   - `domain/` 抽象层（registry、state、cred、translate、wire+子包、
     usage、vision）：系统**知道什么** —— 领域数据、接缝、翻译规则；
   - `engine/` 操作层（routing、nativebackend、catalog、discover）：
     系统**做什么** —— 拥有任务生命周期的多步执行器；
   - `app/` 语义层（server、controlplane、codexconfig、cli）：对外
     表面与契约；只有 cmd 依赖 app。
   层归属判据："知道什么 vs 做什么" —— domain 模块无任务生命周期
   （translate 是纯函数规则库、registry 是数据+接缝），engine 模块
   拥有完整任务流程（routing 拥有一次 Routing Turn、catalog 拥有
   刷新→构建→发布、discover 拥有探测→注册、nativebackend 拥有
   上游调用）。
2. `configfile` → `codexconfig`（目录与包名同步改）。
3. 反冗余纪律：公共技术实现只允许一份且在 lib。首批收敛点：
   TOML basic string 转义（`tomlconf.Quote`，顺带修复 `%q` 产出
   `\x..` 非 TOML 合法形状的潜伏缺陷）与上游地址解析
   （`registry.ResolveBaseURL`）。
4. `cmd/codex-router` 缩为薄入口：只剩版本注入（构建时
   `-X main.version` 打在 package main）与进程入口；命令实现与分发
   在 `internal/app/cli`，`cli.Main(version)` 是唯一导出面，持有
   进程退出语义。
5. 车道规则由 `internal/arch` 测试钉住（go/parser 扫 import，
   随 `make test` 常跑）：违规 = 红测试，指名道姓文件与方向。

## 后果

- 新包归属按车道规则判定，不再靠评审记忆；分层不会被"顺手 import"
  悄悄侵蚀。
- 与 Go 平铺惯例的权衡已明示：只加一层分组、组内仍是行为命名的
  平包，不引入 domain/usecase/infra 式层层嵌套；买到的是 19 包规模
  的 DAG 可见性与 AI 导航性。
- 历史文档（ADR 0001-0004 正文中的旧路径）保持原样 —— 它们是
  记录不是规范；活文档（docs/architecture/README.md）已同步新路径。
