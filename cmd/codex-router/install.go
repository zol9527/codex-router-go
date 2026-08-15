package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/loyd/codex-router/internal/catalog"
	"github.com/loyd/codex-router/internal/configfile"
	"github.com/loyd/codex-router/internal/cred"
	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/state"
)

// cmdInstall：secret 生成 → 注册表自包含拷贝 → catalog 发布 →
// config.toml 集成 → launchd 注册。幂等：重复执行等于刷新。
//
// 自包含：install 把源 config/ 注册表拷进安装目录（二进制旁）并放置
// bin/control 启动器，之后 serve（plist 内嵌 --config）与 tray
//（ModelRouterSourceRoot 指安装目录）都不再依赖仓库 checkout。
func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	stateDir := fs.String("state", state.DefaultDir(), "state directory")
	configDir := fs.String("config", "", "source registry config directory to install from")
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

	// 源注册表：--config 显式给出 > 从 cwd 向上找（在仓库里跑）>
	// 已安装的拷贝（无仓库修复场景）。源优先于已装拷贝，重装才能
	// 带上注册表的变更。
	sourceConfig := sourceConfigDir(*configDir)
	installConfig := ""
	if exe, err := selfBinaryPath(); err == nil {
		installConfig = filepath.Join(filepath.Dir(exe), "config")
	}
	if *dryRun {
		fmt.Printf("would copy registry config: %s -> %s\n", sourceConfig, installConfig)
	} else {
		if installConfig == "" {
			return fmt.Errorf("cannot resolve install directory for self-contained config")
		}
		if err := copyConfigTree(sourceConfig, installConfig); err != nil {
			return fmt.Errorf("copy registry config: %w", err)
		}
		if err := writeControlLauncher(filepath.Dir(installConfig)); err != nil {
			return fmt.Errorf("write control launcher: %w", err)
		}
	}

	// 从拷贝加载注册表 —— 这一步同时证明拷贝是完整的。
	loadDir := sourceConfig
	if !*dryRun && installConfig != "" {
		loadDir = installConfig
	}
	reg, err := registry.Load(loadDir)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if reg.Providers[id] == nil {
			return fmt.Errorf("unknown provider %q (not in config/ registry)", id)
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
		fallback := catalog.Build(nil, reg, func(providerID string) bool { return true }, false)
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
		fmt.Printf("would install launchd agent: %s\n", launchdPlistPath())
		return nil
	}
	if err := configfile.Install(codexConfig, routerCfg); err != nil {
		return err
	}

	// launchd 注册。
	if err := installLaunchd(*port, st.Dir); err != nil {
		return fmt.Errorf("launchd install: %w", err)
	}

	fmt.Printf("installed: %d models published, config.toml integrated, service registered\n", count)
	fmt.Printf("state: %s\n", st.Dir)
	if installConfig != "" {
		fmt.Printf("self-contained: %s (registry config + bin/control copied)\n", filepath.Dir(installConfig))
	}
	return nil
}

// sourceConfigDir 解析安装的注册表来源：--config 显式给出 > 从 cwd
// 向上找（在仓库里跑 install 的正常路径）> 已安装拷贝（无仓库的
// 修复场景，重装等于刷新当前已装的注册表）。
func sourceConfigDir(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if dir, found := walkUpConfigDir(); found {
		return dir
	}
	return defaultConfigDir()
}

// walkUpConfigDir 从当前目录向上找 config/（源码树运行形态）。
func walkUpConfigDir() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "config")
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}

// copyConfigTree 递归拷贝注册表目录。先清空目标再拷 —— 注册表文件
// 会增删（provider 下线、模型移除），叠加拷贝会把已删的留在安装里。
func copyConfigTree(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil // 契约里只有目录与 json 文件
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o644)
	})
}

// writeControlLauncher 在安装目录放置 bin/control。tray 校验并 exec
// 的就是这个路径（ModelRouterSourceRoot 指向安装目录），内容与仓库
// 里的 bin/control 等价，但解析的是同目录的二进制。
func writeControlLauncher(installDir string) error {
	script := `#!/bin/sh
set -eu

# 自包含安装里的 control 入口。tray 的 ModelRouterSourceRoot 指向本目录。
exec "${CODEX_ROUTER_GO_BINARY:-$(dirname "$0")/../codex-router}" control "$@"
`
	dir := filepath.Join(installDir, "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "control")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		return err
	}
	return os.Chmod(path, 0o755)
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
		return nil
	}
	fmt.Println("uninstalled: config.toml restored, launchd agent removed" + removedBinary + " (state preserved)")
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
	reg, err := registry.Load(defaultConfigDir())
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
