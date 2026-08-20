import AppKit
import Combine
import Foundation
import ServiceManagement
import SwiftUI


struct ModelSettingsAccordion: View {
    @ObservedObject var store: RouterStore
    let target: RouterTarget
    @State var subagentsExpanded = true
    @State var pickerExpanded = true
    @State var visionExpanded = true
    // "Subagent models" / "Model picker" 各 provider 分组的折叠状态（按
    // section:provider 键控，见 providerBinding）。
    @State var collapsedProviders = Set<String>()

    private struct ProviderModels: Identifiable {
      let provider: String
      let models: [RouterModel]
      var id: String { provider }
    }


    private var settings: ModelSettingsSnapshot? { target.modelSettings }
    private var busy: Bool { store.providerOperation == "models" }

    // Hidden models stay listed here. A model hidden from the picker cannot be
    // a subagent either, but dropping its row made it look deleted and left no
    // way back to it from this panel -- the tray must always show every model
    // it can still change.
    //
    // 列出全部已启用路由模型而不只是 v2 候选：未证明的模型通过行开关
    // 本地声明（declare）为 v2 —— 声明权在操作者手里，不再要求注册表
    // 证明 + 重编。
    private var enabledExternalModels: [RouterModel] {
      target.models
        .filter {
          $0.enabled && $0.provider != "openai"
        }
        .sorted {
          if $0.provider != $1.provider { return $0.provider < $1.provider }
          return $0.slug < $1.slug
        }
    }

    private var enabledModels: [RouterModel] {
      target.models
        .filter(\.enabled)
        .sorted {
          if $0.provider != $1.provider { return $0.provider < $1.provider }
          return $0.slug < $1.slug
        }
    }

    private func providerGroups(_ models: [RouterModel]) -> [ProviderModels] {
      Dictionary(grouping: models, by: \.provider)
        .map { ProviderModels(provider: $0.key, models: $0.value.sorted { $0.slug < $1.slug }) }
        .sorted { $0.provider < $1.provider }
    }

    private func providerName(_ id: String) -> String {
      if id == "openai" { return "OpenAI" }
      return target.providers?.first(where: { $0.id == id })?.displayName ?? id
    }

    // Keyed by section as well as provider: "Subagent models" and "Model
    // picker" list the same providers for different settings, so a shared key
    // made expanding one open the other and turned the two panels into
    // look-alikes -- which is how a subagent toggle gets mistaken for a picker
    // toggle.
    private func providerBinding(_ section: String, _ provider: String) -> Binding<Bool> {
      let key = "\(section):\(provider)"
      return Binding(
        get: { !collapsedProviders.contains(key) },
        set: { expanded in
          if expanded {
            collapsedProviders.remove(key)
          } else {
            collapsedProviders.insert(key)
          }
        }
      )
    }

    // Per-provider counts, so a click that lands on the wrong panel is visible
    // in the header it did not change instead of only in Codex's picker.
    private func subagentGroupSummary(_ group: ProviderModels) -> String {
      "\(group.models.filter { isSubagent($0) }.count) of \(group.models.count) on"
    }

    private func pickerGroupSummary(_ group: ProviderModels) -> String {
      "\(group.models.filter { !hiddenModels.contains($0.slug) }.count) of \(group.models.count) visible"
    }

    var body: some View {
      VStack(alignment: .leading, spacing: 10) {
        AccordionPanel(
          title: routerLocalized("Subagent models"),
          summary: subagentSummary,
          expanded: $subagentsExpanded
        ) {
          VStack(alignment: .leading, spacing: 8) {
            toggleRow(
              title: routerLocalized("All proven models"),
              detail: settings?.subagents.mode == "all"
                ? "Every proven v2 model can run as a subagent"
                : "Only selected proven v2 models can run as subagents",
              isOn: Binding(
                get: { settings?.subagents.mode == "all" },
                set: { enabled in
                  let current = settings?.subagents
                  let mode = enabled
                    ? "all"
                    : current?.enabled.isEmpty == false ? "selected" : "proven"
                  Task { await store.setSubagentMode(mode) }
                }
              ),
              disabled: busy
            )
            Text(routerLocalized("Subagent choices do not hide models from Codex's picker — use Model picker below for that."))
              .font(.system(size: 9))
              .foregroundStyle(routerMuted)
            Text(routerLocalized("Turning on an unproven model declares it locally as a v2 subagent; turning it off removes the declaration."))
              .font(.system(size: 9))
              .foregroundStyle(routerMuted)
            toolbar(
              buttons: [
                ("Subagents on", { Task { await store.selectAllSubagents() } }),
                ("Subagents off", { Task { await store.unselectAllSubagents() } }),
              ]
            )
            // 空态 = 没有任何已启用 provider 的路由模型（分身面板现在
            // 列全部可声明模型，不再只收 v2 候选）。
            if enabledExternalModels.isEmpty {
              Text(routerLocalized(
                "No routed models available — enable a provider first."
              ))
              .font(.system(size: 9))
              .foregroundStyle(routerMuted)
            }
            ForEach(providerGroups(enabledExternalModels)) { group in
              AccordionPanel(
                title: providerName(group.provider),
                summary: subagentGroupSummary(group),
                expanded: providerBinding("subagents", group.provider)
              ) {
                VStack(alignment: .leading, spacing: 6) {
                  toolbar(
                    buttons: [
                      ("Subagents on", {
                        Task { await store.setSubagentProvider(group.provider, enabled: true) }
                      }),
                      ("Subagents off", {
                        Task { await store.setSubagentProvider(group.provider, enabled: false) }
                      }),
                    ]
                  )
                  ForEach(group.models) { model in
                    toggleRow(
                      title: model.displayName,
                      detail: subagentDetail(for: model),
                      isOn: Binding(
                        get: { isSubagent(model) },
                        set: { enabled in
                          Task {
                            if model.proven == true {
                              // 注册表证明过的：走 disabled 收窄。
                              await store.setSubagentModel(model.slug, enabled: enabled)
                            } else if enabled {
                              // 未证明的：开 = 本地声明（顺带清掉可能
                              // 残留的 disabled 否决）。
                              await store.setSubagentModel(model.slug, enabled: true)
                              await store.declareSubagentModel(model.slug)
                            } else {
                              // 关 = 撤销本地声明。
                              await store.undeclareSubagentModel(model.slug)
                            }
                          }
                        }
                      ),
                      disabled: busy || model.visible == false
                    )
                  }
                }
              }
            }
          }
        }

        AccordionPanel(
          title: routerLocalized("Model picker"),
          summary: pickerSummary,
          expanded: $pickerExpanded
        ) {
          VStack(alignment: .leading, spacing: 8) {
            Text(routerLocalized("Hidden models stay connected but are not offered by Codex."))
              .font(.system(size: 9))
              .foregroundStyle(routerMuted)
            toolbar(
              buttons: [
                ("Show all", { Task { await store.showAllPickerModels() } }),
                ("Hide all", { Task { await store.hideAllPickerModels() } }),
              ]
            )
            ForEach(providerGroups(enabledModels)) { group in
              AccordionPanel(
                title: providerName(group.provider),
                summary: pickerGroupSummary(group),
                expanded: providerBinding("picker", group.provider)
              ) {
                VStack(alignment: .leading, spacing: 6) {
                  toolbar(
                    buttons: [
                      ("Show all", {
                        Task { await store.setPickerProvider(group.provider, visible: true) }
                      }),
                      ("Hide all", {
                        Task { await store.setPickerProvider(group.provider, visible: false) }
                      }),
                    ]
                  )
                  ForEach(group.models) { model in
                    toggleRow(
                      title: model.displayName,
                      detail: model.slug,
                      isOn: Binding(
                        get: { !hiddenModels.contains(model.slug) },
                        set: { visible in
                          Task { await store.setPickerModel(model.slug, visible: visible) }
                        }
                      ),
                      disabled: busy
                    )
                  }
                }
              }
            }
          }
        }

        // Header says "Vision" and nothing else; the state it used to summarise
        // is one line below, in the toggle's own detail.
        AccordionPanel(
          title: routerLocalized("Vision"),
          summary: "",
          expanded: $visionExpanded
        ) {
          visionPanel
        }
      }
    }

    private static let checkColumnWidth: CGFloat = 38


    // What this model is for, in one truncating phrase rather than a row of
    // competing badges: its Codex role first, then how well it reads images if
    // that has been measured.


    // Useful first: models that actually drive Codex, then the rest that can
    // chat, then image readers, then the ones that can do neither. A flat list
    // in this order groups by role without spending rows on group headers,
    // which the popover width cannot afford.


    // Lets a text-only model (DeepSeek, GLM, ...) answer about a pasted image by
    // having a vision model read it. The engine defaults to an enabled paid
    // model; local engines appear once registered. Everything maps to a
    // `control vision-bridge` command, so the tray never needs the agent.
    @ViewBuilder private var visionPanel: some View {
      VStack(alignment: .leading, spacing: 8) {
        Text(routerLocalized("Text-only models can't see images. When on, a vision model reads the paste and hands over the text."))
          .font(.system(size: 9))
          .foregroundStyle(routerMuted)
        toggleRow(
          title: routerLocalized("Read images for text-only models"),
          detail: vision?.enabled == true
            ? (RouterLanguage.isSimplifiedChinese ? "读取引擎：\(currentEngineLabel)" : "Reading via \(currentEngineLabel)")
            : routerLocalized("Off — text-only models refuse pasted images"),
          isOn: Binding(
            get: { vision?.enabled == true },
            set: { on in Task { await store.setVisionBridgeEnabled(on) } }
          ),
          disabled: busy
        )
      }
    }


    private var vision: VisionBridgeSnapshot? { settings?.visionBridge }

    // The engine is fixed (native, from the signed-in ChatGPT session); the
    // label only reports what the bridge resolved to.
    private var currentEngineLabel: String {
      guard let vision else { return routerLocalized("none") }
      return vision.resolvedEngineName ?? vision.resolvedEngine ?? routerLocalized("none")
    }

    private var hiddenModels: Set<String> {
      Set(settings?.picker.hidden ?? [])
    }

    private var disabledSubagentSet: Set<String> {
      Set(settings?.subagents.disabled ?? [])
    }

    // A local selection may withhold a proven model, but never promote an
    // unverified one to native v2 collaboration.
    private func isSubagent(_ model: RouterModel) -> Bool {
      if model.visible == false { return false }
      if model.multiAgentVersion != "v2" { return false }
      if disabledSubagentSet.contains(model.slug) { return false }
      return true
    }

    private func subagentDetail(for model: RouterModel) -> String {
      if model.visible == false { return routerLocalized("Hidden from picker — show it below to use it here") }
      if isSubagent(model) {
        return model.proven == true
          ? routerLocalized("Proven v2")
          : routerLocalized("Locally declared v2")
      }
      if model.proven == true { return routerLocalized("Not selected") }
      return routerLocalized("Off — turn on to declare it as a v2 subagent")
    }

  var subagentSummary: String {
      let count = enabledExternalModels.filter { isSubagent($0) }.count
      return RouterLanguage.isSimplifiedChinese
        ? "\(count) 个已启用 · \(settings?.subagents.mode ?? "proven")"
        : "\(count) enabled · \(settings?.subagents.mode ?? "proven")"
    }

    private var pickerSummary: String {
      let visible = enabledModels.filter { !hiddenModels.contains($0.slug) }.count
      return RouterLanguage.isSimplifiedChinese
        ? "\(visible) 个显示 · \(hiddenModels.count) 个隐藏"
        : "\(visible) visible · \(hiddenModels.count) hidden"
    }

    private func toggleRow(
      title: String,
      detail: String,
      isOn: Binding<Bool>,
      disabled: Bool
    ) -> some View {
      HStack(spacing: 12) {
        VStack(alignment: .leading, spacing: 2) {
          Text(title)
            .font(.system(size: 11, weight: .medium))
            .lineLimit(1)
          Text(detail)
            .font(.system(size: 9))
            .foregroundStyle(routerMutedStrong)
            .lineLimit(1)
            // "Reading via <engine>" carries a model name of unbounded length,
            // and the switch to the right must not be pushed off the panel.
            .truncationMode(.tail)
            .help(detail)
        }
        Spacer(minLength: 8)
        Toggle("", isOn: isOn)
          .labelsHidden()
          .toggleStyle(.switch)
          .controlSize(.mini)
          .tint(routerMint)
          .disabled(disabled)
      }
      .padding(.horizontal, 2)
    }

    private func toolbar(
      buttons: [(String, () -> Void)]
    ) -> some View {
      HStack {
        Spacer()
        ForEach(Array(buttons.enumerated()), id: \.offset) { _, entry in
          Button(entry.0, action: entry.1)
            .buttonStyle(.borderless)
            .font(.system(size: 9, weight: .medium))
            .foregroundStyle(routerMint)
            .disabled(busy)
        }
      }
    }
  }

struct AccordionPanel<Content: View>: View {
    let title: String
    let summary: String
    @Binding var expanded: Bool
    @ViewBuilder var content: () -> Content

    var body: some View {
      VStack(spacing: 0) {
        Button(action: {
          withAnimation(.easeInOut(duration: 0.16)) {
            expanded.toggle()
          }
        }) {
          HStack(spacing: 10) {
            VStack(alignment: .leading, spacing: 2) {
              Text(title)
                .font(.system(size: 12, weight: .medium))
                .lineLimit(1)
              if !summary.isEmpty {
                Text(summary)
                  .font(.system(size: 9))
                  .foregroundStyle(routerMutedStrong)
                  .lineLimit(1)
              }
            }
            Spacer()
            Image(systemName: expanded ? "chevron.down" : "chevron.right")
              .font(.system(size: 10, weight: .semibold))
              .foregroundStyle(routerMuted)
              .frame(width: 14)
          }
          .padding(10)
          .contentShape(Rectangle())
        }
        .buttonStyle(.plain)

        if expanded {
          content()
            .padding(.horizontal, 10)
            .padding(.bottom, 10)
        }
      }
      .glassCard(in: RoundedRectangle(cornerRadius: 10, style: .continuous))
    }
  }

extension TrayView {
  func sectionLabel(_ title: String, detail: String) -> some View {
    HStack {
      Text(title)
        .font(.system(size: 11, weight: .medium))
        .foregroundStyle(routerMutedStrong)
      Spacer()
      Text(detail)
        .font(.system(size: 9, weight: .regular))
        .foregroundStyle(routerMuted)
    }
    .padding(.horizontal, 2)
    .padding(.top, 1)
  }

  func settingRow(
    title: String,
    detail: String,
    isOn: Binding<Bool>,
    isDisabled: Bool = false
  ) -> some View {
    HStack(spacing: 12) {
      VStack(alignment: .leading, spacing: 3) {
        Text(title)
          .font(.system(size: 12, weight: .medium))
        Text(detail)
          .font(.system(size: 9))
          .foregroundStyle(routerMuted)
      }
      Spacer()
      Toggle("", isOn: isOn)
        .labelsHidden()
        .toggleStyle(.switch)
        .controlSize(.small)
        .tint(routerMint)
        .disabled(isDisabled)
    }
    .padding(.vertical, 1)
  }

  // 维护行双动作：Update 重发布 catalog 与集成块；Fix 跑 doctor --fix。
  // detail 行实时说明本次点击会做哪件事，网络动作永远不搞突然袭击。


  var maintenanceRow: some View {
    VStack(alignment: .leading, spacing: 6) {
      HStack(spacing: 12) {
        Text(maintenanceStatus)
          .font(.system(size: 10, weight: .medium))
          .foregroundStyle(
            store.maintenanceMessage == nil
              ? routerMutedStrong
              : store.maintenanceSucceeded
                ? routerMint
                : store.maintenanceRunning
                  ? routerAccent
                  : routerRed
          )
          .lineLimit(2)
        Spacer(minLength: 8)
        if store.maintenanceRunning {
          ProgressView()
            .controlSize(.small)
            .tint(routerAccent)
            .frame(width: 94)
            .accessibilityLabel(routerLocalized("Running Codex Router maintenance"))
        } else {
          Button {
            Task { await store.updateAndVerify() }
          } label: {
            Label(routerLocalized("Update"), systemImage: "arrow.triangle.2.circlepath")
          }
          .buttonStyle(AccentButtonStyle())
          .disabled(store.providerOperation != nil)
          .opacity(store.providerOperation == nil ? 1 : 0.5)
          .help(routerLocalized("Apply the checked-out router revision, then run the Codex doctor"))
          .accessibilityLabel(routerLocalized("Update and verify Codex Router"))
          Button {
            Task { await store.fixAndVerify() }
          } label: {
            Label(routerLocalized("Fix"), systemImage: "wrench.and.screwdriver")
          }
          .buttonStyle(AccentButtonStyle())
          .disabled(store.providerOperation != nil)
          .opacity(store.providerOperation == nil ? 1 : 0.5)
          .help(routerLocalized("Run the Codex doctor and repair managed router files"))
          .accessibilityLabel(routerLocalized("Fix Codex Router installation"))
        }
      }
      if maintenanceFailed {
        Text(maintenanceHint)
          .font(.system(size: 9))
          .foregroundStyle(routerRed.opacity(0.9))
          .lineLimit(3)
      }
    }
    .padding(10)
    .background(
      Color.primary.opacity(0.045),
      in: RoundedRectangle(cornerRadius: 10, style: .continuous)
    )
  }

  var maintenanceStatus: String {
    if store.maintenanceRunning {
      return routerLocalized("Working…")
    }
    if store.maintenanceSucceeded {
      return store.maintenanceMessage ?? routerLocalized("All good")
    }
    if maintenanceFailed {
      return routerLocalized("Update or fix failed")
    }
    return store.maintenanceMessage ?? routerLocalized("Router ready")
  }

  var maintenanceFailed: Bool {
    store.maintenanceMessage != nil &&
      !store.maintenanceSucceeded &&
      !store.maintenanceRunning
  }

  var maintenanceHint: String {
    guard let message = store.maintenanceMessage else { return "" }
    return "\(message)\nIf this keeps failing, run ./bin/support-bundle and share the path."
  }

  var emptyState: some View {
    VStack(spacing: 10) {
      Text(routerLocalized("Router unavailable"))
        .font(.system(size: 13, weight: .semibold))
      Text(routerLocalized("Run setup, then refresh this panel."))
        .font(.system(size: 11))
        .foregroundStyle(routerMuted)
    }
    .frame(maxWidth: .infinity, maxHeight: .infinity)
  }

  var footer: some View {
    HStack(spacing: 9) {
      Button(store.isRefreshing ? routerLocalized("Refreshing…") : routerLocalized("Refresh")) {
        Task {
          await store.refresh()
          await store.refreshAccountUsage()
          await store.refreshProviderUsage()
          await store.refreshProviderSetup()
        }
      }
      .buttonStyle(.plain)
      .font(.system(size: 11, weight: .medium))
      .foregroundStyle(routerAccent)
      .disabled(store.isRefreshing)

      if let message = store.message {
        Text(message)
          .lineLimit(1)
          .font(.system(size: 10))
          .foregroundStyle(Color(red: 1, green: 0.61, blue: 0.52))
      } else {
        Spacer()
        Text(store.lastUpdated.map { "\(routerLocalized("Updated")) \($0.formatted(date: .omitted, time: .shortened))" } ?? routerLocalized("Awaiting data"))
          .font(.system(size: 10, weight: .regular))
          .foregroundStyle(routerMuted)
      }

      Button(routerLocalized("Quit")) { NSApp.terminate(nil) }
        .buttonStyle(.plain)
        .font(.system(size: 11, weight: .medium))
        .foregroundStyle(routerMuted)
    }
    .padding(.top, 10)
  }
}
