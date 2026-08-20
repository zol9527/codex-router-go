import AppKit
import Combine
import Foundation
import ServiceManagement
import SwiftUI

enum UsageRange: Int, CaseIterable, Identifiable {
  case week = 7
  case month = 30
  case quarter = 90

  var id: Int { rawValue }
  var label: String {
    switch self {
    case .week: return "7D"
    case .month: return "30D"
    case .quarter: return "90D"
    }
  }
}

enum TokenDisplayUnit: String, CaseIterable, Identifiable {
  case full
  case millions

  var id: Self { self }

  var label: String {
    switch self {
    case .full: return routerLocalized("Full")
    case .millions: return "M"
    }
  }

  var accessibilityLabel: String {
    switch self {
    case .full: return routerLocalized("Full token numbers")
    case .millions: return routerLocalized("Millions of tokens")
    }
  }

  func format(_ value: Double) -> String {
    let normalized = value.isFinite ? max(0, value) : 0
    switch self {
    case .full:
      return Int64(normalized.rounded()).formatted(.number.grouping(.automatic))
    case .millions:
      return "\(String(format: "%.1f", normalized / 1_000_000))M"
    }
  }
}


struct UsageProviderChoice: Identifiable {
  let id: String
  let displayName: String
  let shortName: String
  let detail: String
  let isEnabled: Bool
}

enum TrayPresenceMode: String, CaseIterable, Identifiable {
  case always
  case followCodex

  var id: String { rawValue }
  var label: String {
    switch self {
    case .always: return routerLocalized("Always")
    case .followCodex: return routerLocalized("With Codex")
    }
  }

  // `control presence` spells the modes in the router's kebab-case vocabulary.
  var controlValue: String {
    switch self {
    case .always: return "always"
    case .followCodex: return "follow-codex"
    }
  }
}

// What the tray last asked the background service to do, so a burst of
// workspace notifications does not re-issue a start the router already honored.
enum ServiceIntent {
  case unknown
  case running
  case stopped
}

enum IslandMode: String, CaseIterable, Identifiable {
  case off
  case notch
  case desktop

  var id: String { rawValue }
  var label: String {
    switch self {
    case .off: return routerLocalized("Off")
    case .notch: return routerLocalized("Notch")
    case .desktop: return routerLocalized("Desktop")
    }
  }
}

struct DesktopQuotaRow: Identifiable {
  let id: String
  let providerID: String
  let providerName: String
  let label: String
  let usedPercent: Double
  let resetAt: TimeInterval?
}

struct UsageOverviewCard: Identifiable {
  let id: String
  let provider: UsageProviderChoice
  let metric: ProviderAccountMetric?
  let kindLabel: String?
  let remainingPercent: Double?
  let resetDate: Date?

  var providerID: String { provider.id }
  var title: String { provider.displayName }
}


enum TrayTab: String, CaseIterable, Identifiable {
  case usage
  case status
  case settings

  var id: String { rawValue }

  var label: String {
    switch self {
    case .usage: return routerLocalized("Usage")
    case .status: return routerLocalized("Status")
    case .settings: return routerLocalized("Settings")
    }
  }
}

// 呈现场景：主窗口（完整 App 形态）与菜单栏弹出（速览形态）。
// 同一套内容，两处渲染 —— 窗口模式加一点呼吸空间与 App 头像。
enum TrayPresentation { case window, menuBar }
