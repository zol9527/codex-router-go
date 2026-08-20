import AppKit
import Combine
import Foundation
import ServiceManagement
import SwiftUI


// Keep the existing material/background treatment, but use stronger text and
// semantic accents so the compact tray remains readable over it.
let routerAccent = Color(red: 0.12, green: 0.40, blue: 0.76)
let routerMint = Color(red: 0.04, green: 0.52, blue: 0.31)
let routerYellow = Color(red: 0.68, green: 0.40, blue: 0.03)
let routerRed = Color(red: 0.72, green: 0.16, blue: 0.12)
let routerInk = Color(red: 0.035, green: 0.043, blue: 0.055)
let routerText = Color.primary.opacity(0.92)
let routerMuted = Color.primary.opacity(0.76)
let routerMutedStrong = Color.primary.opacity(0.90)


/// 菜单栏必须使用真正的 NSImage template mark；Dock 的 AppIcon 含深色底和
/// 渐变，直接缩到 18 pt 会显得像一颗不协调的按钮。`isTemplate` 把下方的
/// 黑色几何当作 alpha 蒙版交给状态栏着色：深色菜单栏是白色，浅色菜单栏则
/// 自动变深，不能再因 SwiftUI `Color.primary` 的环境解析而隐形。
struct RouterMenuBarIcon: View {
  var body: some View {
    Image(nsImage: RouterMenuBarTemplate.image)
      .renderingMode(.template)
    .accessibilityLabel("Model Router")
  }
}

/// 小号菜单栏图标独立于 Dock 的大图标。所有几何均为实心黑色，`isTemplate`
/// 让 AppKit 只取其 alpha 作为蒙版；这正是系统状态栏图标的渲染契约。
enum RouterMenuBarTemplate {
  static let image: NSImage = {
    let size = NSSize(width: 18, height: 18)
    let image = NSImage(size: size, flipped: false) { _ in
      NSColor.black.setFill()

      // 一个入站节点连接三个可选上游；曲线在小尺寸下比直角分叉更清晰。
      let links = NSBezierPath()
      links.lineWidth = 1.7
      links.lineCapStyle = .round
      links.lineJoinStyle = .round
      links.move(to: NSPoint(x: 2.3, y: 9))
      links.line(to: NSPoint(x: 7.2, y: 9))
      links.move(to: NSPoint(x: 7.2, y: 9))
      links.curve(
        to: NSPoint(x: 15.4, y: 14.7),
        controlPoint1: NSPoint(x: 9.6, y: 9),
        controlPoint2: NSPoint(x: 11.8, y: 14.7)
      )
      links.move(to: NSPoint(x: 7.2, y: 9))
      links.line(to: NSPoint(x: 15.4, y: 9))
      links.move(to: NSPoint(x: 7.2, y: 9))
      links.curve(
        to: NSPoint(x: 15.4, y: 3.3),
        controlPoint1: NSPoint(x: 9.6, y: 9),
        controlPoint2: NSPoint(x: 11.8, y: 3.3)
      )
      links.stroke()

      let nodes: [(x: CGFloat, y: CGFloat, radius: CGFloat)] = [
        (2.3, 9.0, 1.25),
        (7.2, 9.0, 1.4),
        (15.4, 14.7, 1.2),
        (15.4, 9.0, 1.2),
        (15.4, 3.3, 1.2),
      ]
      for (x, y, radius) in nodes {
        NSBezierPath(
          ovalIn: NSRect(x: x - radius, y: y - radius, width: radius * 2, height: radius * 2)
        ).fill()
      }
      return true
    }
    image.isTemplate = true
    return image
  }()
}


func standardizedLimitLabel(_ label: String) -> String {
  let lowered = label.lowercased()
  if lowered.contains("5-hour") || lowered.contains("5 hour") {
    return "5-hour limit"
  }
  if lowered.contains("7-day") || lowered.contains("7 day") {
    return "Weekly limit"
  }
  if lowered.contains("weekly") {
    return "Weekly limit"
  }
  if lowered.contains("monthly") {
    return "Monthly limit"
  }
  if lowered.contains("daily") {
    return "Daily limit"
  }
  if lowered.contains("hour") && lowered.contains("limit") {
    return label
  }
  if lowered.contains("quota") || lowered.contains("limit") {
    return label.replacingOccurrences(of: "quota", with: "limit", options: [.caseInsensitive])
  }
  return label
}

func formattedAccountMetric(_ metric: ProviderAccountMetric) -> String {
  if metric.kind == "quota", let remaining = metric.remainingPercent {
    return "\(Int(remaining.rounded()))% left"
  }
  if metric.kind == "balance", let value = metric.value {
    let formatter = NumberFormatter()
    formatter.numberStyle = .currency
    formatter.currencyCode = metric.currency ?? "USD"
    formatter.minimumFractionDigits = 2
    formatter.maximumFractionDigits = 2
    return formatter.string(from: NSNumber(value: value)) ?? String(format: "%.2f", value)
  }
  return "—"
}

func compactTokenCount(_ value: Double) -> String {
  if value >= 1_000_000_000 {
    return String(format: "%.1fB", value / 1_000_000_000)
  }
  if value >= 1_000_000 {
    return String(format: "%.1fM", value / 1_000_000)
  }
  if value >= 1_000 {
    return String(format: "%.1fK", value / 1_000)
  }
  return String(Int(value))
}

func usageResetCaption(_ date: Date) -> String {
  "Resets \(date.formatted(.dateTime.month(.abbreviated).day().hour().minute()))"
}

struct StatusBeacon: View {
  @Environment(\.accessibilityReduceMotion) private var reduceMotion
  let state: RouterActivityState

  var body: some View {
    HStack(spacing: 6) {
      // 呼吸点走 CALayer 动画而非 SwiftUI 时间线。曾两次尝试 TimelineView
      // （withAnimation(repeatForever) → .animation(minimumInterval:) →
      // .periodic）都失败，根因不在 schedule：AppKit 窗口里任何活跃
      // TimelineView 都会拖 NSHostingView 以显示帧率跑 layout pass
      // （2026-08-18 实测生成态 16-27% CPU，采样 UpdateCycle →
      // CA::Transaction::commit → NSHostingView.layout → ViewGraph
      // render，更新栈直指本视图）。CABasicAnimation 由 WindowServer
      // 在 render server 进程插值，App 进程零帧成本。
      BreathingBeaconDot(tint: state.tint, breathing: isBreathing)
        .frame(width: 14, height: 14)
        .accessibilityHidden(true)
      Text(state.label)
        .font(.system(size: 10, weight: .medium))
    }
    .foregroundStyle(state.tint)
  }

  var isBreathing: Bool {
    IslandAnimation.beaconBreathing(state: state, reduceMotion: reduceMotion)
  }
}

/// BreathingBeaconDot 的载体视图：14pt 光晕 + 7pt 实心圆两层。
/// 呼吸 = 光晕层 transform.scale 的 autoreverse 循环动画（0.9→1.28，
/// 0.72s 单程，与旧 TimelineView 版 1.44s 余弦往返同节拍）。
@MainActor
final class BeaconDotView: NSView {
  private let halo = CAShapeLayer()
  private let core = CAShapeLayer()
  var isAnimating = false

  override init(frame frameRect: NSRect) {
    super.init(frame: frameRect)
    wantsLayer = true
    halo.path = CGPath(ellipseIn: bounds, transform: nil)
    core.path = CGPath(
      ellipseIn: NSRect(x: 3.5, y: 3.5, width: 7, height: 7),
      transform: nil
    )
    layer?.addSublayer(halo)
    layer?.addSublayer(core)
  }

  @available(*, unavailable)
  required init?(coder: NSCoder) {
    fatalError("BeaconDotView is created in code only")
  }

  func apply(tint: NSColor, breathing: Bool) {
    CATransaction.begin()
    // 结构/颜色变化不做隐式 CA 过渡；动画只由显式 add 的 keyframe 驱动。
    CATransaction.setDisableActions(true)
    halo.backgroundColor = tint.withAlphaComponent(0.18).cgColor
    core.backgroundColor = tint.cgColor
    if breathing, !isAnimating {
      let scale = CABasicAnimation(keyPath: "transform.scale")
      scale.fromValue = 0.9
      scale.toValue = 1.28
      scale.duration = 0.72
      scale.autoreverses = true
      scale.repeatCount = .infinity
      scale.timingFunction = CAMediaTimingFunction(name: .easeInEaseOut)
      halo.add(scale, forKey: "breath")
      isAnimating = true
    } else if !breathing, isAnimating {
      halo.removeAnimation(forKey: "breath")
      isAnimating = false
    }
    CATransaction.commit()
  }
}

struct BreathingBeaconDot: NSViewRepresentable {
  let tint: Color
  let breathing: Bool

  func makeNSView(context: Context) -> BeaconDotView {
    BeaconDotView(frame: NSRect(x: 0, y: 0, width: 14, height: 14))
  }

  func updateNSView(_ view: BeaconDotView, context: Context) {
    view.apply(tint: NSColor(tint), breathing: breathing)
  }
}

struct AccentButtonStyle: ButtonStyle {
  func makeBody(configuration: Configuration) -> some View {
    configuration.label
      .font(.system(size: 11, weight: .semibold))
      .foregroundStyle(.white)
      .padding(.horizontal, 12)
      .padding(.vertical, 7)
      .background(routerAccent.opacity(configuration.isPressed ? 0.74 : 1), in: Capsule())
      .scaleEffect(configuration.isPressed ? 0.98 : 1)
  }
}

struct VisualEffectBlur: NSViewRepresentable {
  func makeNSView(context: Context) -> NSVisualEffectView {
    let view = NSVisualEffectView()
    view.material = .popover
    view.blendingMode = .behindWindow
    view.state = .active
    return view
  }

  func updateNSView(_ nsView: NSVisualEffectView, context: Context) {}
}
