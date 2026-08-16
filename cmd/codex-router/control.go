package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"context"
	"github.com/loyd/codex-router/internal/catalog"
	"github.com/loyd/codex-router/internal/configfile"

	"github.com/loyd/codex-router/internal/cred"
	"github.com/loyd/codex-router/internal/discover"
	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/state"
)

// cmdControl：tray 的控制面。子命令集合是旧 bin/control 的核心裁剪：
// --json / service / providers / credential。tray 通过
// `codex-router control <args>` 调用（bin/control 改写 exec 目标后零改动）。
func cmdControl(args []string) error {
	if len(args) == 0 {
		controlUsage()
		return nil
	}
	fs := flag.NewFlagSet("control", flag.ContinueOnError)
	stateDir := fs.String("state", state.DefaultDir(), "state directory")
	// tray 的调用形态是 `control --json`：把 --json 从 flag 域剥离为
	// 位置参数，避免与 --state 的解析顺序耦合。
	normalized := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--json" || arg == "-json" {
			normalized = append(normalized, "--json")
			continue
		}
		normalized = append(normalized, arg)
	}
	// --json 可能出现在任意位置；先剥出它再交给 flag 包解析其余。
	jsonFlag := false
	positional := make([]string, 0, len(normalized))
	for _, arg := range normalized {
		if arg == "--json" {
			jsonFlag = true
			continue
		}
		positional = append(positional, arg)
	}
	// Go 的 flag 解析在首个非 flag 参数处停止 —— `control vision-bridge
	// --state X pull` 的 --state 落不进 flag 域。把 --state 对从位置参数
	// 中剥出来手动应用，分发到的子命令拿到的就是纯 action 序列。
	stateFlag := ""
	cleaned := make([]string, 0, len(positional))
	for i := 0; i < len(positional); i++ {
		arg := positional[i]
		if arg == "--state" || arg == "-state" {
			if i+1 < len(positional) {
				stateFlag = positional[i+1]
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "--state=") {
			stateFlag = strings.TrimPrefix(arg, "--state=")
			continue
		}
		if strings.HasPrefix(arg, "-state=") {
			stateFlag = strings.TrimPrefix(arg, "-state=")
			continue
		}
		cleaned = append(cleaned, arg)
	}
	if err := fs.Parse(cleaned); err != nil {
		return err
	}
	rest := fs.Args()
	if stateFlag != "" {
		if err := fs.Set("state", stateFlag); err != nil {
			*stateDir = stateFlag
		}
	}
	if jsonFlag {
		rest = append(rest, "--json")
	}
	st, err := state.Open(*stateDir)
	if err != nil {
		return err
	}
	reg, err := registry.LoadWithOverlay(st.Dir, "")
	if err != nil {
		return err
	}

	switch {
	case len(rest) == 1 && rest[0] == "--json":
		return controlJSON(st, reg)
	case len(rest) >= 1 && rest[0] == "service":
		return controlService(rest[1:])
	case len(rest) >= 1 && rest[0] == "providers" && len(rest) >= 3 && rest[1] == "enable":
		return controlProvidersEnable(st, reg, rest[2:])
	// tray 调的是 `providers --json`（无 list）；enable 之外的任何
	// providers 形式都按 list 处理。
	case len(rest) >= 1 && rest[0] == "providers":
		return controlProvidersList(st, reg, hasJSONFlag(rest))
	case cutControlCommand(rest) != "":
		return fmt.Errorf("%s was removed in the go rewrite; this build serves codex routing only", cutControlCommand(rest))
	case len(rest) >= 1 && rest[0] == "credential" && len(rest) >= 2:
		return controlCredential(st, reg, rest[1:])
	case len(rest) >= 1 && rest[0] == "config" && len(rest) >= 2:
		return controlConfig(st, rest[1:])
	case len(rest) >= 1 && rest[0] == "subagents":
		return controlSubagents(st, reg, rest[1:])
	case len(rest) >= 1 && rest[0] == "picker":
		return controlPicker(st, reg, rest[1:])
	case len(rest) >= 1 && rest[0] == "models":
		return controlModels(st, reg, rest[1:])
	case len(rest) >= 1 && rest[0] == "reload":
		return controlReload(st, reg)
	case len(rest) >= 1 && rest[0] == "account":
		return controlAccount(*stateDir, reg)
	case len(rest) >= 1 && rest[0] == "provider-usage":
		return controlProviderUsage(*stateDir, reg)
	case len(rest) >= 1 && rest[0] == "probe":
		return controlProbe(st, reg, rest[1:])
	case len(rest) >= 1 && rest[0] == "vision-bridge":
		return controlVisionBridge(*stateDir, reg, rest[1:])
	case len(rest) >= 3 && rest[0] == "presence" && rest[1] == "set":
		if err := state.SetPresenceMode(st.Dir, rest[2]); err != nil {
			return err
		}
		raw, _ := json.MarshalIndent(state.PresenceSnapshot(st.Dir), "", "  ")
		fmt.Println(string(raw))
		return nil
	default:
		controlUsage()
		return fmt.Errorf("unsupported control command: %v", rest)
	}
}

// hasJSONFlag 报告参数序列里是否带 --json。
func hasJSONFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--json" {
			return true
		}
	}
	return false
}

func controlUsage() {
	fmt.Fprint(os.Stderr, `control commands:
  control --json                          full snapshot for the tray
  control service start|stop|restart|status
  control providers list [--json]
  control providers enable ID [ID...]     (append to selection)
  control credential PROVIDER             write api_key to config.toml (stdin prompt)
  control credential PROVIDER --remove    remove the provider's config.toml table
  control config init                     write the commented config.toml template
  control reload                          re-read config + refresh catalog, no restart
  control subagents status|mode <m>|select-all|unselect-all|declare <slug>|undeclare <slug>|set <slug> on|off|provider <id> on|off
  control picker set <slug> show|hide | provider <id> show|hide | all show|hide | status
  control models sync [PROVIDER]|list|remove <slug>|add PROVIDER <upstream-id>
  control presence set always|follow-codex
  control account --json | control provider-usage --json
  control probe PROVIDER [MODEL]           upstream behavior probe (models/args-visibility/usage accounting)
  control vision-bridge on|off | status | effort <level|default>
`)
}

// cutControlCommand 报告 tray 可能发出、但 go 重写已裁剪的子命令名。
// 它们的 UI 入口还在（维护卡、登录模式开关等），命中时给一行人话
// 而非整屏 usage —— 那段 stderr 会被 tray 原样显示在面板页脚。
func cutControlCommand(rest []string) string {
	if len(rest) == 0 {
		return ""
	}
	cut := map[string]bool{
		"apply": true, "doctor": true, "maintenance": true,
		"auth-mode": true, "signed-routing": true,
		"login": true, "install-cli": true,
		"harness": true, "local-models": true,
	}
	if cut[rest[0]] {
		return rest[0]
	}
	return ""
}

// controlJSON 是 tray 五分钟轮询的主快照。形状必须对齐 tray 的
// RouterSnapshot 解码器（ModelRouterTrayApp.swift）：
//
//	targets: {codex: {target, configured, active, enabledProviders,
//	                  providers[], models[]}} —— 字典而非扁平字段；
//	presence 四个字段全部非可选，缺 harnessPublished 会让整个快照
//	解码失败，面板落到「路由不可用」。
func controlJSON(st *state.State, reg *registry.Registry) error {
	enabled := st.EnabledProviders()
	enabledSet := map[string]bool{}
	for _, id := range enabled {
		enabledSet[id] = true
	}
	resolver := cred.New(st)

	providers := []map[string]any{}
	models := []map[string]any{}
	for _, id := range orderedProviderIDs(reg) {
		p := reg.Providers[id]
		if p == nil || p.VariantOf != "" {
			continue // 变体跟随家族主项，不单独展示
		}
		_, source := resolver.Resolve(p)
		providers = append(providers, map[string]any{
			"id": p.ID, "displayName": p.DisplayName, "kind": p.Kind,
			"enabled":              enabledSet[p.ID],
			"credentialConfigured": source != "",
			"credentialSource":     source,
		})
	}
	hidden := state.ReadPickerHidden(st.Dir)
	// 分身裁决与 catalog 同规：注册表证明 OR 本地声明，disabled/隐藏
	// 一票否决。UI 看到的必须是有效版本而不是注册表原始值 —— 否则
	// 本地声明的模型（declared）在设置页里凭空消失。
	subagentSettings := state.ReadSubagentSettings(st.Dir)
	resolvedSubagents := map[string]*registry.Model{}
	for _, m := range catalog.ApplySubagentMultiAgent(reg.Models, subagentSettings, hidden) {
		resolvedSubagents[m.Slug] = m
	}
	declaredSubagents := map[string]bool{}
	for _, slug := range subagentSettings.Declared {
		declaredSubagents[slug] = true
	}
	for _, m := range reg.Models {
		if !m.Listed || !st.ProviderEnabled(m.Provider, reg.CanonicalProviderID) {
			continue
		}
		entry := map[string]any{
			"slug": m.Slug, "displayName": m.DisplayName,
			"provider": reg.CanonicalProviderID(m.Provider),
			// RouterModel.enabled 非可选：能进这张表的模型都是
			// 已启用 provider 下的已发布模型，恒为 true。
			"enabled": st.ProviderEnabled(m.Provider, reg.CanonicalProviderID),
			// visible = 未被 picker 隐藏；multiAgentVersion 供分身
			// UI 判断候选资格（nil = v1，取裁决后的有效值）。
			"visible": !hidden[m.Slug],
			// proven = 注册表证明；declared = 本地声明。UI 据此区分
			// 开关语义：证明过的走 disabled 收窄，未证明的走声明通道。
			"proven":   m.MultiAgentVersion == "v2",
			"declared": declaredSubagents[m.Slug],
		}
		if resolved := resolvedSubagents[m.Slug]; resolved != nil && resolved.MultiAgentVersion == "v2" {
			entry["multiAgentVersion"] = "v2"
		}
		models = append(models, entry)
	}
	target := map[string]any{
		"target":           "codex",
		"configured":       true,
		"active":           probeURL(fmt.Sprintf("http://127.0.0.1:%d/health", defaultPort())),
		"enabledProviders": enabled,
		"providers":        providers,
		"models":           models,
		// App 设置页的「Subagent models / Model picker」区块数据源
		//（tray 的 ModelSettingsSnapshot 解码器）。
		"modelSettings": map[string]any{
			"subagents": state.SubagentSettingsSnapshot(st.Dir),
			"picker":    state.PickerSnapshot(st.Dir),
			// 视觉卡数据源：enabled/engine/effort/引擎列表/下载状态。
			"visionBridge": visionBridgeSnapshot(st, reg),
		},
	}
	payload := map[string]any{
		"targets": map[string]any{"codex": target},
		"version": version,
		// presence 块：tray 读 effectiveMode 而非自行推导
		//（两边各自推导必然漂移）。
		"presence": state.PresenceSnapshot(st.Dir),
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(raw))
	return nil
}

func orderedProviderIDs(reg *registry.Registry) []string {
	return []string{"zai-coding", "opencode-go"}
}

// controlService：App 化后的服务面 —— 不再经 launchd，直接管进程。
// 常态下服务由 Model Router App 作为子进程托管（App 退出它也退出）；
// 这里的 start 是终端救急路径（分离进程，App 之外存活），stop 对
// pidfile 里的进程发 SIGTERM —— App 托管的与终端拉起的都一样能停。
func controlService(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("service requires start|stop|restart|status")
	}
	action := args[0]
	switch action {
	case "status":
		running := probeURL(fmt.Sprintf("http://127.0.0.1:%d/health", defaultPort()))
		fmt.Printf("service: %s\n", map[bool]string{true: "running", false: "stopped"}[running])
		return nil
	case "start", "restart":
		if action == "restart" {
			if err := stopServiceByPidfile(); err != nil {
				return err
			}
		}
		return startServiceDetached()
	case "stop":
		if err := stopServiceByPidfile(); err != nil {
			return err
		}
		fmt.Println("service: stopped")
		return nil
	default:
		return fmt.Errorf("unknown service action %q", action)
	}
}

// startServiceDetached 分离进程拉起 serve（终端救急：App 没开时也能有
// 服务）。Setsid 脱离会话，stderr 追加进 router.log 保留线索。
func startServiceDetached() error {
	if probeURL(fmt.Sprintf("http://127.0.0.1:%d/health", defaultPort())) {
		fmt.Println("service: already running")
		return nil
	}
	exe, err := selfBinaryPath()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(state.DefaultDir(), "router.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		logFile = nil
	}
	cmd := exec.Command(exe, "serve",
		"--state", state.DefaultDir(), "--port", fmt.Sprintf("%d", defaultPort()))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn serve: %w", err)
	}
	if logFile != nil {
		defer logFile.Close()
	}
	// 等 /health 就绪再报成功 —— 立即返回会让调用方误判。
	for i := 0; i < 40; i++ {
		time.Sleep(250 * time.Millisecond)
		if probeURL(fmt.Sprintf("http://127.0.0.1:%d/health", defaultPort())) {
			fmt.Println("service: started")
			return nil
		}
	}
	return fmt.Errorf("serve spawned but /health did not come up within 10s (see router.log)")
}

// stopServiceByPidfile 读状态目录的 pidfile，对进程发 SIGTERM。
// pidfile 缺失/进程已死都视为已停止（清掉陈旧文件）。
func stopServiceByPidfile() error {
	pidfile := filepath.Join(state.DefaultDir(), "router.pid")
	raw, err := os.ReadFile(pidfile)
	if err != nil {
		if probeURL(fmt.Sprintf("http://127.0.0.1:%d/health", defaultPort())) {
			return fmt.Errorf("service is answering /health but wrote no pidfile; stop it from its owner (the app or its terminal)")
		}
		return nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		os.Remove(pidfile)
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal %d: %w", pid, err)
	}
	// 等优雅退出（最长 5s），超时不强杀 —— 请求有 10s 的排空预算。
	for i := 0; i < 20; i++ {
		time.Sleep(250 * time.Millisecond)
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			os.Remove(pidfile)
			return nil
		}
	}
	return nil
}

// controlProvidersList 的 JSON 输出形状对齐 tray 的
// ProviderSetupSnapshot 解码器：providers 数组，小写字段，configured
// 与 action 非可选。本 fork 的 provider 全是 API-key 型，action 恒为
// "add-key"（已配置时状态行走 configured 分支；action 决定的是未配置
// 时按钮的行为——展开隐藏输入框）。
func controlProvidersList(st *state.State, reg *registry.Registry, asJSON bool) error {
	resolver := cred.New(st)
	enabled := map[string]bool{}
	for _, id := range st.EnabledProviders() {
		enabled[id] = true
	}
	if asJSON {
		entries := []map[string]any{}
		for _, id := range orderedProviderIDs(reg) {
			p := reg.Providers[id]
			if p == nil || p.VariantOf != "" {
				continue
			}
			_, source := resolver.Resolve(p)
			entries = append(entries, map[string]any{
				"id": p.ID, "displayName": p.DisplayName, "kind": p.Kind,
				"configured": source != "",
				"action":     "add-key",
			})
		}
		raw, _ := json.MarshalIndent(map[string]any{"providers": entries}, "", "  ")
		fmt.Println(string(raw))
		return nil
	}
	for _, id := range orderedProviderIDs(reg) {
		p := reg.Providers[id]
		if p == nil {
			continue
		}
		_, source := resolver.Resolve(p)
		status := "disabled"
		if enabled[p.ID] {
			status = "enabled"
		}
		credState := "no-key"
		if source != "" {
			credState = "key:" + source
		} else if provider, name := state.DanglingEnvRef(); provider == p.ID {
			credState = "no-key (config.toml references unset variable " + name + ")"
		}
		fmt.Printf("%-16s %-24s %-8s %s\n", p.ID, p.DisplayName, status, credState)
	}
	return nil
}

func controlProvidersEnable(st *state.State, reg *registry.Registry, ids []string) error {
	current := st.EnabledProviders()
	seen := map[string]bool{}
	for _, id := range current {
		seen[id] = true
	}
	for _, id := range ids {
		if reg.Providers[id] == nil {
			return fmt.Errorf("unknown provider %q", id)
		}
		if !seen[id] {
			current = append(current, id)
			seen[id] = true
		}
	}
	if err := st.SetEnabledProviders(current); err != nil {
		return err
	}
	fmt.Printf("enabled providers: %s\n", strings.Join(current, ", "))
	fmt.Println("republish the catalog by re-running install")
	return nil
}

// controlCredential 从 stdin 读 key（tray 传 stdin，不在参数里）。
// controlCredential：凭证的家是 config.toml（Claude Code 式配置项）。
// stdin 隐藏输入的终端流程保留；tray 的 Add Key 走同一条路。改写是
// 行级手术 —— 操作者的注释与其他表原样保留。
func controlCredential(st *state.State, reg *registry.Registry, args []string) error {
	providerID := args[0]
	p := reg.Providers[providerID]
	if p == nil {
		return fmt.Errorf("unknown provider %q", providerID)
	}
	// 变体与家族主项共享一张凭证表（opencode-go-responses → opencode-go）。
	family := p.ID
	if p.VariantOf != "" {
		family = p.VariantOf
	}
	if len(args) > 1 && args[1] == "--remove" {
		if err := st.RemoveConfigTable(family); err != nil {
			return err
		}
		fmt.Printf("credential removed: %s (config.toml [%s] table)\n", providerID, family)
		return nil
	}
	fmt.Fprintf(os.Stderr, "Paste %s (input hidden, then Enter): ", p.Credential.Prompt)
	reader := bufio.NewReader(os.Stdin)
	value, err := reader.ReadString('\n')
	if err != nil && value == "" {
		return err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("empty credential; nothing written")
	}
	if err := st.WriteConfigCredential(family, value); err != nil {
		return err
	}
	fmt.Printf("credential stored: %s (config.toml [%s], 0600)\n", providerID, family)
	return nil
}

// configTemplate 是 control config init 写出的带注释模板 ——
// 每个可启用 provider 一节，api_key 以注释形态等着被填。
const configTemplate = `# Model Router 凭证配置（Claude Code 式）。
# 每个 provider 一节；api_key 即凭证。保存后下一回合请求即生效，
# 无需重启。也可在 App 的设置页填入（写入同一文件）。

[zai-coding]
# api_key = "sk-..."

[opencode-go]
# api_key = "..."
`

// controlConfig：config 子命令。init —— 文件不存在则写模板（0600）
// 并打印路径；已存在时不动，只打印路径。
func controlConfig(st *state.State, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("config requires init")
	}
	path := st.ConfigPath()
	switch args[0] {
	case "init":
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if err := os.WriteFile(path, []byte(configTemplate), 0o600); err != nil {
				return err
			}
			if err := os.Chmod(path, 0o600); err != nil {
				return err
			}
			fmt.Printf("config template written: %s\n", path)
			return nil
		}
		fmt.Printf("config already exists: %s\n", path)
		return nil
	default:
		return fmt.Errorf("unknown config action %q", args[0])
	}
}

// controlReload：不重启的"重新加载"。凭证本来逐请求解析（config.toml
// 改了即生效），这里补齐另外两样会滞留的东西：模型 catalog（需要
// codex 抓原生条目）与 config.toml 集成块。同时整文校验 config.toml
// —— 坏文件在此给出带行号的错误，而不是在每个请求上静默变成
// "missing"。服务进程全程不动。
func controlReload(st *state.State, reg *registry.Registry) error {
	if err := st.ConfigParseError(); err != nil {
		return fmt.Errorf("config.toml: %w", err)
	}
	enabled := map[string]bool{}
	for _, id := range st.EnabledProviders() {
		enabled[id] = true
	}

	// catalog 刷新（原生抓取失败不阻塞 —— 与 install 同策略）。
	mergedPath := filepath.Join(st.Dir, "merged-models.json")
	count, err := catalog.Refresh("codex", mergedPath, reg, func(providerID string) bool {
		return enabled[providerID] || enabled[reg.CanonicalProviderID(providerID)]
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[reload] native catalog capture failed: %v (catalog kept)\n", err)
	}

	// config.toml 集成块重发布（caller key 不变 → Codex 无感）。
	callerKey, err := st.CallerKey()
	if err != nil {
		return err
	}
	absState, err := filepath.Abs(st.Dir)
	if err != nil {
		return err
	}
	if err := configfile.Install(codexConfigPath(), configfile.RouterConfig{
		BaseURL:     fmt.Sprintf("http://127.0.0.1:%d/_codex-router/%s/v1", defaultPort(), callerKey),
		CatalogPath: filepath.Join(absState, "merged-models.json"),
	}); err != nil {
		return err
	}

	fmt.Printf("reloaded: %d models published, config.toml integration refreshed\n", count)
	resolver := cred.New(st)
	for _, id := range st.EnabledProviders() {
		p := reg.Providers[id]
		if p == nil {
			continue
		}
		value, source := resolver.Resolve(p)
		if value == "" {
			fmt.Printf("  credential %s: missing\n", id)
		} else {
			fmt.Printf("  credential %s: %s\n", id, source)
		}
	}
	fmt.Println("service process untouched — credentials apply per request")
	return nil
}

func stdinFile() *os.File { return os.Stdin }

// controlSubagents：Codex 协作分身设置面。语义对照 Node 版 control.mjs：
// select-all 整体替换为全开；unselect-all 显式全关（selected 模式 +
// 全部可见路由模型进 disabled）；其余按模式/单模型/provider 操作。
// 每次变更后刷新 catalog（multi_agent_version 变化只有重发布才被
// Codex 看到）并回印快照。
func controlSubagents(st *state.State, reg *registry.Registry, args []string) error {
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}
	printSnapshot := func() error {
		raw, _ := json.MarshalIndent(state.SubagentSettingsSnapshot(st.Dir), "", "  ")
		fmt.Println(string(raw))
		return nil
	}
	refresh := func() error { return controlReload(st, reg) }

	switch action {
	case "status":
		return printSnapshot()
	case "select-all":
		// 声明列表是独立通道，整体替换时保留。
		if _, err := state.ReplaceSubagentSettings(st.Dir, state.SubagentSettings{
			Mode:     state.SubagentModeAll,
			Declared: state.ReadSubagentSettings(st.Dir).Declared,
		}); err != nil {
			return err
		}
	case "unselect-all":
		// 显式全关：selected 模式 + 空 enabled + 全部可见路由模型进
		// disabled —— 一个不剩，而不是回到"只开证明过的"。
		disabled := []string{}
		for _, m := range reg.Models {
			if m.Listed && st.ProviderEnabled(m.Provider, reg.CanonicalProviderID) {
				disabled = append(disabled, m.Slug)
			}
		}
		if _, err := state.ReplaceSubagentSettings(st.Dir, state.SubagentSettings{
			Mode:     state.SubagentModeSelected,
			Disabled: disabled,
			Declared: state.ReadSubagentSettings(st.Dir).Declared,
		}); err != nil {
			return err
		}
	case "declare", "undeclare":
		// 本地 v2 声明（用户主权通道）：把注册表未证明的路由模型提为
		// 分身候选。disabled / picker 隐藏仍可一票否决。
		if len(args) < 2 {
			return fmt.Errorf("usage: control subagents %s <model-slug>", action)
		}
		if reg.BySlug(args[1]) == nil {
			return fmt.Errorf("unknown model slug: %s", args[1])
		}
		if _, err := state.SetSubagentDeclared(st.Dir, []string{args[1]}, action == "declare"); err != nil {
			return err
		}
	case "mode":
		if len(args) < 2 {
			return fmt.Errorf("usage: control subagents mode <all|selected|proven>")
		}
		if _, err := state.SetSubagentMode(st.Dir, args[1]); err != nil {
			return fmt.Errorf("unknown mode %q (choose: all, selected, proven)", args[1])
		}
	case "set":
		if len(args) < 3 || (args[2] != "on" && args[2] != "off") {
			return fmt.Errorf("usage: control subagents set <model-slug> <on|off>")
		}
		if reg.BySlug(args[1]) == nil {
			return fmt.Errorf("unknown model slug: %s", args[1])
		}
		if _, err := state.SetSubagentModels(st.Dir, []string{args[1]}, args[2] == "on"); err != nil {
			return err
		}
	case "provider":
		if len(args) < 3 || (args[2] != "on" && args[2] != "off") {
			return fmt.Errorf("usage: control subagents provider <provider-id> <on|off>")
		}
		provider := reg.CanonicalProviderID(args[1])
		slugs := []string{}
		for _, m := range reg.Models {
			if m.Listed && reg.CanonicalProviderID(m.Provider) == provider &&
				st.ProviderEnabled(m.Provider, reg.CanonicalProviderID) {
				slugs = append(slugs, m.Slug)
			}
		}
		if len(slugs) == 0 {
			return fmt.Errorf("no enabled models found for provider: %s", args[1])
		}
		if _, err := state.SetSubagentModels(st.Dir, slugs, args[2] == "on"); err != nil {
			return err
		}
	default:
		return fmt.Errorf("usage: control subagents status|mode <all|selected|proven>|select-all|unselect-all|declare <slug>|undeclare <slug>|set <slug> on|off|provider <id> on|off")
	}
	if err := refresh(); err != nil {
		return err
	}
	return printSnapshot()
}

// controlPicker：模型选择器可见性面。隐藏 = 从 picker 目录拿掉、
// 按名路由不受影响；每次变更刷新 catalog 后回印快照。
func controlPicker(st *state.State, reg *registry.Registry, args []string) error {
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}
	printSnapshot := func() error {
		raw, _ := json.MarshalIndent(state.PickerSnapshot(st.Dir), "", "  ")
		fmt.Println(string(raw))
		return nil
	}
	enabledSlugs := func() []string {
		slugs := []string{}
		for _, m := range reg.Models {
			if m.Listed && st.ProviderEnabled(m.Provider, reg.CanonicalProviderID) {
				slugs = append(slugs, m.Slug)
			}
		}
		return slugs
	}
	refresh := func() error { return controlReload(st, reg) }

	switch action {
	case "status":
		return printSnapshot()
	case "set":
		if len(args) < 3 || (args[2] != "show" && args[2] != "hide") {
			return fmt.Errorf("usage: control picker set <model-slug> <show|hide>")
		}
		if reg.BySlug(args[1]) == nil {
			return fmt.Errorf("unknown model slug: %s", args[1])
		}
		if err := state.SetPickerModels(st.Dir, []string{args[1]}, args[2] == "show"); err != nil {
			return err
		}
	case "provider":
		if len(args) < 3 || (args[2] != "show" && args[2] != "hide") {
			return fmt.Errorf("usage: control picker provider <provider-id> <show|hide>")
		}
		provider := reg.CanonicalProviderID(args[1])
		slugs := []string{}
		for _, m := range reg.Models {
			if m.Listed && reg.CanonicalProviderID(m.Provider) == provider &&
				st.ProviderEnabled(m.Provider, reg.CanonicalProviderID) {
				slugs = append(slugs, m.Slug)
			}
		}
		if len(slugs) == 0 {
			return fmt.Errorf("no enabled models found for provider: %s", args[1])
		}
		if err := state.SetPickerModels(st.Dir, slugs, args[2] == "show"); err != nil {
			return err
		}
	case "all":
		if len(args) < 2 || (args[1] != "show" && args[1] != "hide") {
			return fmt.Errorf("usage: control picker all <show|hide>")
		}
		if args[1] == "show" {
			if err := state.ClearPickerHidden(st.Dir); err != nil {
				return err
			}
		} else {
			if err := state.SetPickerModels(st.Dir, enabledSlugs(), false); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("usage: control picker status|set <slug> show|hide|provider <id> show|hide|all show|hide")
	}
	if err := refresh(); err != nil {
		return err
	}
	return printSnapshot()
}

// controlModels：动态模型注册面。
//
//	sync [PROVIDER]   发现+注册（无参数=所有已启用且有凭证的 provider）
//	add PROVIDER ID   手动注册单个上游模型
//	list / remove     覆盖层查看/移除
//
// 写入 user-models.json 覆盖层后向服务进程发 SIGUSR1 热重载注册表；
// catalog 随之刷新（picker 显示仍需重开 Codex）。
func controlModels(st *state.State, reg *registry.Registry, args []string) error {
	action := "list"
	if len(args) > 0 {
		action = args[0]
	}
	printList := func() {
		entries := registry.ReadUserModels(st.Dir)
		if len(entries) == 0 {
			fmt.Println("no dynamically registered models")
			return
		}
		for _, e := range entries {
			fmt.Printf("%-34s %-10s ctx=%-8d %s\n",
				e.Model.Slug, e.Source, e.Model.ContextWindow, e.AddedAt)
		}
	}
	switch action {
	case "list":
		printList()
		return nil
	case "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: control models remove <slug>")
		}
		entries := registry.ReadUserModels(st.Dir)
		kept := entries[:0]
		removed := false
		for _, e := range entries {
			if e.Model.Slug == args[1] {
				removed = true
				continue
			}
			kept = append(kept, e)
		}
		if !removed {
			return fmt.Errorf("no overlay entry for %s", args[1])
		}
		if err := registry.WriteUserModels(st.Dir, kept); err != nil {
			return err
		}
		if err := controlReload(st, reg); err != nil {
			return err
		}
		fmt.Printf("removed: %s\n", args[1])
		return nil
	case "add":
		if len(args) < 3 {
			return fmt.Errorf("usage: control models add PROVIDER <upstream-model-id>")
		}
		return runModelSync(st, reg, args[1], args[2])
	case "sync":
		targets := args[1:]
		if action == "add" {
			targets = args[1:2]
		}
		return runModelSyncAll(st, reg, targets)
	default:
		return fmt.Errorf("usage: control models sync [PROVIDER]|list|remove <slug>|add PROVIDER <id>")
	}
}

// runModelSyncAll 对目标 provider（空=全部已启用且有凭证）执行发现+注册。
func runModelSyncAll(st *state.State, reg *registry.Registry, targets []string) error {
	resolver := cred.New(st)
	if len(targets) == 0 {
		targets = st.EnabledProviders()
	}
	total := 0
	for _, id := range targets {
		p := reg.Providers[reg.CanonicalProviderID(id)]
		if p == nil {
			return fmt.Errorf("unknown provider %q", id)
		}
		credential, source := resolver.Resolve(p)
		if credential == "" {
			fmt.Printf("%s: no credential (set api_key in %s) — embedded registry only\n", p.ID, st.ConfigPath())
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		report := discover.Sync(ctx, st.Dir, reg, p.ID, credential)
		cancel()
		if report.Err != nil {
			fmt.Printf("%s: sync failed: %v (credential: %s)\n", p.ID, report.Err, source)
			continue
		}
		for _, e := range report.Added {
			fmt.Printf("%s: + %s (ctx=%d, %s)\n", p.ID, e.Model.Slug, e.Model.ContextWindow, e.Source)
		}
		fmt.Printf("%s: %d added (%d models.dev, %d cloned), %d already routed\n",
			p.ID, len(report.Added), report.MetaHits, report.Clones, len(report.Skipped))
		total += len(report.Added)
	}
	if total == 0 {
		fmt.Println("no new models registered")
		return nil
	}
	if err := controlReload(st, reg); err != nil {
		return err
	}
	signalServerRegistryReload()
	fmt.Println("overlay written; server registry reloaded (reopen Codex to see new picker entries)")
	return nil
}

// runModelSync 手动注册单个模型（不经过货架对照，直接建模）。
func runModelSync(st *state.State, reg *registry.Registry, providerID, upstreamID string) error {
	p := reg.Providers[reg.CanonicalProviderID(providerID)]
	if p == nil {
		return fmt.Errorf("unknown provider %q", providerID)
	}
	resolver := cred.New(st)
	credential, _ := resolver.Resolve(p)
	if credential == "" {
		return fmt.Errorf("no credential for %s — set api_key in %s first", p.ID, st.ConfigPath())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	entry := discover.BuildEntry(reg, p, upstreamID, st.Dir, ctx)
	entries := registry.ReadUserModels(st.Dir)
	for _, e := range entries {
		if e.Model.Slug == entry.Model.Slug {
			fmt.Printf("already registered: %s\n", entry.Model.Slug)
			return nil
		}
	}
	entries = append(entries, entry)
	if err := registry.WriteUserModels(st.Dir, entries); err != nil {
		return err
	}
	fmt.Printf("registered: %s (ctx=%d, source=%s)\n", entry.Model.Slug, entry.Model.ContextWindow, entry.Source)
	if err := controlReload(st, reg); err != nil {
		return err
	}
	signalServerRegistryReload()
	return nil
}

// signalServerRegistryReload 向 serve 进程发 SIGUSR1（读 router.pid）。
// 服务没跑（App 关着）就跳过 —— 下次启动自然加载覆盖层。
func signalServerRegistryReload() {
	raw, err := os.ReadFile(filepath.Join(state.DefaultDir(), "router.pid"))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return
	}
	if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
		fmt.Fprintf(os.Stderr, "[models] server reload signal failed: %v (restart picks it up)\n", err)
	}
}
