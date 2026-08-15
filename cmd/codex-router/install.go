package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/loyd/codex-router/internal/catalog"
	"github.com/loyd/codex-router/internal/configfile"
	"github.com/loyd/codex-router/internal/cred"
	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/state"
)

// cmdInstall：secret 生成 → catalog 发布 → config.toml 集成。
// 幂等：重复执行等于刷新。
//
// App 化后服务由 App 子进程托管（launchd 已移除），install 只负责
// Codex 侧的集成物：config.toml 标记块 + 模型 catalog + caller secret。
// 注册表内嵌在二进制里，不再有目录拷贝。
func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	stateDir := fs.String("state", state.DefaultDir(), "state directory")
	configDir := fs.String("config", "", "registry config directory override (default: embedded registry)")
	port := fs.Int("port", defaultPort(), "listen port")
	providers := fs.String("providers", "zai-coding,opencode-go", "comma-separated provider ids to enable")
	codexBinary := fs.String("codex", "codex", "codex CLI binary (for native catalog capture)")
	dryRun := fs.Bool("dry-run", false, "print planned actions without writing")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := state.Open(*stateDir)
	if err != nil {
		return err
	}
	if err := st.EnsureSecrets(); err != nil {
		return err
	}
	ids := splitCSV(*providers)
	if err := st.SetEnabledProviders(ids); err != nil {
		return err
	}

	reg, err := registry.LoadDefault(*configDir)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if reg.Providers[id] == nil {
			return fmt.Errorf("unknown provider %q (not in registry)", id)
		}
	}

	// catalog 发布。
	mergedPath := filepath.Join(st.Dir, "merged-models.json")
	count, err := catalog.Refresh(*codexBinary, mergedPath, reg, func(providerID string) bool {
		for _, id := range ids {
			if id == providerID || reg.CanonicalProviderID(providerID) == id {
				return true
			}
		}
		return false
	})
	if err != nil {
		// 原生目录抓取失败不阻塞安装：Codex 未装/未登录时 picker
		// 只缺原生条目，路由模型仍然可用。
		fmt.Fprintf(os.Stderr, "[install] native catalog capture failed: %v (continuing)\n", err)
		fallback := catalog.Build(nil, reg.Models, func(providerID string) bool { return true }, false)
		if err := catalog.Write(mergedPath, fallback); err != nil {
			return err
		}
		count = 0
	}

	// config.toml 集成。
	callerKey, err := st.CallerKey()
	if err != nil {
		return err
	}
	absState, err := filepath.Abs(st.Dir)
	if err != nil {
		return err
	}
	routerCfg := configfile.RouterConfig{
		BaseURL:     fmt.Sprintf("http://127.0.0.1:%d/_codex-router/%s/v1", *port, callerKey),
		CatalogPath: filepath.Join(absState, "merged-models.json"),
	}
	codexConfig := codexConfigPath()
	if *dryRun {
		fmt.Printf("would write catalog: %s (%d models)\n", mergedPath, count)
		fmt.Printf("would install config blocks in: %s\n", codexConfig)
		fmt.Printf("base URL: http://127.0.0.1:%d/_codex-router/[REDACTED]/v1\n", *port)
		fmt.Println("service runs inside the Model Router app (launchd no longer involved)")
		return nil
	}
	if err := configfile.Install(codexConfig, routerCfg); err != nil {
		return err
	}

	fmt.Printf("installed: %d models published, config.toml integrated\n", count)
	fmt.Printf("state: %s\n", st.Dir)
	fmt.Println("service: starts with the Model Router app (open the app to serve)")
	return nil
}

func splitCSV(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func orDefault(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

// cmdUninstall： uninstall's job is to leave no trace of itself —
// launchd plist unloaded and removed, the installed binary directory
// deleted, config.toml blocks restored. State (credentials, usage history,
// settings) is operator data and survives by default; --purge destroys it
// too, as an explicit opt-in.
func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "also delete the state directory (credentials, usage history) — destructive")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := configfile.Uninstall(codexConfigPath()); err != nil {
		return err
	}
	if err := uninstallLaunchd(); err != nil {
		return err
	}
	// 安装时放置的二进制目录一并删除 —— uninstall 的职责是不留痕迹。
	// 正在运行的进程不受影响（macOS 下 unlink 已加载的二进制是合法的，
	// inode 保留到进程退出），但本命令通常就是那个进程在删自己：
	// 删除成功、退出后 launchd 不会再把它拉起来（plist 已先卸）。
	removedBinary := removeBinaryInstall()
	if *purge {
		stateDir := state.DefaultDir()
		if err := os.RemoveAll(stateDir); err != nil {
			return fmt.Errorf("purge state %s: %w", stateDir, err)
		}
		fmt.Printf("uninstalled: config.toml restored, launchd agent removed, binary deleted, state PURGED at %s\n", stateDir)
		fmt.Println("the Model Router app (if installed) is yours to delete — drag it out of ~/Applications")
		return nil
	}
	fmt.Println("uninstalled: config.toml restored, launchd agent removed" + removedBinary + " (state preserved)")
	fmt.Println("the Model Router app (if installed) is yours to delete — drag it out of ~/Applications")
	if _, err := os.Stat(state.DefaultDir()); err == nil {
		fmt.Printf("state preserved at %s (credentials, usage history); pass --purge to destroy it\n", state.DefaultDir())
	}
	return nil
}

// removeBinaryInstall 清掉安装的工件。二进制可以住在操作者的共享目录
//（如 ~/bin）里，那里还有人家自己的东西 —— 只删自己放下的三样：二进制
// 本体、旁边的 config/ 注册表拷贝、bin/control 启动器（bin/ 目录空了
// 才顺手删）。旧自包含布局（~/.local/share/codex-router-go）整个目录
// 都是路由器的，整体删除。返回用于状态行的人类可读片段。
func removeBinaryInstall() string {
	removed := false
	if exe, err := selfBinaryPath(); err == nil {
		dir := filepath.Dir(exe)
		artifacts := []string{
			exe,
			filepath.Join(dir, "config"),
			filepath.Join(dir, "bin", "control"),
		}
		for _, artifact := range artifacts {
			if _, err := os.Stat(artifact); err == nil {
				if err := os.RemoveAll(artifact); err == nil {
					removed = true
				}
			}
		}
		// bin/ 目录只有在空的时候才移除 —— 非空说明里面有别人的东西。
		os.Remove(filepath.Join(dir, "bin"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		legacy := filepath.Join(home, ".local", "share", "codex-router-go")
		if _, err := os.Stat(legacy); err == nil {
			if err := os.RemoveAll(legacy); err == nil {
				removed = true
			}
		}
	}
	if removed {
		return ", binary deleted"
	}
	return ""
}

// cmdDoctor：残血体检 —— 服务活、key 在、catalog 新鲜、config 集成、凭据可解析。
func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	stateDir := fs.String("state", state.DefaultDir(), "state directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := state.Open(*stateDir)
	if err != nil {
		return err
	}
	reg, err := registry.LoadDefault("")
	if err != nil {
		return err
	}
	resolver := cred.New(st)
	failures := 0

	check := func(name string, ok bool, detail string) {
		status := "OK"
		if !ok {
			status = "FAIL"
			failures++
		}
		line := fmt.Sprintf("%-28s %s", name+":", status)
		if detail != "" {
			line += "  " + detail
		}
		fmt.Println(line)
	}

	if _, err := st.CallerKey(); err != nil {
		check("caller key", false, err.Error())
	} else {
		check("caller key", true, "")
	}
	check("internal key", st.InternalKey() != "", "")

	merged := filepath.Join(st.Dir, "merged-models.json")
	_, statErr := os.Stat(merged)
	check("catalog present", statErr == nil, merged)

	installed, baseURL, _ := configfile.Status(codexConfigPath())
	check("codex config integration", installed, redactURL(baseURL))

	// 坏的 config.toml 会让凭证静默失效（解析失败按无凭证处理），
	// 体检必须把它指给操作者 —— 带行号的解析错误。
	if err := st.ConfigParseError(); err != nil {
		check("credentials config", false, err.Error())
	} else if _, err := os.Stat(st.ConfigPath()); err == nil {
		check("credentials config", true, st.ConfigPath())
	}

	for _, id := range st.EnabledProviders() {
		p := reg.Providers[id]
		if p == nil {
			continue
		}
		value, source := resolver.Resolve(p)
		label := fmt.Sprintf("credential %s", id)
		if value == "" {
			check(label, false, "missing (env/file/keychain)")
		} else {
			check(label, true, "source="+source)
		}
	}

	check("service reachable", probeHealth(*stateDir), "")

	if failures > 0 {
		return fmt.Errorf("doctor found %d failure(s)", failures)
	}
	return nil
}

func redactURL(value string) string {
	// 绝不打印完整 caller URL：把 secret 段换成 [REDACTED]。
	parts := strings.Split(value, "/")
	for i, part := range parts {
		if len(part) >= 32 && isSecretShaped(part) {
			parts[i] = "[REDACTED]"
		}
	}
	return strings.Join(parts, "/")
}

func isSecretShaped(value string) bool {
	for _, c := range value {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}
