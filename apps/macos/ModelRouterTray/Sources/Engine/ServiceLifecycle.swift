// ServiceLifecycle.swift —— 服务的进程级生命周期。
//
// 两条生命周期路径的所有权边界（操作者契约）：
//   - App 托管：ServiceSupervisor 以子进程形态拉起 serve，App 退出它
//     也退出（3s 优雅 + SIGKILL 硬边界），崩溃按指数退避重拉。
//   - 终端救急：Go 侧 controlplane.Service 的 detached 启动与 pidfile
//     停止（`control service`），不经本文件。
// RouterProcessLocator 负责 codex-router 二进制的解析（bundle 内嵌 →
// 开发 checkout → ~/bin 回退链）。Codex 桌面 App 的重启（设置页
// 「修复并重启」路径）也归这里 —— 进程/应用生命周期聚在一处。

import AppKit
import Foundation

/// 重启 Codex 桌面 App：优雅退出 → 最多 5s 等待 → 重新拉起。
/// 纯 NSWorkspace 操作，不触碰 RouterStore 状态。
func restartCodexDesktopApp() async throws {
  let bundleIdentifier = "com.openai.codex"
  let workspace = NSWorkspace.shared
  let runningApplications = NSRunningApplication.runningApplications(
    withBundleIdentifier: bundleIdentifier
  )
  let applicationURL = runningApplications.compactMap(\.bundleURL).first
    ?? workspace.urlForApplication(withBundleIdentifier: bundleIdentifier)

  guard let applicationURL else {
    throw RouterError("the Codex desktop app could not be found")
  }

  for application in runningApplications where !application.isTerminated {
    guard application.terminate() else {
      throw RouterError("Codex did not accept a graceful quit request")
    }
  }

  for _ in 0..<50 {
    if runningApplications.allSatisfy({ $0.isTerminated }) { break }
    try await Task.sleep(nanoseconds: 100_000_000)
  }

  guard runningApplications.allSatisfy({ $0.isTerminated }) else {
    throw RouterError("Codex did not quit in time; restart it manually")
  }

  let configuration = NSWorkspace.OpenConfiguration()
  configuration.activates = true
  try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
    workspace.openApplication(at: applicationURL, configuration: configuration) { _, error in
      if let error {
        continuation.resume(throwing: error)
      } else {
        continuation.resume(returning: ())
      }
    }
  }
}


// 路由器可执行文件的解析。发行形态：二进制内嵌在 App bundle 的
// MacOS/ 里（App 化的核心 —— .app 即部署单元）。回退链：
// ModelRouterSourceRoot/bin/control（开发 checkout）、~/bin/codex-router
//（操作者布局）。任选其一可用即走。
final class RouterProcessLocator {
  static let shared = RouterProcessLocator()

  struct Resolved {
    let url: URL
    let needsControlPrefix: Bool
  }

  private enum Candidate {
    case direct(URL)      // codex-router 二进制
    case wrapper(URL)     // bin/control 脚本
  }

  func resolve() throws -> Resolved {
    switch firstCandidate() {
    case .direct(let url): return Resolved(url: url, needsControlPrefix: true)
    case .wrapper(let url): return Resolved(url: url, needsControlPrefix: false)
    case nil:
      throw RouterError("Cannot find the codex-router binary. Rebuild the app from the router repository.")
    }
  }

  private func firstCandidate() -> Candidate? {
    let fm = FileManager.default
    // 1. bundle 内嵌二进制（发行形态）。
    if let bundleURL = Bundle.main.executableURL?.deletingLastPathComponent()
      .appendingPathComponent("codex-router"),
      fm.isExecutableFile(atPath: bundleURL.path) {
      return .direct(bundleURL)
    }
    // 2. ModelRouterSourceRoot 的 bin/control（开发 checkout 构建）。
    if let configured = Bundle.main.object(forInfoDictionaryKey: "ModelRouterSourceRoot") as? String,
      !configured.isEmpty {
      let control = URL(fileURLWithPath: configured, isDirectory: true)
        .standardizedFileURL.resolvingSymlinksInPath()
        .appendingPathComponent("bin/control")
      if fm.isExecutableFile(atPath: control.path) {
        return .wrapper(control)
      }
    }
    // 3. 操作者布局 ~/bin/codex-router。
    let homeBinary = FileManager.default.homeDirectoryForCurrentUser
      .appendingPathComponent("bin/codex-router")
    if fm.isExecutableFile(atPath: homeBinary.path) {
      return .direct(homeBinary)
    }
    return nil
  }
}


// ServiceSupervisor：App 化后的服务生命周期所有者。
//
// 契约（操作者拍板）：
//   - App 打开 → 探活 /health，没跑才 spawn serve 子进程（托管形态）
//   - 子进程意外退出 → 探活：端口仍健康说明外部实例接管（bind 冲突
//     是我们输掉了），不再重拉；不健康则退避重拉（上限后放弃并上报）
//   - App 退出 → SIGTERM 子进程等优雅排空（最长 3s），仍活着 SIGKILL
//     —— 服务随 App 走是硬约束，孤儿进程比硬杀更违背契约
//   - 外部实例（~/bin CLI 拉起）不归我们托管，退出时通过 control
//     service stop（pidfile SIGTERM）请它退场
final class ServiceSupervisor {
  static let shared = ServiceSupervisor()

  private let queue = DispatchQueue(label: "router.service-supervisor")
  private var child: Process?
  private var quitting = false
  private var restartAttempts = 0
  private var onUnrecoverable: ((String) -> Void)?

  private var healthURL: URL {
    let port = ProcessInfo.processInfo.environment["MODEL_ROUTER_PORT"] ?? "4202"
    return URL(string: "http://127.0.0.1:\(port)/health")!
  }

  private var stateDir: String {
    ProcessInfo.processInfo.environment["CODEX_ROUTER_STATE_DIR"]
      ?? FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".codex-router").path
  }

  func startIfNotRunning() async {
    guard !quitting else { return }
    if await probeHealthy() { return } // 外部实例在服务，别抢
    queue.sync {
      guard self.child == nil || !self.child!.isRunning else { return }
      self.spawnChild()
    }
  }

  func stopForAppQuit() {
    queue.sync {
      quitting = true
      guard let child, child.isRunning else { return }
      child.terminate() // SIGTERM → Go 侧 10s 优雅排空
      // App 退出是硬边界：给 3 秒优雅，之后强杀 —— 子进程绝不许
      // 活过 App（孤儿服务违背 "App 退出即服务退出" 的契约）。
      let deadline = Date().addingTimeInterval(3)
      while child.isRunning && Date() < deadline {
        Thread.sleep(forTimeInterval: 0.05)
      }
      if child.isRunning {
        kill(child.processIdentifier, SIGKILL)
      }
    }
    // 服务不是我们拉起的（外部 ~/bin CLI）：请 pidfile 里的进程退场。
    // 分离执行，退出不等待。
    if !queue.sync(execute: { child?.isRunning ?? false }) {
      let task = Process()
      do {
        let router = try RouterProcessLocator.shared.resolve()
        task.executableURL = router.url
        task.arguments = router.needsControlPrefix
          ? ["control", "service", "stop"] : ["service", "stop"]
        try? task.run()
      } catch {
        // 找不到二进制时无事可做 —— 我们本就没拉起过服务。
      }
    }
  }

  private func spawnChild() {
    guard let router = try? RouterProcessLocator.shared.resolve() else {
      reportUnrecoverable("Cannot find the codex-router binary inside the app.")
      return
    }
    let task = Process()
    task.executableURL = router.url
    task.arguments = ["serve", "--state", stateDir]
    task.standardOutput = appLogFile()
    task.standardError = appLogFile()
    task.terminationHandler = { [weak self] exited in
      self?.queue.async { self?.childExited(exited) }
    }
    do {
      try task.run()
      child = task
      supervisorLog("spawned serve pid \(task.processIdentifier)")
    } catch {
      supervisorLog("spawn failed: \(error.localizedDescription)")
      reportUnrecoverable("Failed to start the router service: \(error.localizedDescription)")
    }
  }

  // 子进程退出。quitting 是我们主动停（App 退出）；其余按健康度分诊。
  private func childExited(_ process: Process) {
    if process !== child { return }
    child = nil
    guard !quitting else { return }
    queue.asyncAfter(deadline: .now() + .milliseconds(300)) { [weak self] in
      // 端口仍健康 = 外部实例在服务（我们 bind 输了或被顶替）：
      // 不重拉，避免双实例互踩。
      Task { [weak self] in
        guard let self else { return }
        if await self.probeHealthy() {
          self.supervisorLog("child exited but port healthy — external instance took over")
          return
        }
        self.queue.async { self.scheduleRestart() }
      }
    }
  }

  private func scheduleRestart() {
    guard !quitting else { return }
    restartAttempts += 1
    guard restartAttempts <= 5 else {
      supervisorLog("crashed \(restartAttempts - 1) times; giving up")
      reportUnrecoverable("Router service crashed \(restartAttempts - 1) times; giving up. Check router.log.")
      return
    }
    // 1s → 2s → 4s → 8s → 16s：崩溃风暴不该把 CPU 点着。
    let delay = Double(1 << (restartAttempts - 1))
    supervisorLog("crash restart attempt \(restartAttempts) in \(delay)s")

    queue.asyncAfter(deadline: .now() + delay) { [weak self] in
      guard let self, !self.quitting, self.child == nil else { return }
      Task { await self.startIfNotRunning() }
    }
  }

  private func probeHealthy() async -> Bool {
    var request = URLRequest(url: healthURL)
    request.cachePolicy = .reloadIgnoringLocalCacheData
    request.timeoutInterval = 2
    guard let (_, response) = try? await URLSession.shared.data(for: request) else { return false }
    return (response as? HTTPURLResponse)?.statusCode == 200
  }

  // router.log 追加句柄。serve 自身的日志（listen/错误）落这里，与
  // 旧 launchd StandardErrorPath 行为一致。
  /// Supervisor 事件落 router.log —— 下次"没拉起服务"不再是无证据
  /// 悬案（2026-08-15 14:52 那次就查不到原因）。
  private func supervisorLog(_ message: String) {
    let line = "[supervisor] \(message)\n"
    let handle = appLogFile()
    if handle != FileHandle.nullDevice {
      _ = try? handle.write(contentsOf: Data(line.utf8))
    }
  }

  private var logHandle: FileHandle?
  private func appLogFile() -> FileHandle {
    if let logHandle { return logHandle }
    let path = (stateDir as NSString).appendingPathComponent("router.log")
    let fm = FileManager.default
    // 启动轮转：超过 2MB 归档为 router.log.1（单代，覆盖旧归档）。
    // 只能在这里做 —— 句柄一旦创建（并被 serve 子进程继承 fd），
    // 运行中改名会割裂 tray 与 serve 两路写入。首次调用早于 spawn，
    // 因此每次 App 运行的轮转点必然在子进程接管句柄之前。
    if let size = (try? fm.attributesOfItem(atPath: path)[.size]) as? Int, size > 2_000_000 {
      try? fm.removeItem(atPath: path + ".1")
      try? fm.moveItem(atPath: path, toPath: path + ".1")
    }
    if !fm.fileExists(atPath: path) {
      fm.createFile(atPath: path, contents: nil, attributes: [.posixPermissions: 0o600])
    }
    let handle = FileHandle(forWritingAtPath: path)
    _ = try? handle?.seekToEnd()
    logHandle = handle
    return handle ?? FileHandle.nullDevice
  }

  private func reportUnrecoverable(_ message: String) {
    Task { @MainActor in
      RouterStore.shared.publishNotice(message)
    }
  }
}


struct RouterError: LocalizedError {
  let message: String
  init(_ message: String) { self.message = message }
  var errorDescription: String? { message }
}
