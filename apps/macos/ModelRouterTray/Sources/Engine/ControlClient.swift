// ControlClient.swift —— tray 侧的 codex-router 控制面客户端。
//
// 职责仅限"把一条 control 命令变成进程并交回输出"：二进制解析走
// RouterProcessLocator，参数按 needsControlPrefix 补 "control" 前缀，
// stdin 管道供 credential 写入，非零退出把 stderr 摘要成 RouterError。
// 状态机（快照应用、轮询、@Published）不在此 —— 那是 RouterStore 的事；
// 契约解码在 ControlContract.swift。三者分离后，client 可脱离 UI 独立
// 测试参数拼装与错误路径。

import Foundation

final class RouterControlClient {
  /// 二进制解析注入点：默认走 RouterProcessLocator 的真实回退链
  ///（bundle 内嵌 → 开发 checkout → ~/bin）；测试覆写以指向假命令。
  var resolveBinary: () throws -> RouterProcessLocator.Resolved = {
    try RouterProcessLocator.shared.resolve()
  }

  /// 执行一条 control 子命令，返回 stdout。非零退出时抛出携带
  /// stderr 摘要的 RouterError（UI 经 store.message 呈现）。
  func run(_ arguments: [String], stdin: Data? = nil) async throws -> Data {
    let router = try resolveBinary()
    return try await Task.detached {
      let task = Process()
      task.executableURL = router.url
      // 直接二进制需要 "control" 前缀；bin/control 包装器已含它。
      task.arguments = router.needsControlPrefix ? ["control"] + arguments : arguments
      task.currentDirectoryURL = router.url.deletingLastPathComponent()
      var environment = ProcessInfo.processInfo.environment
      let home = FileManager.default.homeDirectoryForCurrentUser.path
      let preferredPaths = [
        "\(home)/.npm-global/bin",
        "\(home)/.local/bin",
        "/opt/homebrew/bin",
        "/usr/local/bin",
      ]
      environment["PATH"] = (preferredPaths + [environment["PATH"] ?? ""]).joined(separator: ":")
      task.environment = environment
      let output = Pipe()
      let errors = Pipe()
      let input = stdin.map { _ in Pipe() }
      task.standardOutput = output
      task.standardError = errors
      task.standardInput = input
      try task.run()
      let stdoutReader = Task.detached {
        output.fileHandleForReading.readDataToEndOfFile()
      }
      let stderrReader = Task.detached {
        errors.fileHandleForReading.readDataToEndOfFile()
      }
      if let stdin, let input {
        input.fileHandleForWriting.write(stdin)
        try? input.fileHandleForWriting.close()
      }
      task.waitUntilExit()
      let stdout = await stdoutReader.value
      let stderr = await stderrReader.value
      guard task.terminationStatus == 0 else {
        let detail = String(data: stderr, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines)
        throw RouterError(detail?.isEmpty == false ? detail! : "Model Router control command failed.")
      }
      return stdout
    }.value
  }
}
