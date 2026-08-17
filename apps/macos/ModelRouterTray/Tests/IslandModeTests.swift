import Testing

@testable import ModelRouterTray

// Regression for #180. The overlay covers the notch on every display, so which
// mode a launch resolves to is the whole behavior of that issue.
@Suite("Dynamic Island default")
struct IslandModeTests {
  @Test("a fresh install starts with the overlay off")
  func freshInstallIsOff() {
    #expect(
      RouterStore.resolveIslandMode(
        storedMode: nil,
        legacyVisible: nil,
        hasLaunchedBefore: false
      ) == .off
    )
  }

  @Test("an install that has launched before keeps the overlay")
  func existingInstallKeepsNotch() {
    #expect(
      RouterStore.resolveIslandMode(
        storedMode: nil,
        legacyVisible: nil,
        hasLaunchedBefore: true
      ) == .notch
    )
  }

  @Test("an explicit choice always wins", arguments: ["off", "notch", "desktop"])
  func explicitChoiceWins(raw: String) {
    let expected = IslandMode(rawValue: raw)
    #expect(
      RouterStore.resolveIslandMode(
        storedMode: raw,
        legacyVisible: nil,
        hasLaunchedBefore: false
      ) == expected
    )
    // Even against a legacy boolean that disagrees, and on a fresh install.
    #expect(
      RouterStore.resolveIslandMode(
        storedMode: raw,
        legacyVisible: false,
        hasLaunchedBefore: false
      ) == expected
    )
  }

  @Test("an unreadable stored mode falls through rather than crashing")
  func unknownStoredModeFallsThrough() {
    #expect(
      RouterStore.resolveIslandMode(
        storedMode: "sideways",
        legacyVisible: nil,
        hasLaunchedBefore: false
      ) == .off
    )
  }

  @Test("the pre-desktop-mode boolean still migrates", arguments: [true, false])
  func legacyBooleanMigrates(visible: Bool) {
    #expect(
      RouterStore.resolveIslandMode(
        storedMode: nil,
        legacyVisible: visible,
        hasLaunchedBefore: false
      ) == (visible ? .notch : .off)
    )
    // The legacy answer outranks launch history: it is an actual answer.
    #expect(
      RouterStore.resolveIslandMode(
        storedMode: nil,
        legacyVisible: visible,
        hasLaunchedBefore: true
      ) == (visible ? .notch : .off)
    )
  }
}

@Suite("Activity polling")
struct ActivityPollingTests {
  @Test("active work keeps the responsive interval")
  func activeWorkKeepsFastInterval() {
    #expect(
      RouterStore.activityPollingInterval(
        surfacesVisible: true,
        activeRequestCount: 1,
        activityState: .idle
      ) == 350_000_000
    )
    #expect(
      RouterStore.activityPollingInterval(
        surfacesVisible: true,
        activeRequestCount: 0,
        activityState: .generating
      ) == 350_000_000
    )
  }

  @Test("idle polling slows down, and hidden idle polling slows further")
  func idlePollingSlowsDown() {
    #expect(
      RouterStore.activityPollingInterval(
        surfacesVisible: true,
        activeRequestCount: 0,
        activityState: .idle
      ) == 1_000_000_000
    )
    #expect(
      RouterStore.activityPollingInterval(
        surfacesVisible: false,
        activeRequestCount: 0,
        activityState: .idle
      ) == 3_000_000_000
    )
  }

  @Test("health retries stay responsive while the router is starting")
  func startingStateStaysResponsive() {
    #expect(
      RouterStore.activityPollingInterval(
        surfacesVisible: false,
        activeRequestCount: 0,
        activityState: .starting
      ) == 350_000_000
    )
  }
}
