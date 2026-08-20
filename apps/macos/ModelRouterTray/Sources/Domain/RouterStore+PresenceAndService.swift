import AppKit
import Combine
import Foundation
import ServiceManagement
import SwiftUI

extension RouterStore {

  // In follow mode every tray surface and the endpoint track the Codex/ChatGPT
  // desktop apps. The process itself stays resident as the watcher — quitting
  // on app exit would leave nothing around to notice the next launch. Workspace
  // notifications are backed by polling because missing one must never strand
  // Codex without its endpoint.
  func startHostAppObservation() {
    // The mode lives in two places: UserDefaults for the tray and presence.json
    // for doctor. Only a toggle used to write the second one, so a reinstall or
    // a cleared state directory left doctor believing the router should always
    // be up while the tray was quietly stopping it. Republish on every launch.
    persistPresenceMode(presenceMode)
    let center = NSWorkspace.shared.notificationCenter
    for name in [
      NSWorkspace.didLaunchApplicationNotification,
      NSWorkspace.didTerminateApplicationNotification,
    ] {
      workspaceObservers.append(
        center.addObserver(forName: name, object: nil, queue: .main) { [weak self] _ in
          Task { @MainActor in self?.refreshHostAppRunning() }
        }
      )
    }
    refreshHostAppRunning()
    hostAppRecheck?.cancel()
    hostAppRecheck = Task { [weak self] in
      while !Task.isCancelled {
        try? await Task.sleep(for: self?.hostAppRecheckInterval ?? .seconds(5))
        guard !Task.isCancelled else { return }
        self?.refreshHostAppRunning()
      }
    }
  }

  // The mode the tray acts on. A harness turn or a TUI turn arrives over a
  // socket with no app behind it, so following the Codex apps would stop the
  // router under a user with nothing left to notice their next request.
  var effectivePresenceMode: TrayPresenceMode {
    routerPinsServiceOn ? .always : presenceMode
  }

  func updateRouterPinsServiceOn(_ pinned: Bool) {
    guard routerPinsServiceOn != pinned else { return }
    routerPinsServiceOn = pinned
    refreshSurfacesVisible()
    reconcileService()
  }

  func setPresenceMode(_ mode: TrayPresenceMode) {
    presenceMode = mode
    defaults.set(mode.rawValue, forKey: presenceModeKey)
    refreshSurfacesVisible()
    // doctor reads the mode from the router's own state directory: without it a
    // service the tray stopped on purpose reads as a crash and drives the Fix
    // button into a full repair.
    persistPresenceMode(mode)
    reconcileService()
  }

  func refreshHostAppRunning() {
    let detected = hostAppRunningNow()
    if hostAppRunning != detected { hostAppRunning = detected }
    refreshSurfacesVisible()
    reconcileService()
  }

  // Codex ships two ways: the desktop app, which has a bundle identifier, and
  // the npm CLI, which is a plain terminal process and has none. Follow mode
  // checked only the bundle identifiers, so every CLI session read as "Codex is
  // not running" -- which hid the menu bar item immediately and then stopped the
  // router thirty seconds into the user's work, exactly when it was needed. Look
  // for the process too.
  func hostAppRunningNow() -> Bool {
    let bundleMatch = hostAppBundleIDs.contains { identifier in
      NSRunningApplication.runningApplications(withBundleIdentifier: identifier)
        .contains { !$0.isTerminated }
    }
    if bundleMatch { return true }
    return Self.anyProcessRunning(named: Self.hostProcessNames)
  }

  // sysctl rather than spawning pgrep: this runs every five seconds for the
  // life of the session, and a fork/exec on that cadence is a real cost on a
  // laptop. NSRunningApplication cannot see processes that are not bundled apps,
  // so there is no AppKit answer here.
  // nonisolated: a process scan touches no actor state, and pinning it to the
  // main actor would make it unusable from anywhere but the UI.
  nonisolated static func anyProcessRunning(named names: [String]) -> Bool {
    var request: [Int32] = [CTL_KERN, KERN_PROC, KERN_PROC_ALL, 0]
    var byteCount = 0
    guard sysctl(&request, UInt32(request.count), nil, &byteCount, nil, 0) == 0, byteCount > 0
    else { return false }

    let stride = MemoryLayout<kinfo_proc>.stride
    // Processes can appear between sizing and reading, so ask for headroom and
    // trust the byte count sysctl reports back rather than the one it predicted.
    var entries = [kinfo_proc](repeating: kinfo_proc(), count: byteCount / stride + 32)
    byteCount = entries.count * stride
    let read = entries.withUnsafeMutableBytes { buffer -> Int32 in
      sysctl(&request, UInt32(request.count), buffer.baseAddress, &byteCount, nil, 0)
    }
    guard read == 0 else { return false }

    // Our own Codex does not count as "Codex is running".
    //
    // The tray polls `control account` every 30 seconds, which starts
    // `codex app-server` to read usage -- a process whose `p_comm` is exactly
    // `codex`. Counting it latches follow mode on: the tray sees Codex as
    // permanently present and never releases the router again.
    //
    // The match is a *grandchild*, not a child: the tray spawns `control`, and
    // `control` spawns Codex. So collect the parent of every process first and
    // walk the chain, rather than comparing a single ppid.
    var parentOf: [pid_t: pid_t] = [:]
    var matches: [pid_t] = []
    for index in 0..<min(byteCount / stride, entries.count) {
      let process = entries[index].kp_proc
      let identifier = process.p_pid
      parentOf[identifier] = entries[index].kp_eproc.e_ppid
      let comm = withUnsafeBytes(of: process.p_comm) { raw -> String in
        // p_comm is a fixed 17-byte field, NUL-padded rather than NUL-terminated
        // when the name fills it, so measure before decoding.
        var length = 0
        while length < raw.count, raw[length] != 0 { length += 1 }
        return String(decoding: raw[0..<length], as: UTF8.self)
      }
      if names.contains(where: { $0.compare(comm, options: .caseInsensitive) == .orderedSame }) {
        matches.append(identifier)
      }
    }
    let own = getpid()
    return matches.contains { !isDescendant($0, of: own, parentOf: parentOf) }
  }

  // Walks a pid up to an ancestor. Bounded rather than `while true`: this reads
  // a table sampled from the kernel between two sysctl calls, and a torn read
  // must not be able to spin the scan that runs every five seconds.
  nonisolated static func isDescendant(
    _ pid: pid_t,
    of ancestor: pid_t,
    parentOf: [pid_t: pid_t],
  ) -> Bool {
    var current = pid
    for _ in 0..<64 {
      if current == ancestor { return true }
      guard let parent = parentOf[current], parent != 0, parent != current else { return false }
      current = parent
    }
    return false
  }

  func refreshSurfacesVisible() {
    let pinnedByUser = userRevealUntil.map { $0 > Date() } ?? false
    // effectivePresenceMode, not presenceMode: the router pins follow mode to
    // always while a client it cannot watch is talking to it, and a user launch
    // must not undo that.
    surfacesVisible = pinnedByUser || effectivePresenceMode == .always || hostAppRunning
  }

  // Opening Model Router from Finder, Spotlight, Launchpad, or the Dock has to
  // produce a menu bar item and a live router even in follow mode with Codex
  // closed. Without this the app looked broken on exactly the launch that
  // motivates having an icon at all: double-click, nothing appears, because
  // follow mode had already decided the surfaces should stay hidden.
  //
  // Time-boxed rather than sticky, so follow mode takes over again on its own
  // and the user does not silently end up in always-on.
  func revealForUserLaunch() {
    userRevealUntil = Date().addingTimeInterval(Self.userRevealWindow)
    refreshSurfacesVisible()
    startService()
    attentionPulse &+= 1
    NSApp.activate(ignoringOtherApps: true)
    attentionRelease?.cancel()
    attentionRelease = Task { [weak self] in
      try? await Task.sleep(for: .seconds(Self.userRevealWindow))
      guard !Task.isCancelled, let self else { return }
      self.userRevealUntil = nil
      self.refreshSurfacesVisible()
      self.reconcileService()
    }
  }

  func persistPresenceMode(_ mode: TrayPresenceMode) {
    enqueueServiceWork { [weak self] in
      _ = try? await self?.runControl(arguments: ["presence", "set", mode.controlValue])
    }
  }

  // In follow mode the router runs only while Codex or ChatGPT is open. Starts
  // are immediate so the gateway is warming while the user walks to the prompt.
  // Stops are deferred: Codex restarts itself, and a request can outlive the
  // window that issued it.
  func reconcileService() {
    guard effectivePresenceMode == .followCodex else {
      pendingServiceStop?.cancel()
      pendingServiceStop = nil
      // 退出 follow 模式 = 表面回到常驻，服务必须可用：tray 之前为
      // 空闲省电停掉的服务要重新拉起。
      if serviceIntent == .stopped { startService() }
      serviceIntent = .unknown
      return
    }
    if hostAppRunning {
      pendingServiceStop?.cancel()
      pendingServiceStop = nil
      startService()
      return
    }
    // Periodic process rechecks must not restart this grace period forever.
    guard pendingServiceStop == nil else { return }
    serviceStopGeneration += 1
    let generation = serviceStopGeneration
    pendingServiceStop = Task { [weak self] in
      guard let self else { return }
      // The handle is released however this task ends, including the early
      // returns below and the ones inside `stopServiceWhenIdle`. Leaving it set
      // is what made a single spurious "Codex is running" permanent: the
      // `pendingServiceStop == nil` guard above would then refuse to schedule
      // another stop for the rest of the session. The generation check keeps a
      // cancelled task from clearing a handle a later reconcile installed.
      defer {
        if self.serviceStopGeneration == generation { self.pendingServiceStop = nil }
      }
      try? await Task.sleep(for: self.hostAppAbsenceGrace)
      guard !Task.isCancelled else { return }
      // Do not trust a possibly missed launch notification. Query the process
      // list again at the decision point before unloading the endpoint.
      self.refreshHostAppRunning()
      guard !Task.isCancelled, !self.hostAppRunning else { return }
      await self.stopServiceWhenIdle()
    }
  }

  func stopServiceWhenIdle() async {
    while !Task.isCancelled {
      guard effectivePresenceMode == .followCodex, !hostAppRunning else { return }
      if activeRequestCount == 0 && activityState == .idle { break }
      try? await Task.sleep(for: activeRequestRecheck)
      refreshHostAppRunning()
    }
    guard !Task.isCancelled, effectivePresenceMode == .followCodex, !hostAppRunning else { return }
    guard serviceIntent != .stopped else { return }
    serviceIntent = .stopped
    enqueueServiceWork { [weak self] in
      guard let self, self.effectivePresenceMode == .followCodex, !self.hostAppRunning else { return }
      await self.runServiceCommand("stop")
    }
  }

  func startService() {
    guard serviceIntent != .running else { return }
    serviceIntent = .running
    enqueueServiceWork { [weak self] in
      await self?.runServiceCommand("start")
    }
  }

  // launchctl rejects overlapping bootstrap/bootout for the same label, so every
  // service call queues behind the previous one.
  func enqueueServiceWork(_ work: @escaping @MainActor @Sendable () async -> Void) {
    let previous = serviceWork
    serviceWork = Task { [weak self] in
      _ = await previous?.value
      guard self != nil else { return }
      await work()
    }
  }

  func runServiceCommand(_ action: String) async {
    do {
      _ = try await runControl(arguments: ["service", action])
    } catch {
      // A failed stop is harmless; a failed start is not, so surface it and let
      // the next Codex launch retry from a known-unknown intent.
      serviceIntent = .unknown
      if action == "stop" { pendingServiceStop = nil }
      message = "Router \(action): \(error.localizedDescription)"
    }
    await refresh()
  }

  // 操作者规定：App 退出即服务退出（quit = off）。托管路径交给
  // ServiceSupervisor（它直接 SIGTERM 自己的子进程）；若服务是外部
  // 拉起的（~/bin CLI），以分离进程走 control service stop（pidfile
  // SIGTERM），退出不等待应答。
  func stopServiceOnQuit() {
    pendingServiceStop?.cancel()
    hostAppRecheck?.cancel()
    ServiceSupervisor.shared.stopForAppQuit()
  }

  // App 即开关的另一半：App 启动时若路由器未运行则拉起（以托管子进程
  // 形态，崩溃自动重拉）。探活先行 —— 端口已被外部实例服务时绝不
  // 再拉一个（bind 冲突 + 抢主）。
  func ensureServiceRunningAtLaunch() async {
    await ServiceSupervisor.shared.startIfNotRunning()
  }
}
