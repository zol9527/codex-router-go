import AppKit
import Combine
import Foundation
import ServiceManagement
import SwiftUI

extension RouterStore {
  func focusUsageProvider(_ providerID: String) {
    guard usageProviderChoices.contains(where: { $0.id == providerID }) else { return }
    guard selectedUsageProviderID != providerID else { return }
    selectedUsageProviderID = providerID
    Task {
      if providerID == "openai" {
        await refreshAccountUsage()
      } else {
        await refreshProviderUsage()
      }
    }
  }

  func selectUsageProvider(_ providerID: String) {
    manuallySelectedUsageProvider = true
    focusUsageProvider(providerID)
  }

  // One click covers the whole route into a provider: install the official CLI
  // when it is missing, then go straight into its browser sign-in. Stopping
  // after the install left a row that looked finished but still had no
  // credential, and made connecting a two-click ritual for no reason.


  func resolveInitialUsageProvider() {
    guard accountUsageResolved,
          !hasResolvedInitialUsageProvider,
          !hasObservedActiveProvider
    else { return }

    let provider: String?
    if accountUsage != nil {
      provider = "openai"
    } else {
      let selectedModelProvider = snapshot.targets["codex"]?.selectedModel.flatMap { selectedModel in
        snapshot.targets["codex"]?.models.first(where: { $0.slug == selectedModel })?.provider
      }
      provider = [selectedModelProvider]
        .compactMap { $0 }
        .first(where: { $0 != "openai" && usageProviderIsAvailable($0) })
        ?? usageProviderChoices.first(where: {
          $0.id != "openai" && usageProviderIsAvailable($0.id)
        })?.id
    }

    guard let provider else { return }
    hasResolvedInitialUsageProvider = true
    focusUsageProvider(provider)
  }

  func usageProviderIsAvailable(_ providerID: String) -> Bool {
    usageProviderHasCredentials(providerID)
  }

  func usageProviderHasCredentials(_ providerID: String) -> Bool {
    if providerID == "openai" { return accountUsage != nil }
    return providerSetup[providerID]?.configured == true
  }

  func providerDetail(_ providerID: String, enabled: Set<String>) -> String {
    if enabled.contains(providerID) {
      return providerID.hasSuffix("-oauth")
        ? routerLocalized("OAuth · enabled")
        : routerLocalized("API · enabled")
    }
    if providerSetup[providerID]?.configured == true { return routerLocalized("Ready to enable") }
    return routerLocalized("Needs setup")
  }

}
