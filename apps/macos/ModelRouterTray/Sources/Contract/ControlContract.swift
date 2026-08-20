// ControlContract.swift —— tray 与 codex-router 控制面之间的 JSON 契约。
//
// 这里的 Decodable 结构与 Go 侧 internal/app/controlplane 的类型化 Snapshot
// 对端：加字段两边同步改；Go 侧输出 null 数组会让这里的 [T] 解码直接
// 失败（契约测试钉住）。RouterError / RouterHealth / RouterActivity
// 原为 ModelRouterTrayApp.swift 的文件私有类型，拆分后改为模块内可见。

import SwiftUI

enum RouterActivityState: String, Decodable {
  case idle
  case generating
  case starting
  case error

  var tint: Color {
    switch self {
    case .idle: return routerMint
    case .generating: return routerYellow
    case .starting: return routerAccent
    case .error: return routerRed
    }
  }

  var label: String {
    switch self {
    case .idle: return routerLocalized("Idle")
    case .generating: return routerLocalized("Thinking")
    case .starting: return routerLocalized("Starting")
    case .error: return routerLocalized("Error")
    }
  }
}


struct RouterHealth: Decodable {
  let activity: RouterActivity
}

struct RouterActivity: Decodable {
  let state: RouterActivityState
  let provider: String?
  let model: String?
  let sessionName: String?
  let activeCount: Int?
  let active: [RouterActiveRequest]?
}

struct RouterActiveRequest: Decodable, Identifiable, Equatable {
  let id: String
  let provider: String
  let model: String?
  let sessionName: String?
  let sessionId: String?
  let threadId: String?
  let parentThreadId: String?
  let agentName: String?
  let agentNickname: String?
  let isSubagent: Bool?
  let startedAt: Double
}


struct RouterSnapshot: Decodable {
  let targets: [String: RouterTarget]
  // Absent from an older router's output, so the tray keeps working against one
  // rather than failing the whole decode over a field it gained later.
  let presence: RouterPresence?
  static let empty = RouterSnapshot(targets: [:], presence: nil)
}

struct RouterPresence: Decodable {
  let mode: String
  let effectiveMode: String
  let harnessPublished: Bool
  let terminalCodex: Bool
}


struct CodexAccountUsage: Decodable, Equatable {
  let fetchedAt: String
  let planType: String?
  let limitId: String?
  let primary: CodexRateLimitWindow?
  let secondary: CodexRateLimitWindow?
  let dailyUsageBuckets: [CodexDailyUsageBucket]
  let summary: CodexUsageSummary

  static func == (lhs: CodexAccountUsage, rhs: CodexAccountUsage) -> Bool {
    lhs.planType == rhs.planType
      && lhs.limitId == rhs.limitId
      && lhs.primary == rhs.primary
      && lhs.secondary == rhs.secondary
      && lhs.dailyUsageBuckets == rhs.dailyUsageBuckets
      && lhs.summary == rhs.summary
  }
}

struct CodexRateLimitWindow: Decodable, Equatable {
  let usedPercent: Int
  let remainingPercent: Int
  let windowDurationMins: Int?
  let resetsAt: TimeInterval?

  var resetDate: Date? { resetsAt.map(Date.init(timeIntervalSince1970:)) }

  var durationLabel: String {
    guard let minutes = windowDurationMins else { return routerLocalized("Current limit") }
    if minutes >= 1_440, minutes.isMultiple(of: 1_440) {
      let days = minutes / 1_440
      if days == 1 { return routerLocalized("Daily limit") }
      if days == 7 { return routerLocalized("Weekly limit") }
      return RouterLanguage.isSimplifiedChinese ? "\(days) 天限制" : "\(days)-day limit"
    }
    if minutes >= 60, minutes.isMultiple(of: 60) {
      return RouterLanguage.isSimplifiedChinese ? "\(minutes / 60) 小时限制" : "\(minutes / 60)-hour limit"
    }
    return RouterLanguage.isSimplifiedChinese ? "\(minutes) 分钟限制" : "\(minutes)-minute limit"
  }
}

struct CodexDailyUsageBucket: Decodable, Equatable {
  let startDate: String
  let tokens: Int64
}

struct DailyUsagePoint: Identifiable, Equatable {
  let date: Date
  let tokens: Double
  var id: Date { date }
}

struct CodexUsageSummary: Decodable, Equatable {
  let lifetimeTokens: Int64?
  let peakDailyTokens: Int64?
  let currentStreakDays: Int?
}

struct ProviderUsageSnapshot: Decodable, Equatable {
  let fetchedAt: String
  let scope: String
  let providers: [RouterProviderUsage]

  static func == (lhs: ProviderUsageSnapshot, rhs: ProviderUsageSnapshot) -> Bool {
    lhs.scope == rhs.scope && lhs.providers == rhs.providers
  }
}

struct RouterProviderUsage: Decodable, Identifiable, Equatable {
  let id: String
  let displayName: String
  let credentialType: String
  let scope: String
  let requests: Int
  let successfulRequests: Int
  let meteredRequests: Int
  let inputTokens: Int64
  let outputTokens: Int64
  let totalTokens: Int64
  let dailyUsageBuckets: [ProviderDailyUsageBucket]
  let account: ProviderAccountUsage
  // Optional so a newer tray still decodes snapshots from an older router.
  let models: [RouterModelUsage]?
}

struct RouterModelUsage: Decodable, Identifiable, Equatable {
  let slug: String
  let displayName: String
  let requests: Int
  let successfulRequests: Int
  let meteredRequests: Int
  let inputTokens: Int64
  let outputTokens: Int64
  let totalTokens: Int64
  let speedSampleCount: Int?
  let observedTokensPerSecond: Double?
  let lastUsedAt: String?

  var id: String { slug }
}

struct ModelUsageRow: Identifiable {
  let providerID: String
  let providerName: String
  let model: RouterModelUsage

  var id: String { "\(providerID)/\(model.slug)" }
}

struct ProviderAccountUsage: Decodable, Equatable {
  let status: String
  let source: String
  let metrics: [ProviderAccountMetric]
  let message: String?
  let plan: String?
  let dashboardUrl: String?
}

struct ProviderAccountMetric: Decodable, Equatable {
  let kind: String
  let label: String
  let usedPercent: Double?
  let remainingPercent: Double?
  let used: Double?
  let limit: Double?
  let remaining: Double?
  let unit: String?
  let resetAt: TimeInterval?
  let value: Double?
  let currency: String?
  let detail: String?
  let available: Bool?

  var resetDate: Date? { resetAt.map(Date.init(timeIntervalSince1970:)) }
}

struct ProviderDailyUsageBucket: Decodable, Equatable {
  let startDate: String
  let tokens: Int64
  let requests: Int
}


struct RouterTarget: Decodable {
  let target: String
  let configured: Bool
  let active: Bool
  let enabledProviders: [String]
  let providers: [RouterProviderInfo]?
  let models: [RouterModel]
  let selectedModel: String?
  let loginFree: Bool?
  let loginFreeManaged: Bool?
  let signedRouting: Bool?
  let signedRoutingManaged: Bool?
  let nativeAliases: [String: String]?
  let modelSettings: ModelSettingsSnapshot?
}

struct RouterProviderInfo: Decodable {
  let id: String
  let displayName: String
  let kind: String?

  static let legacyFallback: [RouterProviderInfo] = [
    .init(id: "grok-oauth", displayName: "Grok OAuth", kind: "oauth"),
    .init(id: "kimi-oauth", displayName: "Kimi OAuth", kind: "oauth"),
    .init(id: "deepseek", displayName: "DeepSeek API", kind: "openai-compatible"),
    .init(id: "grok-api", displayName: "Grok API", kind: "openai-compatible"),
    .init(id: "kimi-api", displayName: "Kimi API", kind: "openai-compatible"),
    .init(id: "kimi-api-cn", displayName: "Kimi API (China)", kind: "openai-compatible"),
    .init(id: "anthropic-api", displayName: "Anthropic API", kind: "openai-compatible"),
  ]
}

struct RouterModel: Decodable, Identifiable {
  let slug: String
  let displayName: String
  let provider: String
  let enabled: Bool
  let multiAgentVersion: String?
  let visible: Bool?
  // proven = 注册表 multiAgentVersion v2；declared = 本地声明。
  // 开关语义分流：证明过的走 disabled 收窄，未证明的走声明通道。
  let proven: Bool?
  let declared: Bool?
  var id: String { slug }
}

struct ModelSettingsSnapshot: Decodable {
  let subagents: SubagentSettingsSnapshot
  let picker: PickerSettingsSnapshot
  let visionBridge: VisionBridgeSnapshot?
}


struct LocalCatalogSnapshot: Decodable {
  let mode: String?
  let note: String?
}


/// The vision bridge snapshot. The reading engine is fixed to the native
/// vision model of the signed-in ChatGPT session, so there is no engine
/// choice to carry — only whether the bridge is on and what it resolves to.
struct VisionBridgeSnapshot: Decodable {
  let enabled: Bool
  let resolvedEngine: String?
  let resolvedEngineName: String?
}

struct SubagentSettingsSnapshot: Decodable {
  let mode: String
  let enabled: [String]
  let disabled: [String]
  let all: Bool
}

struct PickerSettingsSnapshot: Decodable {
  let hidden: [String]
}


struct ProviderSetupSnapshot: Decodable {
  let providers: [ProviderSetupState]
}

struct ProviderSetupState: Decodable, Identifiable, Equatable {
  let id: String
  let displayName: String
  let kind: String
  let configured: Bool
  let cliInstalled: Bool?
  let action: String
  // An API provider whose official CLI mints its key through a browser
  // sign-in (Command Code) keeps `kind == "api"` and the key field, and adds
  // these: `signIn` marks the second route, `signedIn` says the key in play
  // came from that session, and `signInAction` is that route's next step.
  let signIn: Bool?
  let signedIn: Bool?
  let signInAction: String?
  let credentialLabel: String?
  // Set when connecting successfully still leaves the account unable to use
  // the API, because its plan does not include one. Shown before the buttons
  // rather than after a 403 lands in Codex.
  let planNote: String?
  let anonymousNote: String?
}
