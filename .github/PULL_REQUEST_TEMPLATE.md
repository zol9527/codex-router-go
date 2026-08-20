## 📝 变更描述
<!-- 简要描述此 PR 的内容 -->

## 🏷️ 类型
- [ ] ✨ 新功能
- [ ] 🐛 Bug 修复
- [ ] 🔀 wire 协议 / 翻译层（Responses ↔ chat-completions、codex app tools）
- [ ] 🗂️ provider 注册表 / 模型 catalog
- [ ] 🔧 重构（不改变外部行为）
- [ ] 🍎 macOS App / Swift（apps/macos）
- [ ] ⚡ 性能优化
- [ ] 🔒 安全修复（认证、凭证、key 处理）
- [ ] ✅ 测试
- [ ] 📚 文档（README / docs/）
- [ ] 📦 依赖更新
- [ ] 🚀 构建 / 部署（Makefile、scripts/）

## 📋 变更内容
<!-- 列出主要变更点；涉及协议或配置格式时说明前后差异 -->

## 🔗 关联 Issue
<!-- 关闭相关 Issue；无则留空 -->
Closes #

## ✅ 检查清单
- [ ] `make test` 全量通过（Go）
- [ ] 若改动了 Swift 代码，`make test-app` 通过
- [ ] `make cli`（或 `make app`，涉及 App 时）可成功构建
- [ ] 已添加/更新相关测试——wire 翻译、路由、凭证等行为变更须有测试覆盖
- [ ] 未提交二进制或 `dist/` 产物（构建产物统一落 `dist/`，不进仓库）
- [ ] 若改了 `config.toml` 格式或 provider 注册表：旧配置兼容 / 有迁移说明，并更新了 README 或 docs/
- [ ] 若替换了运行中的二进制逻辑：确认走原子 `mv`（避免 macOS 代码签名 SIGKILL）

## 🧪 验证方式
<!-- 描述如何手动验证：如 doctor 输出、picker 中选路由模型、实跑一次 Codex 请求等 -->

## 📸 截图/演示
<!-- 若涉及 App UI 或模型 picker 变化，请附截图/录屏 -->

## 💬 备注
<!-- 其他需要说明的信息 -->

## 🔍 Review 重点关注
<!-- 告诉 Reviewer 重点关注哪些部分（如协议边界、error path、并发安全） -->
