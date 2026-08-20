import AppKit
import Combine
import Foundation
import ServiceManagement
import SwiftUI

extension RouterStore {
  // install-cli is a no-op when the CLI is already present, so an unknown
  // state costs a lookup rather than a wrong branch.
  func connectProvider(_ provider: String) async {
    let reconnecting = providerSetup[provider]?.configured == true
    let needsInstall = providerSetup[provider]?.cliInstalled != true
    await performProviderOperation(
      provider,
      successMessage: reconnecting
        ? "Provider reconnected."
        : "Provider connected. Restart Codex to refresh its model picker."
    ) {
      if needsInstall {
        _ = try await runControl(arguments: ["install-cli", provider])
      }
      _ = try await runControl(arguments: ["login", provider])
      if !reconnecting {
        try await updateProviderSelection(provider, enabled: true)
      }
    }
  }

  func loginProvider(_ provider: String) async {
    let reconnecting = providerSetup[provider]?.configured == true
    await performProviderOperation(
      provider,
      successMessage: reconnecting
        ? "Provider reconnected."
        : "Provider connected. Restart Codex to refresh its model picker."
    ) {
      _ = try await runControl(arguments: ["login", provider])
      if !reconnecting {
        try await updateProviderSelection(provider, enabled: true)
      }
    }
  }

  func saveProviderKey(_ provider: String, key: String) async {
    let secret = Data(key.utf8)
    let label = providerSetup[provider]?.credentialLabel ?? "API key"
    await performProviderOperation(
      provider,
      successMessage: "\(label) saved. Restart Codex to refresh its model picker."
    ) {
      _ = try await runControl(arguments: ["credential", provider], stdin: secret)
      try await updateProviderSelection(provider, enabled: true)
    }
  }

  // The control plane already drops the provider from the Codex selection when
  // the key file is deleted; this only makes that selection live.
  func removeProviderKey(_ provider: String) async {
    let label = providerSetup[provider]?.credentialLabel ?? "API key"
    await performProviderOperation(
      provider,
      successMessage: "\(label) removed. Restart Codex to refresh its model picker."
    ) {
      _ = try await runControl(arguments: ["credential", provider, "--remove"])
      _ = try? await runControl(arguments: ["apply", "--targets", "codex", "--activate"])
    }
  }



  func setProvider(_ provider: String, enabled: Bool) async {
    guard providerOperation == nil else { return }
    providerOperation = provider
    defer { providerOperation = nil }
    do {
      try await updateProviderSelection(provider, enabled: enabled)
      await refresh()
      await refreshProviderUsage()
      message = enabled
        ? "Provider added. Restart Codex to refresh its model picker."
        : "Provider hidden. Restart Codex to refresh its model picker."
    } catch {
      message = error.localizedDescription
      await refresh()
    }
  }

  func updateAndVerify() async {
    guard providerOperation == nil else { return }
    providerOperation = "maintenance"
    maintenanceMessage = "Running update and doctor…"
    maintenanceSucceeded = false
    defer { providerOperation = nil }
    do {
      _ = try await runControl(arguments: ["maintenance"])
      await refresh()
      await refreshAccountUsage()
      await refreshProviderUsage()
      await refreshProviderSetup()
      maintenanceSucceeded = true
      maintenanceMessage = "Update installed. Fully quit and reopen Codex to load updated models and agents."
    } catch {
      maintenanceMessage = error.localizedDescription
      await refresh()
    }
  }


  func fixAndVerify() async {
    guard providerOperation == nil else { return }
    providerOperation = "doctor"
    maintenanceMessage = "Running doctor --fix…"
    maintenanceSucceeded = false
    defer { providerOperation = nil }
    do {
      _ = try await runControl(arguments: ["doctor", "--fix"])
      await refresh()
      await refreshAccountUsage()
      await refreshProviderUsage()
      await refreshProviderSetup()
      maintenanceSucceeded = true
      maintenanceMessage = "Repair verified. Fully quit and reopen Codex if models changed."
    } catch {
      maintenanceMessage = error.localizedDescription
      await refresh()
    }
  }

  func setLoginFree(_ enabled: Bool) async {
    guard providerOperation == nil else { return }
    providerOperation = "auth-mode"
    defer { providerOperation = nil }
    do {
      _ = try await runControl(arguments: ["auth-mode", enabled ? "on" : "off"])
    } catch {
      let errorMessage = error.localizedDescription
      await refresh()
      message = errorMessage
      return
    }

    await refresh()
    do {
      try await restartCodexDesktopApp()
      message = enabled
        ? "Codex restarted with external-provider mode."
        : "Codex restarted with OpenAI login restored."
    } catch {
      message = "Mode changed, but Codex could not restart: \(error.localizedDescription)"
    }
  }

  func setSignedRouting(_ enabled: Bool) async {
    guard providerOperation == nil else { return }
    providerOperation = "signed-routing"
    defer { providerOperation = nil }
    do {
      _ = try await runControl(arguments: ["signed-routing", enabled ? "on" : "off"])
      await refresh()
      message = enabled
        ? "Router with ChatGPT enabled. Fully quit and reopen Codex when ready."
        : "Previous provider restored. Fully quit and reopen Codex when ready."
    } catch {
      await refresh()
      message = error.localizedDescription
    }
  }



  func performProviderOperation(
    _ provider: String,
    successMessage: String,
    operation: () async throws -> Void
  ) async {
    guard providerOperation == nil else { return }
    providerOperation = provider
    defer { providerOperation = nil }
    do {
      try await operation()
      await refreshProviderSetup()
      await refresh()
      await refreshProviderUsage()
      message = successMessage
    } catch {
      message = error.localizedDescription
      await refreshProviderSetup()
    }
  }

  func updateProviderSelection(_ provider: String, enabled: Bool) async throws {
    let wasEnabled = snapshot.targets["codex"]?.enabledProviders.contains(provider) == true
    _ = try await runControl(
      arguments: ["set", provider, enabled ? "on" : "off", "--targets", "codex"]
    )
    do {
      _ = try await runControl(arguments: ["apply", "--targets", "codex", "--activate"])
    } catch {
      _ = try? await runControl(
        arguments: ["set", provider, wasEnabled ? "on" : "off", "--targets", "codex"]
      )
      _ = try? await runControl(arguments: ["apply", "--targets", "codex", "--activate"])
      throw error
    }
  }
}
