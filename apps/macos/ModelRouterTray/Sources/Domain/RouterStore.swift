import AppKit
import Combine
import Foundation
import ServiceManagement
import SwiftUI

@MainActor
final class RouterStore: ObservableObject {
  static let shared = RouterStore()

  @Published var snapshot = RouterSnapshot.empty
  @Published var isRefreshing = false
  @Published var message: String?

  /// 非用户操作的异步通知（服务崩溃放弃重拉等）。清空逻辑沿用
  /// message 的既有生命周期（下一次操作覆盖）。
  func publishNotice(_ text: String) {
    message = text
  }
  @Published var lastUpdated: Date?
  @Published var selectedUsageProviderID: String
  @Published var activityState: RouterActivityState = .idle
  @Published var activeRequests: [RouterActiveRequest] = []
  @Published var activeRequestCount: Int = 0
  @Published var activeModel: String?
  @Published var activitySessionName: String?
  @Published var accountUsage: CodexAccountUsage?
  @Published var accountUsageError: String?
  @Published var providerUsage: ProviderUsageSnapshot?
  @Published var providerUsageError: String?
  @Published var providerSetup: [String: ProviderSetupState] = [:]
  @Published var providerOperation: String?
  @Published var benchmarkingTag: String?
  @Published var maintenanceMessage: String?
  @Published var maintenanceSucceeded = false
  @Published var islandMode: IslandMode
  // Publishing the language makes every view re-render on change, so the
  // panel switches in place instead of waiting for the next relaunch.
  @Published var language: TrayLanguage = RouterLanguage.selection
  @Published var presenceMode: TrayPresenceMode
  @Published var hostAppRunning = false
  @Published var surfacesVisible = true
  // A client the tray cannot watch -- the harness, or a terminal `codex` --
  // overrides follow mode. The router computes this; the tray does not
  // re-derive it. Sourced from the routine snapshot, so a client appearing
  // mid-session is picked up without a relaunch.
  @Published var routerPinsServiceOn = false
  // Bumped every time the user opens the app by hand. StatusItemLabel watches
  // it so a double-click gets a visible answer even when the tray was already
  // running and nothing about the router changed.
  @Published var attentionPulse = 0
  var attentionRelease: Task<Void, Never>?
  var userRevealUntil: Date?
  static let userRevealWindow: TimeInterval = 20

  var polling = false
  var activityPolling = false
  var accountUsagePolling = false
  var providerPolling = false
  let defaults = UserDefaults.standard
  let islandVisibilityKey = "ModelRouterTray.islandVisible"
  let islandModeKey = "ModelRouterTray.islandMode"
  // Named for the retired login item because `update` still reads this default
  // to locate a tray installed outside the standard paths.
  let loginItemBundlePathKey = "ModelRouterTray.loginItemBundlePath"
  let presenceModeKey = "ModelRouterTray.presenceMode"
  // The Codex desktop app plus the ChatGPT desktop app, either of which counts
  // as "Codex is open" for the follow mode.
  let hostAppBundleIDs = ["com.openai.codex", "com.openai.chat"]
  // p_comm truncates at 16 characters, so these must be the executable names as
  // the kernel stores them. The npm wrapper is a Node script that execs a native
  // binary called `codex`; the desktop app's helper is also `codex`, which is
  // harmless because that case is already covered by the bundle check.
  nonisolated static let hostProcessNames = ["codex"]
  var workspaceObservers: [NSObjectProtocol] = []
  var pendingServiceStop: Task<Void, Never>?
  // Bumped for every scheduled stop so a cancelled task can tell whether the
  // handle it would clear is still its own.
  var serviceStopGeneration = 0
  var hostAppRecheck: Task<Void, Never>?
  var serviceWork: Task<Void, Never>?
  var serviceIntent: ServiceIntent = .unknown
  // Codex relaunches itself to apply updates, so a momentary disappearance must
  // not bounce the router. Wait the absence out and re-check the process list
  // directly before stopping; workspace notifications are only hints.
  let hostAppAbsenceGrace = Duration.seconds(30)
  let hostAppRecheckInterval = Duration.seconds(5)
  // A request in flight outlives the window that started it; retry rather than
  // cutting a generation off mid-stream.
  let activeRequestRecheck = Duration.seconds(15)
  var accountUsageResolved = false
  var hasResolvedInitialUsageProvider = false
  var hasObservedActiveProvider = false
  var manuallySelectedUsageProvider = false
  var latestObservedActivityRequestID: String?
  var lastObservedSessionID: String?
  var activityHealthFailureStartedAt: Date?

  var dailyUsageCache: [DailyUsageCacheKey: [DailyUsagePoint]] = [:]
  var localUsageTotalsCache: [LocalUsageTotalsCacheKey: UsageTotals] = [:]

  struct DailyUsageCacheBucket: Hashable {
    let startDate: String
    let tokens: Int64
  }

  struct DailyUsageCacheKey: Hashable {
    let providerID: String
    let days: Int
    let today: Date
    let buckets: [DailyUsageCacheBucket]
  }

  struct LocalUsageTotalsCacheBucket: Hashable {
    let startDate: String
    let tokens: Int64
    let requests: Int
  }

  struct LocalUsageTotalsCacheKey: Hashable {
    let providerID: String
    let days: Int
    let today: Date
    let buckets: [LocalUsageTotalsCacheBucket]
  }

  struct UsageTotals {
    let tokens: Double
    let requests: Int
  }

  static let dayKeyFormatter: DateFormatter = {
    let formatter = DateFormatter()
    formatter.locale = Locale(identifier: "en_US_POSIX")
    formatter.calendar = Calendar(identifier: .gregorian)
    formatter.dateFormat = "yyyy-MM-dd"
    return formatter
  }()

  // What the three stored signals mean, as one pure decision. Pulled out of
  // `init` so it can be tested: the mode this picks is the difference between
  // an overlay covering somebody's notch on every display and it never
  // appearing, and asserting on the source text of an initializer proves only
  // that the source says what it says.
  //
  // `storedMode` is the operator's own answer and is taken verbatim forever.
  // `legacyVisible` is the pre-desktop-mode boolean, migrated once. When
  // neither exists nobody has answered: the overlay is opt-in for a new
  // install, but an install that has launched before keeps it, because
  // silently retiring an overlay somebody has been using is its own surprise.
  nonisolated static func resolveIslandMode(
    storedMode: String?,
    legacyVisible: Bool?,
    hasLaunchedBefore: Bool
  ) -> IslandMode {
    if let storedMode, let mode = IslandMode(rawValue: storedMode) { return mode }
    if let legacyVisible { return legacyVisible ? .notch : .off }
    return hasLaunchedBefore ? .notch : .off
  }

  // Activity polling drives SwiftUI state, and the old fixed 350ms cadence kept
  // the main thread laying out views even while nothing was happening. Keep the
  // fast cadence only while an activity must be watched; idle/hidden states can
  // discover the next request a little later without visibly changing the UI.
  nonisolated static func activityPollingInterval(
    surfacesVisible: Bool,
    activeRequestCount: Int,
    activityState: RouterActivityState
  ) -> UInt64 {
    if activeRequestCount > 0 || activityState == .generating || activityState == .starting {
      return 350_000_000
    }
    return surfacesVisible ? 1_000_000_000 : 3_000_000_000
  }

  init() {
    selectedUsageProviderID = "openai"
    // retireLoginItem records the bundle path on every bundled launch and runs
    // after this initializer, so its absence here means nothing has ever
    // launched from a bundle.
    let resolvedIslandMode = Self.resolveIslandMode(
      storedMode: defaults.string(forKey: islandModeKey),
      legacyVisible: defaults.object(forKey: islandVisibilityKey) == nil
        ? nil
        : defaults.bool(forKey: islandVisibilityKey),
      hasLaunchedBefore: defaults.object(forKey: loginItemBundlePathKey) != nil
    )
    islandMode = resolvedIslandMode
    // Persist it, so "never configured" and "explicitly chose notch" stop being
    // the same state for every launch after this one.
    if defaults.string(forKey: islandModeKey) == nil {
      defaults.set(resolvedIslandMode.rawValue, forKey: islandModeKey)
    }
    if let raw = defaults.string(forKey: presenceModeKey),
      let mode = TrayPresenceMode(rawValue: raw)
    {
      presenceMode = mode
    } else {
      presenceMode = .always
    }
  }

  var codexActive: Bool {
    snapshot.targets["codex"]?.active == true
  }

  var loginFree: Bool {
    snapshot.targets["codex"]?.loginFree == true
  }

  var signedRouting: Bool {
    snapshot.targets["codex"]?.signedRouting == true
  }


  var maintenanceRunning: Bool {
    providerOperation == "maintenance" || providerOperation == "doctor"
  }


  // 设置行的窄口：runControl 是 store 的私有工作面，TrayView 的
  // 「打开配置文件」只需要一次性 fire-and-forget 命令。
  func runControlPublic(arguments: [String]) async throws -> Data {
    try await runControl(arguments: arguments)
  }

  /// 动态模型同步：实时拉 provider 货架 + models.dev 参数 → 覆盖层。
  /// 结果行（+ slug / 已收录数）直接显示在页脚消息里。
  func syncModelsNow() async {
    do {
      let output = try await runControlPublic(arguments: ["models", "sync"])
      let text = String(data: output, encoding: .utf8) ?? ""
      let lines = text.split(separator: "\n").prefix(6).joined(separator: "\n")
      message = lines.isEmpty
        ? routerLocalized("Model sync finished.")
        : lines
      await refresh()
    } catch {
      message = error.localizedDescription
    }
  }

  // control 进程执行归 RouterControlClient（ControlClient.swift）；
  // 这里保留薄转发，30+ 个业务调用点无需感知拆分。
  let control = RouterControlClient()

  func runControl(arguments: [String], stdin: Data? = nil) async throws -> Data {
    try await control.run(arguments, stdin: stdin)
  }
}
