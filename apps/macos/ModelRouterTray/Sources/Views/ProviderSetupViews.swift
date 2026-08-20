import AppKit
import Combine
import Foundation
import ServiceManagement
import SwiftUI

// 触发移除确认的宽限窗口（秒）：防止误触 arm 状态立刻执行。
let removalArmWindow: TimeInterval = 4

struct ProviderSetupRow: View {
  let provider: (id: String, enabled: Bool)
  let setup: ProviderSetupState?
  let account: ProviderAccountUsage?
  let isBusy: Bool
  let controlsDisabled: Bool
  let onToggle: (Bool) -> Void
  let onConnect: () -> Void
  let onLogin: () -> Void
  let onSaveKey: (String) -> Void
  let onRemoveKey: () -> Void

  @State var showingKeyField = false
  @State var apiKey = ""
  // A sheet or confirmation dialog resigns key and closes the menu bar popover
  // before it can be answered, so removal is confirmed by arming the button.
  @State var removalArmed = false
  @State var armGeneration = 0

  var credentialLabel: String { setup?.credentialLabel ?? routerLocalized("API key") }

  var body: some View {
    VStack(alignment: .leading, spacing: 9) {
      HStack(spacing: 10) {
        VStack(alignment: .leading, spacing: 2) {
          Text(setup?.displayName ?? provider.id)
            .font(.system(size: 12, weight: .medium))
          Text(detail)
            .font(.system(size: 9, weight: .regular))
            .foregroundStyle(detailTint)
        }
        Spacer()
        actionControl
      }

      if let planNote = setup?.planNote {
        HStack(alignment: .top, spacing: 5) {
          Image(systemName: "creditcard")
            .font(.system(size: 9, weight: .semibold))
          Text(planNote)
            .font(.system(size: 9))
            .fixedSize(horizontal: false, vertical: true)
        }
        .foregroundStyle(routerYellow.opacity(0.9))
      }

      if let anonymousNote = setup?.anonymousNote {
        HStack(alignment: .top, spacing: 5) {
          Image(systemName: "info.circle")
            .font(.system(size: 9, weight: .semibold))
          Text(anonymousNote)
            .font(.system(size: 9))
            .fixedSize(horizontal: false, vertical: true)
        }
        .foregroundStyle(routerMuted)
      }

      if showingKeyField, setup?.kind == "api" {
        VStack(alignment: .leading, spacing: 5) {
          Text(
            setup?.configured == true
              ? (RouterLanguage.isSimplifiedChinese ? "替换\(credentialLabel)" : "Replacement \(credentialLabel)")
              : credentialLabel
          )
            .font(.system(size: 9, weight: .medium))
            .foregroundStyle(routerMuted)
          HStack(spacing: 7) {
            SecureField(
              RouterLanguage.isSimplifiedChinese
                ? "粘贴\(credentialLabel)"
                : "Paste \(credentialLabel.lowercased())",
              text: $apiKey
            )
              .textFieldStyle(.plain)
              .font(.system(size: 11, design: .monospaced))
              .padding(.horizontal, 9)
              .padding(.vertical, 7)
              .glassCard(in: RoundedRectangle(cornerRadius: 6, style: .continuous))
            Button(routerLocalized("Save")) {
              let key = apiKey
              apiKey = ""
              showingKeyField = false
              onSaveKey(key)
            }
            .buttonStyle(AccentButtonStyle())
            .disabled(apiKey.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
          }
        }
        .transition(.opacity.combined(with: .move(edge: .top)))
      }
    }
    .padding(.vertical, 7)
    .animation(.easeOut(duration: 0.18), value: showingKeyField)
    .animation(.easeOut(duration: 0.15), value: removalArmed)
    .onChange(of: setup?.configured) { configured in
      if configured == true {
        apiKey = ""
        showingKeyField = false
        disarmRemoval()
      }
    }
  }

  var detailTint: Color {
    if removalArmed { return routerRed }
    return setup?.configured == true ? routerMuted : routerYellow.opacity(0.9)
  }

  var detail: String {
    if removalArmed { return routerLocalized("Click the check again to delete this credential") }
    guard let setup else { return routerLocalized("Checking setup…") }
    if oauthNeedsReconnect {
      return routerLocalized("Session expired · reconnect for account usage")
    }
    if setup.configured {
      let visibility = provider.enabled ? routerLocalized("Available in Codex") : routerLocalized("Hidden from Codex")
      return setup.signedIn == true
        ? (RouterLanguage.isSimplifiedChinese ? "已登录 · \(visibility)" : "Signed in · \(visibility)")
        : (RouterLanguage.isSimplifiedChinese ? "就绪 · \(visibility)" : "Ready · \(visibility)")
    }
    switch setup.action {
    case "install": return routerLocalized("Official CLI required")
    case "login": return routerLocalized("Sign in with the official CLI")
    case "add-key":
      return offersSignIn ? routerLocalized("Sign in or paste an API key") : "\(credentialLabel) \(routerLocalized("required"))"
    default: return routerLocalized("Setup required")
    }
  }

  var offersSignIn: Bool { setup?.signIn == true }

  // Names both halves when both will run, so one click never does more than
  // the label promised.
  var signInTitle: String {
    setup?.signInAction == "install" ? routerLocalized("Install & Sign In") : routerLocalized("Sign In")
  }

  @ViewBuilder
  var actionControl: some View {
    if isBusy {
      ProgressView()
        .controlSize(.small)
        .tint(routerAccent)
        .frame(width: 42)
    } else if setup?.configured == true {
      HStack(spacing: 8) {
        if setup?.kind == "oauth" {
          if oauthNeedsReconnect {
            Button(routerLocalized("Reconnect"), action: onLogin)
              .buttonStyle(.plain)
              .font(.system(size: 10, weight: .medium))
              .foregroundStyle(routerYellow)
              .disabled(controlsDisabled)
          } else {
            Button(action: onLogin) {
              Image(systemName: "arrow.triangle.2.circlepath")
                .font(.system(size: 10, weight: .semibold))
                .frame(width: 20, height: 20)
            }
            .buttonStyle(.plain)
            .foregroundStyle(routerAccent)
            .help(routerLocalized("Reconnect OAuth"))
            .disabled(controlsDisabled)
          }
        }
        // A key that came from the CLI sign-in can only be renewed by signing
        // in again, so the row keeps that route reachable after connecting.
        if offersSignIn {
          Button(action: { onConnect() }) {
            Image(systemName: "arrow.triangle.2.circlepath")
              .font(.system(size: 10, weight: .semibold))
              .frame(width: 20, height: 20)
          }
          .buttonStyle(.plain)
          .foregroundStyle(routerAccent)
          .help(routerLocalized(setup?.signInAction == "install"
            ? "Install the official CLI and sign in"
            : "Sign in again with the official CLI"))
          .disabled(controlsDisabled)
        }
        if setup?.kind == "api" {
          Button(action: { toggleKeyField() }) {
            Image(systemName: showingKeyField ? "xmark" : "pencil")
              .font(.system(size: 10, weight: .semibold))
              .frame(width: 20, height: 20)
          }
          .buttonStyle(.plain)
          .foregroundStyle(routerAccent)
          .help(
            showingKeyField
              ? routerLocalized("Cancel credential replacement")
              : (RouterLanguage.isSimplifiedChinese ? "替换\(credentialLabel)" : "Replace \(credentialLabel)")
          )
          .disabled(controlsDisabled)

          Button(action: { tapRemove() }) {
            Image(systemName: removalArmed ? "checkmark.circle.fill" : "trash")
              .font(.system(size: removalArmed ? 12 : 10, weight: .semibold))
              .frame(width: 20, height: 20)
          }
          .buttonStyle(.plain)
          .foregroundStyle(removalArmed ? routerRed : routerYellow)
          .help(
            removalArmed
              ? routerLocalized("Click again to delete the stored credential")
              : (RouterLanguage.isSimplifiedChinese ? "移除已保存的\(credentialLabel)" : "Remove stored \(credentialLabel)")
          )
          .disabled(controlsDisabled)
        }
        Toggle("", isOn: Binding(get: { provider.enabled }, set: onToggle))
          .labelsHidden()
          .toggleStyle(.switch)
          .controlSize(.mini)
          .tint(routerMint)
          .disabled(controlsDisabled)
      }
    } else {
      HStack(spacing: 10) {
        // Two ways in, both first-class: the browser sign-in the CLI drives,
        // and the Studio key someone may already hold.
        if offersSignIn {
          Button(signInTitle) { onConnect() }
            .buttonStyle(.plain)
            .font(.system(size: 10, weight: .medium))
            .foregroundStyle(routerAccent)
            .disabled(controlsDisabled)
        }
        Button(actionTitle) { performAction() }
          .buttonStyle(.plain)
          .font(.system(size: 10, weight: .medium))
          .foregroundStyle(offersSignIn ? routerMuted : routerAccent)
          .disabled(controlsDisabled || setup == nil)
      }
    }
  }

  var actionTitle: String {
    switch setup?.action {
    case "install": return routerLocalized("Install & Sign In")
    case "login": return routerLocalized("Sign In")
    case "add-key":
      guard !showingKeyField else { return routerLocalized("Cancel") }
      return credentialLabel == routerLocalized("API key")
        ? routerLocalized("Add Key")
        : (RouterLanguage.isSimplifiedChinese ? "添加\(credentialLabel)" : "Add \(credentialLabel)")
    default: return routerLocalized("Checking…")
    }
  }

  var oauthNeedsReconnect: Bool {
    guard setup?.kind == "oauth", account?.status == "unavailable" else { return false }
    return account?.message?.localizedCaseInsensitiveContains("login") == true
  }

  func performAction() {
    switch setup?.action {
    case "install", "login": onConnect()
    case "add-key": toggleKeyField()
    default: break
    }
  }

  func toggleKeyField() {
    apiKey = ""
    disarmRemoval()
    showingKeyField.toggle()
  }

  // First click arms, second click deletes. The armed state expires on its own
  // so a stray click never leaves a live delete button sitting in the row.
  func tapRemove() {
    if removalArmed {
      disarmRemoval()
      apiKey = ""
      showingKeyField = false
      onRemoveKey()
      return
    }
    removalArmed = true
    armGeneration += 1
    let generation = armGeneration
    DispatchQueue.main.asyncAfter(deadline: .now() + removalArmWindow) {
      if generation == armGeneration { removalArmed = false }
    }
  }

  func disarmRemoval() {
    armGeneration += 1
    removalArmed = false
  }
}

