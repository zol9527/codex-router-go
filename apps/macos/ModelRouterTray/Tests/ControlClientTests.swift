import XCTest
@testable import ModelRouterTray

// ControlClient 的参数拼装与错误路径契约：
//   - 直接二进制（needsControlPrefix=true）补 "control" 前缀；
//   - bin/control 包装器（false）不补；
//   - 非零退出把 stderr 摘要带进 RouterError。
final class ControlClientTests: XCTestCase {
  private var workDir: URL!

  override func setUpWithError() throws {
    workDir = FileManager.default.temporaryDirectory
      .appendingPathComponent("control-client-tests-\(UUID().uuidString)")
    try FileManager.default.createDirectory(at: workDir, withIntermediateDirectories: true)
  }

  override func tearDownWithError() throws {
    try? FileManager.default.removeItem(at: workDir)
  }

  /// 写一个可执行 shell 脚本并返回其 URL。
  private func writeScript(_ body: String) throws -> URL {
    let url = workDir.appendingPathComponent("fake-router-\(UUID().uuidString).sh")
    try "#!/bin/sh\n\(body)".write(to: url, atomically: true, encoding: .utf8)
    try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: url.path)
    return url
  }

  private func makeClient(url: URL, needsControlPrefix: Bool) -> RouterControlClient {
    let client = RouterControlClient()
    client.resolveBinary = {
      RouterProcessLocator.Resolved(url: url, needsControlPrefix: needsControlPrefix)
    }
    return client
  }

  func testDirectBinaryGetsControlPrefix() async throws {
    let script = try writeScript("printf '%s\\n' \"$*\"\n")
    let client = makeClient(url: script, needsControlPrefix: true)
    let out = try await client.run(["set", "zai-coding", "on"])
    XCTAssertEqual(
      String(data: out, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines),
      "control set zai-coding on"
    )
  }

  func testWrapperBinaryKeepsArgumentsVerbatim() async throws {
    let script = try writeScript("printf '%s\\n' \"$*\"\n")
    let client = makeClient(url: script, needsControlPrefix: false)
    let out = try await client.run(["service", "status"])
    XCTAssertEqual(
      String(data: out, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines),
      "service status"
    )
  }

  func testNonZeroExitSurfacesStderrDetail() async {
    do {
      let script = try! writeScript("echo 'provider not found' >&2\nexit 3\n")
      let client = makeClient(url: script, needsControlPrefix: true)
      _ = try await client.run(["providers"])
      XCTFail("expected RouterError")
    } catch let error as RouterError {
      XCTAssertTrue(
        error.localizedDescription.contains("provider not found"),
        "stderr detail should surface, got: \(error.localizedDescription)"
      )
    } catch {
      XCTFail("unexpected error type: \(error)")
    }
  }
}
