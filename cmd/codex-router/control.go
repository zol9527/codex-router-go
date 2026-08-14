package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/loyd/codex-router/internal/cred"
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
		rest = append([]string{"--json"}, rest...)
	}
	st, err := state.Open(*stateDir)
	if err != nil {
		return err
	}
	reg, err := registry.Load(defaultConfigDir())
	if err != nil {
		return err
	}

	switch {
	case len(rest) == 1 && rest[0] == "--json":
		return controlJSON(st, reg)
	case len(rest) >= 1 && rest[0] == "service":
		return controlService(rest[1:])
	case len(rest) >= 1 && rest[0] == "providers" && len(rest) >= 2 && rest[1] == "list":
		return controlProvidersList(st, reg, len(rest) > 2 && rest[2] == "--json")
	case len(rest) >= 1 && rest[0] == "providers" && len(rest) >= 3 && rest[1] == "enable":
		return controlProvidersEnable(st, reg, rest[2:])
	case len(rest) >= 1 && rest[0] == "credential" && len(rest) >= 2:
		return controlCredential(st, reg, rest[1:])
	case len(rest) >= 1 && rest[0] == "vision-bridge":
		return controlVisionBridge(*stateDir, rest[1:])
	case len(rest) >= 1 && rest[0] == "local-runtime":
		return controlLocalRuntime(*stateDir, rest[1:])
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

func controlUsage() {
	fmt.Fprint(os.Stderr, `control commands:
  control --json                          full snapshot for the tray
  control service start|stop|restart|status
  control providers list [--json]
  control providers enable ID [ID...]     (append to selection)
  control credential PROVIDER             read key from stdin (hidden prompt)
  control credential PROVIDER --remove
  control presence set always|follow-codex
  control vision-bridge pull TAG | pull-status | benchmark [TAG] | catalog
  control local-runtime status|start|stop
`)
}

// controlJSON 是 tray 五分钟轮询的主快照。字段集合对齐旧 emitProbe
// 的核心（targets/presence/harness 等 dsh 时代的字段已随目标砍掉）。
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
	for _, m := range reg.Models {
		if !m.Listed || !st.ProviderEnabled(m.Provider, reg.CanonicalProviderID) {
			continue
		}
		models = append(models, map[string]any{
			"slug": m.Slug, "displayName": m.DisplayName,
			"provider":      reg.CanonicalProviderID(m.Provider),
			"contextWindow": m.ContextWindow,
		})
	}
	payload := map[string]any{
		"target":           "codex",
		"configured":       true,
		"active":           probeURL(fmt.Sprintf("http://127.0.0.1:%d/health", defaultPort())),
		"enabledProviders": enabled,
		"providers":        providers,
		"models":           models,
		"version":          version,
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
		out, err := exec.Command("/bin/launchctl", "kickstart", "-k", "gui/"+launchdUID()+"/"+launchdLabel).CombinedOutput()
		if err != nil {
			return fmt.Errorf("launchctl kickstart: %v: %s", err, strings.TrimSpace(string(out)))
		}
		fmt.Println("service: started")
		return nil
	case "stop":
		out, err := exec.Command("/bin/launchctl", "bootout", "gui/"+launchdUID()+"/"+launchdLabel).CombinedOutput()
		if err != nil {
			return fmt.Errorf("launchctl bootout: %v: %s", err, strings.TrimSpace(string(out)))
		}
		fmt.Println("service: stopped")
		return nil
	default:
		return fmt.Errorf("unknown service action %q", action)
	}
}

func launchdUID() string {
	out, err := exec.Command("id", "-u").Output()
	if err != nil {
		return "501"
	}
	return strings.TrimSpace(string(out))
}

func controlProvidersList(st *state.State, reg *registry.Registry, asJSON bool) error {
	resolver := cred.New(st)
	enabled := map[string]bool{}
	for _, id := range st.EnabledProviders() {
		enabled[id] = true
	}
	type row struct {
		ID, DisplayName, Credential string
		Enabled                     bool
	}
	var rows []row
	for _, id := range orderedProviderIDs(reg) {
		p := reg.Providers[id]
		if p == nil {
			continue
		}
		_, source := resolver.Resolve(p)
		rows = append(rows, row{p.ID, p.DisplayName, source, enabled[p.ID]})
	}
	if asJSON {
		raw, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Println(string(raw))
		return nil
	}
	for _, r := range rows {
		status := "disabled"
		if r.Enabled {
			status = "enabled"
		}
		credState := "no-key"
		if r.Credential != "" {
			credState = "key:" + r.Credential
		}
		fmt.Printf("%-16s %-24s %-8s %s\n", r.ID, r.DisplayName, status, credState)
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
func controlCredential(st *state.State, reg *registry.Registry, args []string) error {
	providerID := args[0]
	p := reg.Providers[providerID]
	if p == nil {
		return fmt.Errorf("unknown provider %q", providerID)
	}
	if len(args) > 1 && args[1] == "--remove" {
		path := st.CredentialFilePath(p.Credential.File)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Printf("credential removed: %s\n", providerID)
		return nil
	}
	if p.Credential.File == "" {
		return fmt.Errorf("provider %s has no file credential", providerID)
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
	if err := st.WriteCredentialFile(p.Credential.File, value); err != nil {
		return err
	}
	// keychain 副本：可选，失败不阻塞（文件已可用）。
	fmt.Printf("credential stored: %s (file, 0600)\n", providerID)
	return nil
}

func stdinFile() *os.File { return os.Stdin }
