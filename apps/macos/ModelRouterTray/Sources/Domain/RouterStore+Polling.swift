import AppKit
import Combine
import Foundation
import ServiceManagement
import SwiftUI

extension RouterStore {

  func startPolling() async {
    guard !polling else { return }
    polling = true
    defer { polling = false }
    while !Task.isCancelled {
      await refresh()
      do {
        try await Task.sleep(nanoseconds: 5 * 60 * 1_000_000_000)
      } catch {
        return
      }
    }
  }

  func refresh() async {
    isRefreshing = true
    defer { isRefreshing = false }
    do {
      let output = try await runControl(arguments: ["--json"])
      snapshot = try JSONDecoder().decode(RouterSnapshot.self, from: output)
      updateRouterPinsServiceOn(snapshot.presence?.effectiveMode == "always")
      resolveInitialUsageProvider()
      lastUpdated = .now
      message = nil
    } catch {
      message = error.localizedDescription
    }
  }

  func startActivityPolling() async {
    guard !activityPolling else { return }
    activityPolling = true
    defer { activityPolling = false }
    while !Task.isCancelled {
      await refreshActivity()
      do {
        try await Task.sleep(
          nanoseconds: Self.activityPollingInterval(
            surfacesVisible: surfacesVisible,
            activeRequestCount: activeRequestCount,
            activityState: activityState
          )
        )
      } catch {
        return
      }
    }
  }



  func startAccountUsagePolling() async {
    guard !accountUsagePolling else { return }
    accountUsagePolling = true
    defer { accountUsagePolling = false }
    while !Task.isCancelled {
      await refreshAccountUsage()
      await refreshProviderUsage()
      do {
        try await Task.sleep(nanoseconds: 30 * 1_000_000_000)
      } catch {
        return
      }
    }
  }

  func refreshAccountUsage() async {
    do {
      let output = try await runControl(arguments: ["account", "--json"])
      let nextUsage = try JSONDecoder().decode(CodexAccountUsage.self, from: output)
      if accountUsage != nextUsage { accountUsage = nextUsage }
      if accountUsageError != nil { accountUsageError = nil }
    } catch {
      let nextError = error.localizedDescription
      if accountUsageError != nextError { accountUsageError = nextError }
    }
    accountUsageResolved = true
    resolveInitialUsageProvider()
  }

  func refreshProviderUsage() async {
    do {
      let output = try await runControl(arguments: ["provider-usage", "--json"])
      let nextUsage = try JSONDecoder().decode(ProviderUsageSnapshot.self, from: output)
      if providerUsage != nextUsage { providerUsage = nextUsage }
      if providerUsageError != nil { providerUsageError = nil }
      resolveInitialUsageProvider()
    } catch {
      let nextError = error.localizedDescription
      if providerUsageError != nextError { providerUsageError = nextError }
    }
  }

  func startProviderPolling() async {
    guard !providerPolling else { return }
    providerPolling = true
    defer { providerPolling = false }
    while !Task.isCancelled {
      await refreshProviderSetup()
      do {
        try await Task.sleep(nanoseconds: 60 * 1_000_000_000)
      } catch {
        return
      }
    }
  }

  func refreshProviderSetup() async {
    do {
      let output = try await runControl(arguments: ["providers", "--json"])
      let snapshot = try JSONDecoder().decode(ProviderSetupSnapshot.self, from: output)
      let nextSetup = Dictionary(uniqueKeysWithValues: snapshot.providers.map { ($0.id, $0) })
      if providerSetup != nextSetup { providerSetup = nextSetup }
      resolveInitialUsageProvider()
    } catch {
      let nextMessage = error.localizedDescription
      if message != nextMessage { message = nextMessage }
    }
  }



  func refreshActivity() async {
    let configuredPort = ProcessInfo.processInfo.environment["MODEL_ROUTER_PORT"] ?? "4202"
    guard let url = URL(string: "http://127.0.0.1:\(configuredPort)/health") else {
      recordActivityHealthFailure()
      return
    }
    var request = URLRequest(url: url)
    request.cachePolicy = .reloadIgnoringLocalCacheData
    request.timeoutInterval = 2
    do {
      let (data, response) = try await URLSession.shared.data(for: request)
      guard (response as? HTTPURLResponse)?.statusCode == 200 else {
        throw RouterError("Router health check failed.")
      }
      let health = try JSONDecoder().decode(RouterHealth.self, from: data)
      let previousActivityState = activityState
      let nextActiveRequests = health.activity.active ?? []
      let nextActiveRequestCount = health.activity.activeCount ?? nextActiveRequests.count
      activityHealthFailureStartedAt = nil
      if activityState != health.activity.state { activityState = health.activity.state }
      if activeRequests != nextActiveRequests { activeRequests = nextActiveRequests }
      if activeRequestCount != nextActiveRequestCount {
        activeRequestCount = nextActiveRequestCount
      }
      if activeModel != health.activity.model { activeModel = health.activity.model }
      let latestActiveRequest = nextActiveRequests.last
      let activeSessionID = latestActiveRequest?.sessionId ?? latestActiveRequest?.threadId
      if let activeSessionID, activeSessionID != lastObservedSessionID {
        lastObservedSessionID = activeSessionID
        activitySessionName = nil
      }
      let activeSessionName = latestActiveRequest?.sessionName?.trimmingCharacters(in: .whitespacesAndNewlines)
      if let activeSessionID, activeSessionID == lastObservedSessionID,
         let activeSessionName, !activeSessionName.isEmpty,
         activitySessionName != activeSessionName {
        activitySessionName = activeSessionName
      } else if activeSessionID == nil,
                let sessionName = health.activity.sessionName?.trimmingCharacters(in: .whitespacesAndNewlines),
                !sessionName.isEmpty,
                lastObservedSessionID == nil {
        if activitySessionName != sessionName { activitySessionName = sessionName }
      }
      if health.activity.state == .generating,
         let provider = health.activity.provider {
        hasObservedActiveProvider = true
        if let requestID = nextActiveRequests.last?.id {
          if requestID != latestObservedActivityRequestID {
            latestObservedActivityRequestID = requestID
            manuallySelectedUsageProvider = false
          }
        } else if previousActivityState != .generating {
          // Older router health payloads may not include active request IDs.
          // Treat the transition into generating as the start of a new request.
          manuallySelectedUsageProvider = false
        }
        if !manuallySelectedUsageProvider {
          focusUsageProvider(provider)
        }
      }
      if previousActivityState == .generating, health.activity.state != .generating {
        // Pull the just-finished request into the status speed without waiting
        // for the normal 30-second account polling interval.
        Task { await refreshProviderUsage() }
      }
    } catch {
      recordActivityHealthFailure()
    }
  }

  func recordActivityHealthFailure() {
    if !activeRequests.isEmpty { activeRequests = [] }
    if activeRequestCount != 0 { activeRequestCount = 0 }
    if activeModel != nil { activeModel = nil }
    let now = Date()
    let nextState: RouterActivityState
    if let startedAt = activityHealthFailureStartedAt {
      nextState = now.timeIntervalSince(startedAt) < 30 ? .starting : .error
    } else {
      activityHealthFailureStartedAt = now
      nextState = .starting
    }
    if activityState != nextState { activityState = nextState }
  }
}
