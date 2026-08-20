import AppKit
import Combine
import Foundation
import ServiceManagement
import SwiftUI

extension RouterStore {

  static let providerShortNames: [String: String] = [
    "opencode-free": "OpenCode Free",
    "kilo-free": "Kilo Free",
    "grok-oauth": "Grok",
    "kimi-oauth": "Kimi",
    "deepseek": "DeepSeek",
    "grok-api": "Grok API",
    "kimi-api": "Kimi API",
    "kimi-api-cn": "Kimi CN",
    "anthropic-api": "Claude",
    "zai-coding": "GLM",
    "zai-api": "GLM API",
    "litellm": "LiteLLM",
    "qwen-plan": "Qwen",
    "ollama-cloud": "Ollama",
    "commandcode": "Command Code",
    "github-copilot": "Copilot",
    "clinepass": "ClinePass",
    "chutes": "Chutes",
  ]

  static func shortName(forRegistryProvider provider: RouterProviderInfo) -> String {
    if let short = providerShortNames[provider.id] { return short }
    let base = provider.displayName.split(separator: "(").first.map(String.init)
      ?? provider.displayName
    let trimmed = base.trimmingCharacters(in: .whitespaces)
    return trimmed.count > 12 ? String(trimmed.prefix(12)) : trimmed
  }

  // Provider choices come from the router's registry snapshot so newly added
  // providers appear without a tray update; the static list is only a
  // fallback for routers that predate the snapshot's providers field.
  var usageProviderChoices: [UsageProviderChoice] {
    let target = snapshot.targets["codex"]
    let enabled = Set(target?.enabledProviders ?? [])
    let registryProviders = target?.providers ?? RouterProviderInfo.legacyFallback
    var choices = [
      UsageProviderChoice(
        id: "openai", displayName: "ChatGPT", shortName: "ChatGPT",
        detail: "Codex subscription", isEnabled: true),
    ]
    for provider in registryProviders {
      choices.append(UsageProviderChoice(
        id: provider.id,
        displayName: provider.displayName,
        shortName: Self.shortName(forRegistryProvider: provider),
        detail: providerDetail(provider.id, enabled: enabled),
        isEnabled: enabled.contains(provider.id)))
    }
    return choices
  }

  var selectedUsageProvider: UsageProviderChoice {
    usageProviderChoices.first(where: { $0.id == selectedUsageProviderID }) ?? usageProviderChoices[0]
  }

  var selectedUsageText: String? {
    if selectedUsageUsesChatGPT {
      guard let primary = accountUsage?.primary else { return nil }
      return RouterLanguage.isSimplifiedChinese
        ? "剩余 \(primary.remainingPercent)%"
        : "\(primary.remainingPercent)% left"
    }
    guard providerUsage != nil else { return nil }
    if let metric = selectedAccountMetric { return formattedAccountMetric(metric) }
    return localUsageSummary(for: selectedUsageProviderID, days: 7)
  }

  var selectedUsageUsesChatGPT: Bool {
    selectedUsageProviderID == "openai"
  }

  var selectedProviderUsage: RouterProviderUsage? {
    providerUsage(for: selectedUsageProviderID)
  }

  var selectedAccountMetric: ProviderAccountMetric? {
    selectedProviderUsage?.account.metrics.first
  }

  var selectedTodayTokens: Double {
    dailyUsage(days: 1).last?.tokens ?? 0
  }

  var selectedUsageResetDate: Date? {
    if selectedUsageUsesChatGPT { return accountUsage?.primary?.resetDate }
    return selectedAccountMetric?.resetDate
  }

  /// Running chats, not in-flight HTTP requests. One chat fans out into many
  /// requests (turns, subagents, compactions) and counting those reads as a
  /// runaway number that never matches what the user has open.
  var activeChatCount: Int {
    var seen = Set<String>()
    for request in activeRequests {
      seen.insert(request.sessionId ?? request.sessionName ?? "request-\(request.id)")
    }
    return seen.count
  }

  var hasConcurrentActivity: Bool {
    activeChatCount > 1
  }

  var activitySummaryLabel: String {
    if activityState == .generating, activeChatCount > 1 {
      return RouterLanguage.isSimplifiedChinese
        ? "\(activeChatCount) 个会话"
        : "\(activeChatCount) chats"
    }
    return activityState.label
  }

  var compactActivityProvidersLabel: String {
    let names = uniqueActiveProviderShortNames
    if names.isEmpty { return selectedUsageProvider.shortName }
    if names.count == 1 { return names[0] }
    if names.count == 2 { return "\(names[0]) + \(names[1])" }
    return "\(names[0]) +\(names.count - 1)"
  }

  var uniqueActiveProviderShortNames: [String] {
    var seen = Set<String>()
    var names: [String] = []
    for request in activeRequests {
      let name = shortName(forProvider: request.provider)
      if seen.insert(name).inserted {
        names.append(name)
      }
    }
    return names
  }

  func shortName(forProvider providerID: String) -> String {
    usageProviderChoices.first(where: { $0.id == providerID })?.shortName
      ?? providerID
  }

  func displayName(forProvider providerID: String) -> String {
    usageProviderChoices.first(where: { $0.id == providerID })?.displayName
      ?? providerID
  }

  func modelLabel(for request: RouterActiveRequest) -> String {
    guard let model = request.model, !model.isEmpty else {
      return displayName(forProvider: request.provider)
    }
    if let slash = model.lastIndex(of: "/") {
      return String(model[model.index(after: slash)...])
    }
    return model
  }

  func observedTokensPerSecond(providerID: String?, model: String?) -> Double? {
    guard let model, !model.isEmpty else { return nil }
    let displayName = model.split(separator: "/").last.map(String.init) ?? model
    let matchingProviders: [RouterProviderUsage]
    if let providerID,
       let provider = providerUsage?.providers.first(where: { $0.id == providerID }) {
      matchingProviders = [provider]
    } else {
      matchingProviders = providerUsage?.providers ?? []
    }
    let match = matchingProviders
      .flatMap { $0.models ?? [] }
      .first { $0.slug == model || $0.displayName == displayName }
    if let speed = match?.observedTokensPerSecond { return speed }
    // Protocol variants are folded into their canonical provider in the usage
    // snapshot, so retry across providers before declaring the speed unknown.
    return providerUsage?.providers
      .flatMap { $0.models ?? [] }
      .first { $0.slug == model || $0.displayName == displayName }?
      .observedTokensPerSecond
  }

  var activeModelObservedTokensPerSecond: Double? {
    let latest = activeRequests.last
    return observedTokensPerSecond(
      providerID: latest?.provider,
      model: latest?.model ?? activeModel
    )
  }

  func sessionName(for request: RouterActiveRequest) -> String {
    guard let sessionName = request.sessionName?.trimmingCharacters(in: .whitespacesAndNewlines),
          !sessionName.isEmpty
    else { return routerLocalized("Active session") }
    return sessionName
  }

  var visibleUsageProviders: [UsageProviderChoice] {
    usageProviderChoices.filter { usageProviderHasCredentials($0.id) }
  }

  var visibleUsageCards: [UsageOverviewCard] {
    visibleUsageProviders.flatMap(usageCards(for:))
  }

  /// Every model the router has served, across all providers, heaviest first.
  var overallModelUsage: [ModelUsageRow] {
    guard let snapshot = providerUsage else { return [] }
    return snapshot.providers
      .flatMap { provider in
        (provider.models ?? []).map { model in
          ModelUsageRow(
            providerID: provider.id,
            providerName: provider.displayName,
            model: model
          )
        }
      }
      .filter { $0.model.requests > 0 }
      .sorted {
        if $0.model.totalTokens != $1.model.totalTokens {
          return $0.model.totalTokens > $1.model.totalTokens
        }
        return $0.model.requests > $1.model.requests
      }
  }

  // Issue #182. The headline card follows whatever is generating right now,
  // which answers "how fast is this" but never "how do my models compare".
  // `lastUsedAt` was already decoded and unread; this is what it was for.
  // Only measured models appear -- an unmeasured one would need a placeholder
  // row that says nothing, and the card above already covers "no samples yet".
  var recentModelSpeeds: [ModelUsageRow] {
    guard let snapshot = providerUsage else { return [] }
    return snapshot.providers
      .flatMap { provider in
        (provider.models ?? []).map { model in
          ModelUsageRow(providerID: provider.id, providerName: provider.displayName, model: model)
        }
      }
      .filter { $0.model.observedTokensPerSecond != nil }
      .sorted { ($0.model.lastUsedAt ?? "") > ($1.model.lastUsedAt ?? "") }
      .prefix(4)
      .map { $0 }
  }

  var overallTokenTotal: Int64 {
    overallModelUsage.reduce(0) { $0 + $1.model.totalTokens }
  }

  var overallRequestTotal: Int {
    overallModelUsage.reduce(0) { $0 + $1.model.requests }
  }

  func usageCards(for provider: UsageProviderChoice) -> [UsageOverviewCard] {
    if provider.id == "openai" {
      var cards: [UsageOverviewCard] = []
      if let primary = accountUsage?.primary {
        cards.append(
          UsageOverviewCard(
            id: "openai-primary",
            provider: provider,
            metric: nil,
            kindLabel: primary.durationLabel,
            remainingPercent: Double(primary.remainingPercent),
            resetDate: primary.resetDate
          )
        )
      } else {
        cards.append(
          UsageOverviewCard(
            id: "openai-primary",
            provider: provider,
            metric: nil,
            kindLabel: nil,
            remainingPercent: nil,
            resetDate: nil
          )
        )
      }
      if let secondary = accountUsage?.secondary {
        cards.append(
          UsageOverviewCard(
            id: "openai-secondary",
            provider: provider,
            metric: nil,
            kindLabel: secondary.durationLabel,
            remainingPercent: Double(secondary.remainingPercent),
            resetDate: secondary.resetDate
          )
        )
      }
      return cards
    }

    let metrics = providerUsage(for: provider.id)?.account.metrics ?? []
    if !metrics.isEmpty {
      return metrics.enumerated().map { index, metric in
        let kindLabel = metric.kind == "quota"
          ? standardizedLimitLabel(metric.label)
          : metric.label
        return UsageOverviewCard(
          id: "\(provider.id)-metric-\(index)",
          provider: provider,
          metric: metric,
          kindLabel: kindLabel,
          remainingPercent: metric.remainingPercent,
          resetDate: metric.resetDate
        )
      }
    }

    return [
      UsageOverviewCard(
        id: "\(provider.id)-local",
        provider: provider,
        metric: nil,
        kindLabel: nil,
        remainingPercent: nil,
        resetDate: nil
      )
    ]
  }

  func providerUsage(for providerID: String) -> RouterProviderUsage? {
    providerUsage?.providers.first(where: { $0.id == providerID })
  }

  // the ChatGPT rate-limit windows plus each connected provider's account
  // quota metrics.
  var desktopQuotaRows: [DesktopQuotaRow] {
    var rows: [DesktopQuotaRow] = []
    if let account = accountUsage {
      for (suffix, window) in [("primary", account.primary), ("secondary", account.secondary)] {
        guard let window else { continue }
        rows.append(DesktopQuotaRow(
          id: "openai-\(suffix)",
          providerID: "openai",
          providerName: "ChatGPT",
          label: window.durationLabel,
          usedPercent: Double(window.usedPercent),
          resetAt: window.resetsAt))
      }
    }
    for provider in usageProviderChoices where provider.id != "openai" && provider.isEnabled {
      guard let usage = providerUsage(for: provider.id) else { continue }
      for (index, metric) in usage.account.metrics.enumerated() where metric.kind == "quota" {
        guard let used = metric.usedPercent else { continue }
        rows.append(DesktopQuotaRow(
          id: "\(provider.id)-\(index)",
          providerID: provider.id,
          providerName: provider.shortName,
          label: metric.label,
          usedPercent: used,
          resetAt: metric.resetAt))
      }
    }
    return rows
  }

  func dailyTokens(days: Int) -> [Double] {
    dailyUsage(days: days).map(\.tokens)
  }

  func dailyUsage(days: Int) -> [DailyUsagePoint] {
    let buckets: [DailyUsageCacheBucket]
    if selectedUsageUsesChatGPT {
      buckets = accountUsage?.dailyUsageBuckets.map {
        DailyUsageCacheBucket(startDate: $0.startDate, tokens: $0.tokens)
      } ?? []
    } else {
      buckets = selectedProviderUsage?.dailyUsageBuckets.map {
        DailyUsageCacheBucket(startDate: $0.startDate, tokens: $0.tokens)
      } ?? []
    }
    let calendar = Calendar.current
    let today = calendar.startOfDay(for: .now)
    let cacheKey = DailyUsageCacheKey(
      providerID: selectedUsageProviderID,
      days: days,
      today: today,
      buckets: buckets
    )
    if let cached = dailyUsageCache[cacheKey] { return cached }

    let indexed = Dictionary(uniqueKeysWithValues: buckets.map {
      ($0.startDate, Double($0.tokens))
    })
    let points = (0..<days).map { offset in
      let date = calendar.date(byAdding: .day, value: offset - (days - 1), to: today) ?? today
      return DailyUsagePoint(
        date: date,
        tokens: indexed[Self.dayKeyFormatter.string(from: date)] ?? 0
      )
    }
    if dailyUsageCache.count >= 24 { dailyUsageCache.removeAll(keepingCapacity: true) }
    dailyUsageCache[cacheKey] = points
    return points
  }

  func localUsageTotals(days: Int) -> (tokens: Double, requests: Int) {
    localUsageTotals(for: selectedUsageProviderID, days: days)
  }

  func localUsageTotals(for providerID: String, days: Int) -> (tokens: Double, requests: Int) {
    guard providerID != "openai", let usage = providerUsage(for: providerID) else { return (0, 0) }
    let calendar = Calendar.current
    let today = calendar.startOfDay(for: .now)
    let buckets = usage.dailyUsageBuckets.map {
      LocalUsageTotalsCacheBucket(
        startDate: $0.startDate,
        tokens: $0.tokens,
        requests: $0.requests
      )
    }
    let cacheKey = LocalUsageTotalsCacheKey(
      providerID: providerID,
      days: days,
      today: today,
      buckets: buckets
    )
    if let cached = localUsageTotalsCache[cacheKey] {
      return (cached.tokens, cached.requests)
    }

    let firstDay = calendar.date(byAdding: .day, value: -(days - 1), to: today) ?? today
    let totals = usage.dailyUsageBuckets.reduce(into: (tokens: 0.0, requests: 0)) { totals, bucket in
      guard let date = Self.dayKeyFormatter.date(from: bucket.startDate),
            date >= firstDay,
            date <= today
      else { return }
      totals.tokens += Double(bucket.tokens)
      totals.requests += bucket.requests
    }
    if localUsageTotalsCache.count >= 48 {
      localUsageTotalsCache.removeAll(keepingCapacity: true)
    }
    localUsageTotalsCache[cacheKey] = UsageTotals(tokens: totals.tokens, requests: totals.requests)
    return totals
  }

  func localUsageSummary(for providerID: String, days: Int = 7) -> String {
    let totals = localUsageTotals(for: providerID, days: days)
    if totals.tokens > 0 {
      return "\(compactTokenCount(totals.tokens)) tok"
    }
    if totals.requests > 0 {
      return RouterLanguage.isSimplifiedChinese ? "\(totals.requests) 个请求" : "\(totals.requests) req"
    }
    return routerLocalized("No traffic")
  }

}
