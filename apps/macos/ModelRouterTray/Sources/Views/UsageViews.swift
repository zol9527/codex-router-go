import AppKit
import Combine
import Foundation
import ServiceManagement
import SwiftUI

struct ProviderUsageSection: View {
  @ObservedObject var store: RouterStore
  @State var range: UsageRange = .week
  @AppStorage("ModelRouterTray.tokenDisplayUnit") private var tokenDisplayUnitRawValue =
    TokenDisplayUnit.full.rawValue

  var tokenDisplayUnit: TokenDisplayUnit {
    TokenDisplayUnit(rawValue: self.tokenDisplayUnitRawValue) ?? .full
  }

  var body: some View {
    VStack(alignment: .leading, spacing: 12) {
      if quotaCards.isEmpty {
        HStack(alignment: .firstTextBaseline) {
          VStack(alignment: .leading, spacing: 3) {
            Text(sectionTitle)
              .font(.system(size: 12, weight: .medium))
            Text(limitDetail)
              .font(.system(size: 9))
              .foregroundStyle(routerMuted)
          }
          Spacer()
          Text(primaryMetric)
            .font(.system(size: 20, weight: .semibold))
            .monospacedDigit()
        }
      } else {
        HStack(alignment: .top, spacing: 8) {
          ForEach(quotaCards) { card in
            CurrentUsageLimitCard(card: card)
          }
        }
      }

      HStack(alignment: .firstTextBaseline) {
        Text(routerLocalized(store.selectedUsageUsesChatGPT ? "Daily token usage" : "Router traffic"))
          .font(.system(size: 10, weight: .medium))
          .foregroundStyle(routerMuted)
        Spacer()
        HStack(spacing: 5) {
          UsageRangePicker(selection: $range)
          TokenDisplayUnitPicker(selection: Binding(
            get: { self.tokenDisplayUnit },
            set: { self.tokenDisplayUnitRawValue = $0.rawValue }
          ))
        }
      }

      UsageBarChart(
        points: store.dailyUsage(days: range.rawValue),
        tint: routerAccent,
        tokenDisplayUnit: self.tokenDisplayUnit)
        .id("\(store.selectedUsageProviderID)-\(range.rawValue)-\(self.tokenDisplayUnit.rawValue)")
        .frame(height: 88)

      HStack {
        Text(rangeCaption)
        Spacer()
        if store.selectedUsageUsesChatGPT,
           let streak = store.accountUsage?.summary.currentStreakDays {
          Text(RouterLanguage.isSimplifiedChinese ? "连续 \(streak) 天" : "\(streak)-day streak")
        }
      }
      .font(.system(size: 9))
      .foregroundStyle(routerMuted)

      if let error = usageError {
        Text(error)
          .font(.system(size: 10))
          .foregroundStyle(routerRed)
          .lineLimit(2)
      }

      if let accountMessage {
        Text(accountMessage)
          .font(.system(size: 9))
          .foregroundStyle(routerMuted)
          .lineLimit(2)
      }

      if let dashboardURL {
        Button(routerLocalized("Open usage dashboard")) {
          NSWorkspace.shared.open(dashboardURL)
        }
        .buttonStyle(.link)
        .font(.system(size: 9))
      }
    }
    .padding(.vertical, 2)
  }

  var dashboardURL: URL? {
    guard !store.selectedUsageUsesChatGPT,
          let raw = store.selectedProviderUsage?.account.dashboardUrl
    else { return nil }
    return URL(string: raw)
  }

  var sectionTitle: String {
    if store.selectedUsageUsesChatGPT { return routerLocalized("ChatGPT subscription") }
    return store.selectedProviderUsage?.displayName ?? store.selectedUsageProvider.displayName
  }

  var primaryMetric: String {
    if store.selectedUsageUsesChatGPT {
      guard let value = store.accountUsage?.primary?.remainingPercent else { return "—" }
      return RouterLanguage.isSimplifiedChinese ? "剩余 \(value)%" : "\(value)% left"
    }
    guard store.providerUsage != nil else { return "—" }
    if let metric = store.selectedAccountMetric { return formattedAccountMetric(metric) }
    return self.tokenDisplayUnit.format(store.localUsageTotals(days: range.rawValue).tokens)
  }

  var quotaCards: [UsageOverviewCard] {
    store.usageCards(for: store.selectedUsageProvider).filter { card in
      if store.selectedUsageUsesChatGPT {
        return card.remainingPercent != nil
      }
      return card.metric?.kind == "quota"
    }
  }

  var limitDetail: String {
    if !store.selectedUsageUsesChatGPT {
      guard store.selectedUsageProvider.isEnabled else { return store.selectedUsageProvider.detail }
      guard let usage = store.selectedProviderUsage else { return routerLocalized("Loading provider usage…") }
      if let metric = usage.account.metrics.first {
        if let detail = metric.detail, !detail.isEmpty { return detail }
        return standardizedLimitLabel(metric.label)
      }
      return RouterLanguage.isSimplifiedChinese
        ? "\(usage.credentialType.uppercased()) 流量 · \(routerLocalized("measured on this Mac"))"
        : "\(usage.credentialType.uppercased()) traffic · measured on this Mac"
    }
    return routerLocalized("Loading native Codex usage…")
  }

  var rangeCaption: String {
    let total = store.dailyTokens(days: range.rawValue).reduce(0, +)
    let formattedTotal = self.tokenDisplayUnit.format(total)
    if !store.selectedUsageUsesChatGPT {
      let requests = store.localUsageTotals(days: range.rawValue).requests
      return RouterLanguage.isSimplifiedChinese
        ? "\(formattedTotal) token · \(requests) 个请求 · 近 \(range.rawValue) 天"
        : "\(formattedTotal) tokens · \(requests) requests over \(range.rawValue) days"
    }
    return RouterLanguage.isSimplifiedChinese
      ? "\(formattedTotal) token · 近 \(range.rawValue) 天"
      : "\(formattedTotal) tokens over \(range.rawValue) days"
  }

  var usageError: String? {
    if store.selectedUsageUsesChatGPT {
      return store.accountUsage == nil ? store.accountUsageError : nil
    }
    return store.providerUsage == nil ? store.providerUsageError : nil
  }

  var accountMessage: String? {
    guard !store.selectedUsageUsesChatGPT else { return nil }
    guard store.selectedUsageProvider.isEnabled else {
      return routerLocalized("Set up this provider below to fetch its account usage.")
    }
    guard store.selectedProviderUsage?.account.metrics.isEmpty == true else { return nil }
    return store.selectedProviderUsage?.account.message
  }
}

struct CurrentUsageLimitCard: View {
  let card: UsageOverviewCard

  var body: some View {
    VStack(alignment: .leading, spacing: 7) {
      HStack(alignment: .firstTextBaseline, spacing: 6) {
        Text(card.kindLabel ?? routerLocalized("Usage limit"))
          .font(.system(size: 10, weight: .medium))
          .lineLimit(1)
        Spacer(minLength: 4)
        Text(metricText)
          .font(.system(size: 14, weight: .semibold))
          .monospacedDigit()
      }

      if let remainingFraction {
        GeometryReader { geometry in
          ZStack(alignment: .leading) {
            Capsule().fill(Color.primary.opacity(0.09))
            Capsule()
              .fill(routerAccent.opacity(0.84))
              .frame(width: geometry.size.width * remainingFraction)
          }
        }
        .frame(height: 4)
      }

      Text(resetText)
        .font(.system(size: 8.5))
        .foregroundStyle(routerMuted)
        .lineLimit(1)
    }
    .padding(10)
    .frame(maxWidth: .infinity, minHeight: 65, alignment: .leading)
    .glassCard(in: RoundedRectangle(cornerRadius: 10, style: .continuous))
  }

  var metricText: String {
    if let metric = card.metric { return formattedAccountMetric(metric) }
    guard let remaining = card.remainingPercent else { return "—" }
    return RouterLanguage.isSimplifiedChinese
      ? "剩余 \(Int(remaining.rounded()))%"
      : "\(Int(remaining.rounded()))% left"
  }

  var resetText: String {
    guard let reset = card.resetDate else { return routerLocalized("No reset reported") }
    return usageResetCaption(reset)
  }

  var remainingFraction: CGFloat? {
    guard let remaining = card.remainingPercent else { return nil }
    return CGFloat(max(0, min(100, remaining))) / 100
  }
}

struct ModelUsageBreakdown: View {
  @ObservedObject var store: RouterStore

  private static let visibleRowLimit = 8

  var rows: [ModelUsageRow] {
    Array(store.overallModelUsage.prefix(Self.visibleRowLimit))
  }

  var hiddenCount: Int {
    max(0, store.overallModelUsage.count - rows.count)
  }

  var heaviestTokens: Double {
    Double(rows.map(\.model.totalTokens).max() ?? 0)
  }

  var body: some View {
    VStack(alignment: .leading, spacing: 8) {
      ForEach(rows) { row in
        VStack(alignment: .leading, spacing: 3) {
          HStack(spacing: 6) {
            Text(row.model.displayName)
              .font(.system(size: 10, weight: .medium))
              .lineLimit(1)
            Text(row.providerName)
              .font(.system(size: 8))
              .foregroundStyle(routerMuted)
              .lineLimit(1)
            Spacer(minLength: 6)
            Text(primaryLabel(for: row))
              .font(.system(size: 10, weight: .semibold))
              .monospacedDigit()
          }

          GeometryReader { geometry in
            ZStack(alignment: .leading) {
              Capsule().fill(Color.primary.opacity(0.09))
              Capsule()
                .fill(routerAccent.opacity(0.84))
                .frame(width: geometry.size.width * fraction(for: row))
            }
          }
          .frame(height: 4)

          Text(detailLabel(for: row))
            .font(.system(size: 8))
            .foregroundStyle(routerMuted)
            .lineLimit(1)
        }
      }

      if hiddenCount > 0 {
        Text(
          RouterLanguage.isSimplifiedChinese
            ? "还有 \(hiddenCount) 个模型"
            : "+\(hiddenCount) more model\(hiddenCount == 1 ? "" : "s")"
        )
          .font(.system(size: 8.5))
          .foregroundStyle(routerMuted)
      }
    }
  }

  func fraction(for row: ModelUsageRow) -> Double {
    guard heaviestTokens > 0 else { return 0 }
    return min(1, Double(row.model.totalTokens) / heaviestTokens)
  }

  func primaryLabel(for row: ModelUsageRow) -> String {
    guard row.model.totalTokens > 0 else {
      return RouterLanguage.isSimplifiedChinese ? "\(row.model.requests) 个请求" : "\(row.model.requests) req"
    }
    return RouterLanguage.isSimplifiedChinese
      ? "\(compactTokenCount(Double(row.model.totalTokens))) token"
      : "\(compactTokenCount(Double(row.model.totalTokens))) tok"
  }

  func detailLabel(for row: ModelUsageRow) -> String {
    // A model with traffic but no metered response carries no token counts;
    // say so rather than implying it burned nothing.
    guard row.model.totalTokens > 0 else {
      return RouterLanguage.isSimplifiedChinese
        ? "\(row.model.requests) 个请求 · 未计量"
        : "\(row.model.requests) req · not metered"
    }
    let input = compactTokenCount(Double(row.model.inputTokens))
    let output = compactTokenCount(Double(row.model.outputTokens))
    return RouterLanguage.isSimplifiedChinese
      ? "输入 \(input) · 输出 \(output) · \(row.model.requests) 个请求"
      : "\(input) in · \(output) out · \(row.model.requests) req"
  }
}

struct AllProviderUsageGrid: View {
  @ObservedObject var store: RouterStore

  private let columns = [
    GridItem(.flexible(), spacing: 8),
    GridItem(.flexible(), spacing: 8),
  ]

  var body: some View {
    LazyVGrid(columns: columns, spacing: 8) {
      ForEach(store.visibleUsageCards) { card in
        AllProviderUsageCard(store: store, card: card)
      }
    }
  }
}

struct AllProviderUsageCard: View {
  @ObservedObject var store: RouterStore
  let card: UsageOverviewCard

  var body: some View {
    Button {
      store.selectUsageProvider(card.providerID)
    } label: {
      VStack(alignment: .leading, spacing: 7) {
        HStack(spacing: 6) {
          Circle()
            .fill(card.providerID == store.selectedUsageProviderID ? store.activityState.tint : statusTint)
            .frame(width: 6, height: 6)
          Text(card.title)
            .font(.system(size: 10, weight: .medium))
            .lineLimit(1)
          Spacer(minLength: 4)
        }

        Text(metricText)
          .font(.system(size: 16, weight: .semibold))
          .monospacedDigit()

        if let remainingFraction {
          GeometryReader { geometry in
            ZStack(alignment: .leading) {
              Capsule().fill(Color.primary.opacity(0.09))
              Capsule()
                .fill(routerAccent.opacity(0.84))
                .frame(width: geometry.size.width * remainingFraction)
            }
          }
          .frame(height: 4)
        }

        Text(detailText)
          .font(.system(size: 8.5))
          .foregroundStyle(routerMuted)
          .lineLimit(1)

        Text(footerText)
          .font(.system(size: 8))
          .foregroundStyle(routerMuted)
          .lineLimit(1)
      }
      .padding(10)
      .frame(maxWidth: .infinity, minHeight: 98, alignment: .leading)
      .glassCard(in: RoundedRectangle(cornerRadius: 10, style: .continuous))
      .overlay(
        RoundedRectangle(cornerRadius: 10, style: .continuous)
          .stroke(
            card.providerID == store.selectedUsageProviderID ? routerAccent.opacity(0.45) : Color.clear,
            lineWidth: 0.75
          )
      )
    }
    .buttonStyle(.plain)
    .help(
      RouterLanguage.isSimplifiedChinese
        ? "显示 \(card.provider.displayName) 用量"
        : "Show \(card.provider.displayName) usage"
    )
    .accessibilityLabel(
      RouterLanguage.isSimplifiedChinese
        ? "显示 \(card.provider.displayName) 用量"
        : "Show \(card.provider.displayName) usage"
    )
  }

  var account: ProviderAccountUsage? {
    store.providerUsage(for: card.providerID)?.account
  }

  var oauthNeedsReconnect: Bool {
    guard account?.status == "unavailable" else { return false }
    return account?.message?.localizedCaseInsensitiveContains("login") == true
  }

  var localTotals: (tokens: Double, requests: Int) {
    store.localUsageTotals(for: card.providerID, days: 7)
  }

  var metricText: String {
    if oauthNeedsReconnect { return routerLocalized("Reconnect") }
    if let metric = card.metric { return formattedAccountMetric(metric) }
    if let remaining = card.remainingPercent {
      return RouterLanguage.isSimplifiedChinese
        ? "剩余 \(Int(remaining.rounded()))%"
        : "\(Int(remaining.rounded()))% left"
    }
    if card.providerID == "openai" { return "—" }
    return store.localUsageSummary(for: card.providerID, days: 7)
  }

  var detailText: String {
    if oauthNeedsReconnect { return routerLocalized("OAuth expired · reconnect below") }
    if let kindLabel = card.kindLabel {
      return kindLabel
    }
    if card.providerID == "openai" {
      return store.accountUsage?.primary?.durationLabel ?? "Weekly limit"
    }
    if localTotals.requests > 0 || localTotals.tokens > 0 {
      if localTotals.tokens > 0, localTotals.requests > 0 {
        return RouterLanguage.isSimplifiedChinese
          ? "近 7 天本地 · \(localTotals.requests) 个请求"
          : "7D local · \(localTotals.requests) requests"
      }
      if localTotals.requests > 0 {
        return RouterLanguage.isSimplifiedChinese ? "近 7 天本地 · 未报告 token" : "7D local · tokens not reported"
      }
      return RouterLanguage.isSimplifiedChinese ? "近 7 天本地流量" : "7D local traffic"
    }
    if card.provider.isEnabled { return routerLocalized("No router traffic yet") }
    return routerLocalized("Configured · currently hidden")
  }

  var footerText: String {
    if oauthNeedsReconnect { return routerLocalized("Sign in again to restore quota") }
    if let reset = card.resetDate {
      return usageResetCaption(reset)
    }
    if card.metric != nil || card.providerID == "openai" {
      return routerLocalized("No reset reported")
    }
    return routerLocalized("Local router traffic")
  }

  var remainingFraction: CGFloat? {
    guard let remaining = card.remainingPercent else { return nil }
    return CGFloat(max(0, min(100, remaining))) / 100
  }

  var statusTint: Color {
    if card.providerID == "openai" || card.provider.isEnabled { return routerMint }
    return routerMuted
  }
}

struct UsageRangePicker: View {
  @Binding var selection: UsageRange

  var body: some View {
    HStack(spacing: 2) {
      ForEach(UsageRange.allCases) { range in
        Button(range.label) { selection = range }
          .buttonStyle(.plain)
          .font(.system(size: 9, weight: .medium))
          .foregroundStyle(selection == range ? routerText : routerMuted)
          .padding(.horizontal, 7)
          .padding(.vertical, 4)
          .background(
            selection == range ? Color.primary.opacity(0.10) : Color.clear,
            in: Capsule()
          )
      }
    }
    .padding(2)
    .glassCard(in: Capsule())
  }
}

struct TokenDisplayUnitPicker: View {
  @Binding var selection: TokenDisplayUnit

  var body: some View {
    HStack(spacing: 2) {
      ForEach(TokenDisplayUnit.allCases) { unit in
        Button(unit.label) { self.selection = unit }
          .buttonStyle(.plain)
          .font(.system(size: 9, weight: .medium))
          .foregroundStyle(self.selection == unit ? routerText : routerMuted)
          .padding(.horizontal, 6)
          .padding(.vertical, 4)
          .background(
            self.selection == unit ? Color.primary.opacity(0.10) : Color.clear,
            in: Capsule()
          )
          .accessibilityLabel(unit.accessibilityLabel)
          .accessibilityAddTraits(self.selection == unit ? .isSelected : [])
      }
    }
    .padding(2)
    .glassCard(in: Capsule())
    .accessibilityLabel(routerLocalized("Token unit"))
  }
}

struct UsageBarChart: View {
  let points: [DailyUsagePoint]
  let tint: Color
  var tokenDisplayUnit: TokenDisplayUnit = .full
  var showsAxis = true

  @State var hoveredDate: Date?

  var body: some View {
    GeometryReader { geometry in
      let maximum = max(points.map(\.tokens).max() ?? 0, 1)
      let spacing: CGFloat = points.count > 45 ? 1 : points.count > 14 ? 2 : 4
      let width = max(
        1,
        (geometry.size.width - spacing * CGFloat(max(0, points.count - 1))) /
          CGFloat(max(points.count, 1))
      )
      let axisHeight: CGFloat = showsAxis ? 14 : 0
      let chartHeight = max(1, geometry.size.height - axisHeight)

      ZStack(alignment: .top) {
        VStack(spacing: 2) {
          HStack(alignment: .bottom, spacing: spacing) {
            ForEach(points) { point in
              VStack(spacing: 0) {
                Spacer(minLength: 0)
                RoundedRectangle(cornerRadius: min(2.5, width / 2), style: .continuous)
                  .fill(point.tokens == 0 ? Color.primary.opacity(0.07) : tint.opacity(0.86))
                  .frame(height: max(2, chartHeight * CGFloat(point.tokens / maximum)))
              }
              .frame(width: width, height: chartHeight)
              .contentShape(Rectangle())
              .onHover { hovering in
                if hovering {
                  hoveredDate = point.date
                } else if hoveredDate == point.date {
                  hoveredDate = nil
                }
              }
              .help(hoverText(for: point))
            }
          }

          if showsAxis {
            ZStack(alignment: .leading) {
              ForEach(Array(points.enumerated()), id: \.element.id) { index, point in
                if shouldLabel(index: index) {
                  Text(axisLabel(for: point))
                    .font(.system(size: 7.5, weight: .medium))
                    .foregroundStyle(routerMuted)
                    .fixedSize()
                    .position(
                      x: min(
                        geometry.size.width - 8,
                        max(8, width / 2 + CGFloat(index) * (width + spacing))
                      ),
                      y: 5
                    )
                }
              }
            }
            .frame(height: 12)
          }
        }

        if let point = hoveredPoint {
          Text(hoverText(for: point))
            .font(.system(size: 9, weight: .medium, design: .monospaced))
            .foregroundStyle(routerText)
            .padding(.horizontal, 8)
            .padding(.vertical, 5)
            .background(.regularMaterial, in: Capsule())
            .overlay(Capsule().stroke(Color.primary.opacity(0.12), lineWidth: 0.5))
            .allowsHitTesting(false)
        }
      }
    }
    .accessibilityLabel(routerLocalized("Daily token usage chart. Hover a day for its displayed token count."))
  }

  var hoveredPoint: DailyUsagePoint? {
    guard let hoveredDate else { return nil }
    return points.first(where: { $0.date == hoveredDate })
  }

  func shouldLabel(index: Int) -> Bool {
    let stride = points.count <= 7 ? 1 : points.count <= 31 ? 5 : 15
    return index.isMultiple(of: stride) || index == points.count - 1
  }

  func axisLabel(for point: DailyUsagePoint) -> String {
    if points.count <= 7 {
      return point.date.formatted(.dateTime.weekday(.abbreviated))
    }
    return point.date.formatted(.dateTime.month(.defaultDigits).day())
  }

  func hoverText(for point: DailyUsagePoint) -> String {
    let date = point.date.formatted(.dateTime.weekday(.abbreviated).month(.abbreviated).day())
    let tokens = self.tokenDisplayUnit.format(point.tokens)
    return RouterLanguage.isSimplifiedChinese ? "\(date) · \(tokens) token" : "\(date) · \(tokens) tokens"
  }
}
