import AppKit
import Combine
import Foundation
import ServiceManagement
import SwiftUI

@main
struct ModelRouterTrayApp: App {
  @NSApplicationDelegateAdaptor private var appDelegate: AppDelegate
  @ObservedObject private var store = RouterStore.shared

  var body: some Scene {
    // 完整 App：主窗口承载全部功能（红叉只关窗，App 与服务继续），
    // 菜单栏图标保留为速览/速控。
    WindowGroup {
      TrayView(store: store, presentation: .window)
        .preferredColorScheme(.dark)
    }
    .windowResizability(.contentSize)
    .defaultSize(width: 430, height: 640)

    // The insertion binding is read-only from our side: visibility is decided
    // by the presence mode, not by the system writing back.
    MenuBarExtra(isInserted: Binding(
      get: { store.surfacesVisible },
      set: { _ in }
    )) {
      TrayView(store: store, presentation: .menuBar)
        .frame(width: 352, height: 560)
    } label: {
      RouterMenuBarIcon()
    }
    .menuBarExtraStyle(.window)
  }
}

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
  let store = RouterStore.shared
  private var islandController: IslandWindowController?
  private var desktopPanelController: DesktopPanelWindowController?
  private var surfaceVisibility: AnyCancellable?

  func applicationDidFinishLaunching(_ notification: Notification) {
    // 完整 App 形态：Dock 图标 + 主窗口。菜单栏速览保留（MenuBarExtra
    // 在 regular 应用里照常工作）。LSUIElement 已从 Info.plist 移除，
    // 这里的代码值与 plist 保持一致，双保险。
    NSApp.setActivationPolicy(.regular)
    islandController = IslandWindowController(store: store)
    desktopPanelController = DesktopPanelWindowController(store: store)
    surfaceVisibility = store.$surfacesVisible
      .combineLatest(store.$islandMode)
      .sink { [weak self] visible, mode in
        self?.islandController?.setVisible(visible && mode == .notch)
        self?.desktopPanelController?.setVisible(visible && mode == .desktop)
      }
    store.startHostAppObservation()
    Task { await store.startPolling() }
    Task { await store.startActivityPolling() }
    Task { await store.startAccountUsagePolling() }
    Task { await store.startProviderPolling() }
    store.revealForUserLaunch()
    // Tray 即开关：tray 启动时若路由器没在跑则拉起（对称于退出时的
    // 停止）。探活先行 —— service start 对运行中的服务是重启，绝不
    // 能在每次 tray 启动时误伤正在服务的进程。
    Task { await store.ensureServiceRunningAtLaunch() }
  }

  // Double-clicking an app that is already running sends this instead of a
  // fresh launch. An LSUIElement app has no window and no Dock icon, so without
  // handling it the second open is silently swallowed and the app reads as
  // broken.
  func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
    store.revealForUserLaunch()
    return true
  }

  func applicationWillTerminate(_ notification: Notification) {
    store.stopServiceOnQuit()
  }
}
