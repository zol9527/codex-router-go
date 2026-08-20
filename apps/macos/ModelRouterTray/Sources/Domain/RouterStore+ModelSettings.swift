import AppKit
import Combine
import Foundation
import ServiceManagement
import SwiftUI

extension RouterStore {

  func setIslandMode(_ mode: IslandMode) {
    islandMode = mode
    defaults.set(mode.rawValue, forKey: islandModeKey)
  }

  func setLanguage(_ next: TrayLanguage) {
    guard next != language else { return }
    // RouterLanguage holds the value routerLocalized() reads, so it has to be
    // updated before the published change re-renders anything.
    RouterLanguage.setSelection(next)
    language = next
  }

  // Every vendor quota window the desktop panel can show at a glance:

  func setSubagentMode(_ mode: String) async {
    await applyModelSettings(arguments: ["subagents", "mode", mode])
  }

  func setSubagentModel(_ slug: String, enabled: Bool) async {
    await applyModelSettings(
      arguments: ["subagents", "set", slug, enabled ? "on" : "off"]
    )
  }

  // 本地 v2 声明（用户主权通道）：未证明的路由模型由此提为分身候选。
  func declareSubagentModel(_ slug: String) async {
    await applyModelSettings(arguments: ["subagents", "declare", slug])
  }

  func undeclareSubagentModel(_ slug: String) async {
    await applyModelSettings(arguments: ["subagents", "undeclare", slug])
  }

  func setSubagentProvider(_ provider: String, enabled: Bool) async {
    await applyModelSettings(
      arguments: ["subagents", "provider", provider, enabled ? "on" : "off"]
    )
  }

  func setPickerModel(_ slug: String, visible: Bool) async {
    await applyModelSettings(
      arguments: ["picker", "set", slug, visible ? "show" : "hide"]
    )
  }

  func setPickerProvider(_ provider: String, visible: Bool) async {
    await applyModelSettings(
      arguments: ["picker", "provider", provider, visible ? "show" : "hide"]
    )
  }

  func selectAllSubagents() async {
    await applyModelSettings(arguments: ["subagents", "select-all"])
  }

  func unselectAllSubagents() async {
    await applyModelSettings(arguments: ["subagents", "unselect-all"])
  }

  func showAllPickerModels() async {
    await applyModelSettings(arguments: ["picker", "all", "show"])
  }

  func hideAllPickerModels() async {
    await applyModelSettings(arguments: ["picker", "all", "hide"])
  }

  func setVisionBridgeEnabled(_ enabled: Bool) async {
    await applyModelSettings(arguments: ["vision-bridge", enabled ? "on" : "off"])
  }

  /// Measures Ollama's own eval counters, so the number is this machine's
  /// observed generation speed rather than a marketing estimate.
  func benchmarkLocalModelSpeed(_ tag: String) async {
    guard benchmarkingTag == nil else { return }
    benchmarkingTag = tag
    defer { benchmarkingTag = nil }
    do {
      _ = try await runControl(arguments: ["local-models", "benchmark", tag])
      await refresh()
      message = "\(tag) speed measured. Tokens per second is on its row."
    } catch {
      message = error.localizedDescription
    }
  }

  /// Downloads a local chat model through Ollama, installs/starts Ollama when
  /// needed, and checks the model on for Codex after the pull completes. The
  /// control command returns immediately; the state file is polled so the
  /// tray remains responsive during multi-gigabyte downloads.
  func applyModelSettings(
    arguments: [String],
    successMessage: String = "Model settings applied. Restart Codex to refresh its picker."
  ) async {
    guard providerOperation == nil else { return }
    providerOperation = "models"
    defer { providerOperation = nil }
    do {
      _ = try await runControl(arguments: arguments)
      await refresh()
      message = successMessage
    } catch {
      message = error.localizedDescription
      await refresh()
    }
  }
}
