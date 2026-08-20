import AppKit
import Combine
import Foundation
import ServiceManagement
import SwiftUI


struct TrayView: View {
  @ObservedObject var store: RouterStore
  var presentation: TrayPresentation = .menuBar
  @AppStorage("trayTab") private var tab: TrayTab = .usage
  @State var providersExpanded = true
  // 登录项状态：App 化后服务只随 App 运行，开机自启 = 把 App 注册为
  // 登录项（SMAppService）。nil = 状态不可用（非 bundle 运行）。
  @State var loginItemEnabled: Bool?
  @State var loginItemError: String?

  var isWindow: Bool { presentation == .window }

  func refreshLoginItemStatus() {
    guard Bundle.main.bundleIdentifier != nil else {
      loginItemEnabled = nil
      return
    }
    loginItemEnabled = SMAppService.mainApp.status == .enabled
  }

  func setLoginItem(_ enabled: Bool) {
    do {
      if enabled {
        try SMAppService.mainApp.register()
      } else {
        try SMAppService.mainApp.unregister()
      }
      loginItemError = nil
    } catch {
      loginItemError = error.localizedDescription
    }
    refreshLoginItemStatus()
  }

  // 重载配置：control reload（校验 config.toml / 刷 catalog / 重发布
  // 集成块）。服务进程不动 —— 凭证本来就逐请求解析。
  func reloadConfiguration() {
    Task {
      do {
        _ = try await store.runControlPublic(arguments: ["reload"])
        await store.refresh()
      } catch {
        // 错误经 store.message 呈现（runControl 抛 RouterError 带 stderr）。
      }
    }
  }

  // 打开凭证配置：不存在则先生成带注释的模板（control config init），
  // 再交给系统默认文本编辑器 —— Claude Code 式的 config.toml 是操作
  // 者填 key 的家。
  func openCredentialsConfig() {
    Task {
      _ = try? await store.runControlPublic(arguments: ["config", "init"])
      let dir = ProcessInfo.processInfo.environment["CODEX_ROUTER_STATE_DIR"]
        ?? FileManager.default.homeDirectoryForCurrentUser
          .appendingPathComponent(".codex-router").path
      NSWorkspace.shared.open(URL(fileURLWithPath: dir + "/config.toml"))
    }
  }

  var target: RouterTarget? { store.snapshot.targets["codex"] }
  // Rows come from the registry snapshot, not from the models in the picker.
  // Deriving them from models hid every provider that ships none until its
  // models were curated — which is backwards, because the row is where the
  // operator sets a provider up. That left the ten catalog-only services and
  // the keyless local provider invisible in the one place built to configure
  // them. The model-derived list survives only for routers that predate the
  // snapshot's `providers` field.
  var providers: [(id: String, enabled: Bool)] {
    guard let target else { return [] }
    if let registry = target.providers, !registry.isEmpty {
      let enabled = Set(target.enabledProviders)
      return registry
        .map { (id: $0.id, enabled: enabled.contains($0.id)) }
        .sorted { $0.id < $1.id }
    }
    return Dictionary(grouping: target.models.filter { $0.provider != "openai" }, by: \.provider)
      .map { (id: $0.key, enabled: $0.value.contains(where: \.enabled)) }
      .sorted { $0.id < $1.id }
  }

  var body: some View {
    ZStack {
      VisualEffectBlur()
        .ignoresSafeArea()
      VStack(spacing: 0) {
        header
        if let target {
          content(for: target)
        } else if store.isRefreshing {
          ProgressView()
            .controlSize(.small)
            .tint(routerAccent)
            .frame(maxHeight: .infinity)
        } else {
          emptyState
        }
        footer
      }
      .padding(14)
    }
    .preferredColorScheme(.dark)
    .foregroundStyle(routerText)
    .task { await store.refresh() }
  }


  var header: some View {
    HStack(alignment: .center, spacing: 12) {
      // 窗口形态给 App 头像（App icon 的同源渲染）；菜单栏弹出
      // 寸土寸金，保持纯文字。
      if isWindow {
        Image(nsImage: NSApp.applicationIconImage)
          .resizable()
          .frame(width: 34, height: 34)
          .accessibilityLabel(routerLocalized("Model Router"))
      }
      VStack(alignment: .leading, spacing: 3) {
        Text(routerLocalized("Model Router"))
          .font(.system(size: 15, weight: .semibold))
        Text(accountLabel)
          .font(.system(size: 10, weight: .regular))
          .foregroundStyle(routerMuted)
      }
      Spacer()
      StatusBeacon(state: store.activityState)
    }
    .padding(.bottom, 12)
  }

  var accountLabel: String {
    if !store.selectedUsageUsesChatGPT {
      guard let provider = store.selectedProviderUsage else { return store.selectedUsageProvider.detail }
      return "\(provider.displayName) · \(provider.credentialType.uppercased())"
    }
    guard let plan = store.accountUsage?.planType else { return routerLocalized("Codex account") }
    return "ChatGPT \(plan.capitalized)"
  }

  func content(for target: RouterTarget) -> some View {
    VStack(alignment: .leading, spacing: 12) {
      Picker("", selection: $tab) {
        ForEach(TrayTab.allCases) { item in
          Text(item.label).tag(item)
        }
      }
      .pickerStyle(.segmented)
      .labelsHidden()

      ScrollView(showsIndicators: false) {
        VStack(alignment: .leading, spacing: 14) {
          switch tab {
          case .usage: usageTab
          case .status: statusTab
          case .settings: settingsTab(for: target)
          }
        }
        .padding(.vertical, 1)
      }
    }
  }

  @ViewBuilder
  var usageTab: some View {
    if store.visibleUsageProviders.isEmpty && store.overallModelUsage.isEmpty {
      emptyNotice(routerLocalized("No usage recorded yet"))
    }
    if !store.visibleUsageProviders.isEmpty {
      sectionLabel(routerLocalized("Current usage"), detail: store.selectedUsageProvider.displayName)
      ProviderUsageSection(store: store)
        .id(store.selectedUsageProviderID)
      sectionLabel(routerLocalized("All usage"), detail: routerLocalized("7-day snapshot"))
      AllProviderUsageGrid(store: store)
    }
    if !store.overallModelUsage.isEmpty {
      sectionLabel(
        routerLocalized("Tokens by model"),
        detail: "\(compactTokenCount(Double(store.overallTokenTotal))) tok · \(store.overallRequestTotal) req"
      )
      ModelUsageBreakdown(store: store)
    }
  }

  @ViewBuilder
  var statusTab: some View {
    sectionLabel(routerLocalized("Router"), detail: store.activitySummaryLabel)
    HStack(spacing: 8) {
      Circle()
        .fill(store.activityState.tint)
        .frame(width: 7, height: 7)
      VStack(alignment: .leading, spacing: 2) {
        Text(store.activityState.label)
          .font(.system(size: 12, weight: .medium))
        Text(activityDetail)
          .font(.system(size: 9))
          .foregroundStyle(routerMuted)
      }
      Spacer()
    }

    sectionLabel(routerLocalized("Model speed"), detail: speedSampleDetail)
    HStack(alignment: .firstTextBaseline, spacing: 8) {
      VStack(alignment: .leading, spacing: 2) {
        Text(activeModelLabel)
          .font(.system(size: 10, weight: .medium))
          .lineLimit(1)
          .truncationMode(.middle)
        Text(speedExplanation)
          .font(.system(size: 8))
          .foregroundStyle(routerMuted)
          .lineLimit(1)
      }
      Spacer(minLength: 8)
      Text(activeModelSpeedLabel)
        .font(.system(size: 15, weight: .semibold, design: .monospaced))
        .foregroundStyle(store.activeModelObservedTokensPerSecond == nil ? routerMuted : routerMint)
        .monospacedDigit()
    }
    .padding(9)
    .background(
      Color.primary.opacity(0.045),
      in: RoundedRectangle(cornerRadius: 9, style: .continuous)
    )
    // Issue #182: the card above tracks the active model only, so a second
    // model's speed was unknowable without switching to it and waiting.
    if store.recentModelSpeeds.count > 1 {
      VStack(spacing: 0) {
        ForEach(store.recentModelSpeeds) { row in
          HStack(spacing: 8) {
            Text(row.model.displayName ?? row.model.slug)
              .font(.system(size: 9))
              .lineLimit(1)
              .truncationMode(.middle)
            Spacer(minLength: 8)
            Text(row.providerName)
              .font(.system(size: 8))
              .foregroundStyle(routerMuted)
              .lineLimit(1)
            Text(row.model.observedTokensPerSecond.map { String(format: "%.1f", $0) } ?? "—")
              .font(.system(size: 9, weight: .medium, design: .monospaced))
              .foregroundStyle(routerMint)
              .monospacedDigit()
              .frame(width: 46, alignment: .trailing)
          }
          .padding(.vertical, 3)
          .padding(.horizontal, 9)
        }
      }
      .padding(.vertical, 2)
      .background(
        Color.primary.opacity(0.03),
        in: RoundedRectangle(cornerRadius: 9, style: .continuous)
      )
    }

    sectionLabel(
      routerLocalized("Live requests"),
      detail: store.activeRequests.isEmpty ? routerLocalized("None") : "\(store.activeRequests.count)"
    )
    if store.activeRequests.isEmpty {
      emptyNotice(routerLocalized("Nothing in flight"))
    } else {
      VStack(spacing: 6) {
        ForEach(store.activeRequests) { request in
          HStack(spacing: 6) {
            Text(store.modelLabel(for: request))
              .font(.system(size: 10, weight: .medium))
              .lineLimit(1)
            Text(store.displayName(forProvider: request.provider))
              .font(.system(size: 8))
              .foregroundStyle(routerMuted)
              .lineLimit(1)
            Spacer(minLength: 6)
            Text(elapsedLabel(for: request))
              .font(.system(size: 10))
              .monospacedDigit()
              .foregroundStyle(routerMuted)
          }
        }
      }
    }

    if !quotaResets.isEmpty {
      sectionLabel(routerLocalized("Quota resets"), detail: "\(quotaResets.count)")
      VStack(spacing: 5) {
        ForEach(quotaResets, id: \.id) { entry in
          HStack {
            Text(entry.title)
              .font(.system(size: 10, weight: .medium))
              .lineLimit(1)
            Spacer(minLength: 6)
            Text(usageResetCaption(entry.date))
              .font(.system(size: 9))
              .foregroundStyle(routerMuted)
          }
        }
      }
    }
  }

  var quotaResets: [(id: String, title: String, date: Date)] {
    store.visibleUsageCards.compactMap { card in
      guard let date = card.resetDate else { return nil }
      return (id: card.id, title: card.title, date: date)
    }
  }

  var activityDetail: String {
    guard store.activeRequestCount > 0 else { return routerLocalized("No traffic right now") }
    let chats = store.activeChatCount
    let requests = store.activeRequestCount
    if RouterLanguage.isSimplifiedChinese {
      return "\(chats) 个会话 · \(requests) 个请求进行中"
    }
    return "\(chats) chat\(chats == 1 ? "" : "s") · \(requests) request\(requests == 1 ? "" : "s") in flight"
  }

  var activeModelLabel: String {
    guard let model = store.activeRequests.last?.model ?? store.activeModel else {
      return routerLocalized("No model observed")
    }
    return model.split(separator: "/").last.map(String.init) ?? model
  }

  var activeModelSpeedLabel: String {
    guard let speed = store.activeModelObservedTokensPerSecond else { return "— tok/s" }
    return "\(String(format: "%.1f", speed)) tok/s"
  }

  var speedSampleDetail: String {
    guard let model = store.activeRequests.last?.model ?? store.activeModel else { return routerLocalized("Waiting") }
    let displayName = model.split(separator: "/").last.map(String.init) ?? model
    let sampleCount = store.providerUsage?.providers
      .flatMap { $0.models ?? [] }
      .first { $0.slug == model || $0.displayName == displayName }?
      .speedSampleCount ?? 0
    return sampleCount == 0
      ? routerLocalized("No samples")
      : RouterLanguage.isSimplifiedChinese
        ? "\(sampleCount) 条回复"
        : "\(sampleCount) reply\(sampleCount == 1 ? "" : "s")"
  }

  var speedExplanation: String {
    store.activeModelObservedTokensPerSecond == nil
      ? routerLocalized("Appears after a metered reply")
      : routerLocalized("Observed output throughput")
  }

  // `startedAt` arrives as epoch milliseconds from the router health payload.
  func elapsedLabel(for request: RouterActiveRequest) -> String {
    let elapsed = max(0, Date().timeIntervalSince1970 - request.startedAt / 1_000)
    if elapsed >= 60 {
      return String(format: "%dm %02ds", Int(elapsed) / 60, Int(elapsed) % 60)
    }
    return String(format: "%.1fs", elapsed)
  }

  func emptyNotice(_ text: String) -> some View {
    Text(text)
      .font(.system(size: 10))
      .foregroundStyle(routerMuted)
      .padding(.vertical, 2)
  }

  @ViewBuilder
  func settingsTab(for target: RouterTarget) -> some View {
    HStack(spacing: 12) {
      VStack(alignment: .leading, spacing: 3) {
        Text(routerLocalized("Show tray"))
          .font(.system(size: 12, weight: .medium))
        Text(store.presenceMode == .followCodex && store.routerPinsServiceOn
          ? routerLocalized("Kept on: a terminal session has no window to follow")
          : store.presenceMode == .followCodex
            ? routerLocalized("Appears with Codex or ChatGPT, hides when they quit")
            : routerLocalized("Menu bar icon stays visible"))
          .font(.system(size: 10))
          .foregroundStyle(routerMuted)
      }
      Spacer()
      Picker("", selection: Binding(
        get: { store.presenceMode },
        set: { store.setPresenceMode($0) }
      )) {
        ForEach(TrayPresenceMode.allCases) { mode in
          Text(mode.label).tag(mode)
        }
      }
      .pickerStyle(.segmented)
      .labelsHidden()
      .frame(width: 168)
    }
    .padding(.vertical, 2)
    HStack(spacing: 12) {
      VStack(alignment: .leading, spacing: 3) {
        Text(routerLocalized("Language"))
          .font(.system(size: 12, weight: .medium))
        Text(routerLocalized("Tray language. Reopen the panel to apply everywhere."))
          .font(.system(size: 10))
          .foregroundStyle(routerMuted)
      }
      Spacer()
      // A dropdown rather than the segmented style the neighbouring rows use:
      // the option labels are written in the language each one selects, so
      // they are different scripts and different widths, and a segmented
      // control would size every cell to the widest and leave the Latin ones
      // adrift. A dropdown also stays right when a third language lands.
      Picker("", selection: Binding(
        get: { store.language },
        set: { store.setLanguage($0) }
      )) {
        ForEach(TrayLanguage.allCases) { option in
          Text(option.label).tag(option)
        }
      }
      .pickerStyle(.menu)
      .labelsHidden()
      .frame(width: 168)
    }
    .padding(.vertical, 2)
    // App 化：服务只随 App 运行，开机自启的形态是「登录时启动 App」。
    HStack(spacing: 12) {
      VStack(alignment: .leading, spacing: 3) {
        Text(routerLocalized("Launch at Login"))
          .font(.system(size: 12, weight: .medium))
        Text(routerLocalized(
          "Start Model Router automatically when you log in. The router service runs only while the app is open."
        ))
        .font(.system(size: 10))
        .foregroundStyle(routerMuted)
        if let loginItemError {
          Text(loginItemError)
            .font(.system(size: 9))
            .foregroundStyle(routerRed.opacity(0.9))
            .lineLimit(2)
        }
      }
      Spacer()
      if let enabled = loginItemEnabled {
        Toggle("", isOn: Binding(
          get: { enabled },
          set: { setLoginItem($0) }
        ))
        .toggleStyle(.switch)
        .labelsHidden()
      } else {
        Text(routerLocalized("Login item status unavailable (app not in a bundle)."))
          .font(.system(size: 9))
          .foregroundStyle(routerMuted)
      }
    }
    .padding(.vertical, 2)
    .onAppear { refreshLoginItemStatus() }
    // 凭证配置文件：Claude Code 式 config.toml，一按钮直达。
    HStack(spacing: 12) {
      VStack(alignment: .leading, spacing: 3) {
        Text(routerLocalized("Credentials config"))
          .font(.system(size: 12, weight: .medium))
        Text(routerLocalized(
          "Open ~/.codex-router/config.toml — paste API keys under each provider; takes effect on the next request."
        ))
        .font(.system(size: 10))
        .foregroundStyle(routerMuted)
      }
      Spacer()
      HStack(spacing: 6) {
        Button(routerLocalized("Reload")) {
          reloadConfiguration()
        }
        .buttonStyle(AccentButtonStyle())
        Button(routerLocalized("Open Config")) {
          openCredentialsConfig()
        }
        .buttonStyle(AccentButtonStyle())
      }
    }
    .padding(.vertical, 2)
    HStack(spacing: 12) {
      VStack(alignment: .leading, spacing: 3) {
        Text(routerLocalized("Dynamic Island"))
          .font(.system(size: 12, weight: .medium))
        Text(store.islandMode == .desktop
          ? routerLocalized("Quotas and live activity pinned to the desktop")
          : store.islandMode == .notch
            ? routerLocalized("Usage and activity over the notch on every display")
            : routerLocalized("Off by default. The menu-bar panel stays available either way."))
          .font(.system(size: 10))
          .foregroundStyle(routerMuted)
      }
      Spacer()
      Picker("", selection: Binding(
        get: { store.islandMode },
        set: { store.setIslandMode($0) }
      )) {
        ForEach(IslandMode.allCases) { mode in
          Text(mode.label).tag(mode)
        }
      }
      .pickerStyle(.segmented)
      .labelsHidden()
      .frame(width: 168)
    }
    .padding(.vertical, 2)
    settingRow(
      title: routerLocalized("Use Router with ChatGPT"),
      detail: store.signedRouting
        ? routerLocalized("Native GPT + external models · task history preserved")
        : routerLocalized("Keep ChatGPT login and the current task history"),
      isOn: Binding(
        get: { store.signedRouting },
        set: { enabled in Task { await store.setSignedRouting(enabled) } }
      ),
      isDisabled: store.providerOperation != nil || store.loginFree
    )
    settingRow(
      title: routerLocalized("Use without OpenAI login"),
      detail: store.loginFree
        ? routerLocalized("External providers · Codex restarts automatically")
        : routerLocalized("Use connected models and restart Codex"),
      isOn: Binding(
        get: { store.loginFree },
        set: { enabled in Task { await store.setLoginFree(enabled) } }
      ),
      isDisabled: store.providerOperation != nil || store.signedRouting
    )
    maintenanceRow
    AccordionPanel(
      title: routerLocalized("Providers"),
      summary: store.providerOperation == nil ? routerLocalized("Auto-saved") : routerLocalized("Applying…"),
      expanded: $providersExpanded
    ) {
      VStack(spacing: 0) {
        ForEach(providers, id: \.id) { provider in
          ProviderSetupRow(
            provider: provider,
            setup: store.providerSetup[provider.id],
            account: store.providerUsage(for: provider.id)?.account,
            isBusy: store.providerOperation == provider.id,
            controlsDisabled: store.providerOperation != nil,
            onToggle: { enabled in
              Task { await store.setProvider(provider.id, enabled: enabled) }
            },
            onConnect: { Task { await store.connectProvider(provider.id) } },
            onLogin: { Task { await store.loginProvider(provider.id) } },
            onSaveKey: { key in Task { await store.saveProviderKey(provider.id, key: key) } },
            onRemoveKey: { Task { await store.removeProviderKey(provider.id) } }
          )
          if provider.id != providers.last?.id {
            Divider()
          }
        }
      }
    }
    ModelSettingsAccordion(store: store, target: target)
  }
}
