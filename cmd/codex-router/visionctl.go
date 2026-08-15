package main

// vision-bridge 与 local-runtime 的 control 面板命令：
// 拉取（detached worker）、进度轮询、基准测量、本地运行时管理。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/state"
	"github.com/loyd/codex-router/internal/vision"
)

// cmdVisionPullWorker 是 detached 下载 worker 的隐藏入口
// （StartDetachedPull 重新 exec 本二进制进入这里）。
func cmdVisionPullWorker(args []string) error {
	fs := flag.NewFlagSet("__vision-pull-worker", flag.ContinueOnError)
	stateDir := fs.String("state", state.DefaultDir(), "state directory")
	baseURL := fs.String("base", vision.DefaultLocalVisionBaseURL, "ollama base url")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("a model tag is required")
	}
	// 下载前确保本地运行时在跑（headless 拉起自己的 Ollama）。
	if _, err := vision.EnsureHeadless(context.Background(), *stateDir, *baseURL); err != nil {
		// 拉不起也要把失败写进状态文件，轮询方看得到原因。
		vision.WriteDownload(*stateDir, vision.DownloadState{
			Version: 1, Tag: rest[0], StartedAt: time.Now().UnixMilli(),
			Status: "error", Detail: "failed", Error: err.Error(),
		})
		return err
	}
	return vision.RunDetachedPull(*stateDir, rest[0], *baseURL)
}

// controlVisionBridge 处理 control vision-bridge <action>。
// stateDir 由 cmdControl 解析传入（--state 的剥离在那里统一完成）。
// on/off/engine/effort/local 是 tray 设置页视觉卡的写路径；
// pull/pull-status/benchmark/catalog 管本地模型与测量。
// reg 供 engine 动作校验 pin 的 slug（registry 视觉模型 + native 目录）。
func controlVisionBridge(stateDir string, reg *registry.Registry, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("vision-bridge requires on|off|engine|effort|local|status|pull|pull-status|benchmark|catalog")
	}
	st, err := state.Open(stateDir)
	if err != nil {
		return err
	}
	action := args[0]
	switch action {
	case "on", "off":
		settings, _ := vision.ReadSettings(st.Dir)
		enabled := action == "on"
		settings.Enabled = &enabled
		if err := vision.WriteSettings(st.Dir, settings); err != nil {
			return err
		}
		return printVisionStatus(st, reg)
	case "engine":
		if len(args) < 2 {
			return fmt.Errorf("engine requires a slug, \"local\", or \"auto\"")
		}
		return controlVisionEngine(st, reg, args[1:])
	case "effort":
		if len(args) < 2 {
			return fmt.Errorf("effort requires a level or \"default\"")
		}
		settings, configured := vision.ReadSettings(st.Dir)
		materializeDefaultOn(&settings, configured)
		if args[1] == "default" {
			settings.Effort = ""
		} else if args[1] == "" {
			return fmt.Errorf("effort level must not be empty")
		} else {
			settings.Effort = args[1]
		}
		if err := vision.WriteSettings(st.Dir, settings); err != nil {
			return err
		}
		warnVisionWriteState(st, reg, settings, configured)
		return printVisionStatus(st, reg)
	case "local":
		if len(args) < 2 {
			return fmt.Errorf("local requires a model tag")
		}
		settings, configured := vision.ReadSettings(st.Dir)
		materializeDefaultOn(&settings, configured)
		settings.Engine = vision.LocalEngineSlug
		settings.LocalModel = args[1]
		if err := vision.WriteSettings(st.Dir, settings); err != nil {
			return err
		}
		warnVisionWriteState(st, reg, settings, configured)
		return printVisionStatus(st, reg)
	case "status":
		return printVisionStatus(st, reg)
	case "pull":
		if len(args) < 2 {
			return fmt.Errorf("pull requires a model tag")
		}
		self, err := os.Executable()
		if err != nil {
			return err
		}
		if err := vision.StartDetachedPull(self, st.Dir, args[1], vision.DefaultLocalVisionBaseURL); err != nil {
			return err
		}
		fmt.Printf("download started: %s (poll with 'vision-bridge pull-status')\n", args[1])
		return nil
	case "pull-status":
		download := vision.ReadDownload(st.Dir)
		if download == nil {
			fmt.Println(`{"status":"idle"}`)
			return nil
		}
		raw, _ := json.MarshalIndent(download, "", "  ")
		fmt.Println(string(raw))
		return nil
	case "catalog":
		raw, _ := json.MarshalIndent(vision.RankedLocalVision(), "", "  ")
		fmt.Println(string(raw))
		return nil
	default:
		return fmt.Errorf("unknown vision-bridge action %q", action)
	}
}

// controlVisionEngine 处理 `vision-bridge engine <slug|local|auto> [effort]`。
// tray 的引擎选择与档位一次下发（一条命令两值同步落盘）；
// 单独改档位走 `vision-bridge effort`。slug 必须命中已知引擎 ——
// pin 一个不存在的引擎会让 ResolveEngines 对每次贴图静默失败。
func controlVisionEngine(st *state.State, reg *registry.Registry, args []string) error {
	value := args[0]
	settings, configured := vision.ReadSettings(st.Dir)
	materializeDefaultOn(&settings, configured)
	switch value {
	case "auto":
		settings.Engine = ""
	case "local":
		settings.Engine = vision.LocalEngineSlug
	default:
		if !knownVisionEngineSlug(st, reg, value) {
			return fmt.Errorf("unknown vision engine %q; use a paid/native engine slug, \"local\", or \"auto\"", value)
		}
		settings.Engine = value
	}
	settings.Defaulted = false
	if len(args) >= 2 {
		if args[1] == "default" {
			settings.Effort = ""
		} else if args[1] == "" {
			return fmt.Errorf("effort level must not be empty")
		} else {
			settings.Effort = args[1]
		}
	}
	if err := vision.WriteSettings(st.Dir, settings); err != nil {
		return err
	}
	warnVisionWriteState(st, reg, settings, configured)
	return printVisionStatus(st, reg)
}

// materializeDefaultOn 在写第一个配置文件时把默认的 enabled=true 落成
// 显式值：门控语义里"文件存在但 enabled 缺失"按 off 处理，不物化的话
// engine/effort/local 这些与开关无关的动作会把默认开的桥静默关掉。
// 已配置过的文件不动 —— 存过 false 就是操作者的答案，永远照字面取用。
func materializeDefaultOn(settings *vision.Settings, configured bool) {
	if !configured && settings.Enabled == nil {
		enabled := true
		settings.Enabled = &enabled
	}
}

// warnVisionWriteState 把两类"写成功但桥不生效"的情形提示到 stderr
// （stdout 保持纯 JSON，tray 页脚只显示命令错误）：
//   - 文件解析失败后被重写（enabled 缺失 → 桥保持关）；
//   - pin 的引擎当前够不着（provider 未启用/无凭据）—— ResolveEngines
//     对显式 pin 找不到候选返回空，贴图将静默直通。
func warnVisionWriteState(st *state.State, reg *registry.Registry, settings vision.Settings, configured bool) {
	if configured && settings.Enabled == nil {
		fmt.Fprintln(os.Stderr, "note: previous settings file was unreadable and has been rewritten; the bridge stays off until 'vision-bridge on'")
	}
	if settings.Engine == "" || settings.Engine == vision.LocalEngineSlug {
		return
	}
	candidates, _, _ := visionEngineOptions(st, reg)
	for _, candidate := range candidates {
		if candidate.Slug == settings.Engine {
			return
		}
	}
	fmt.Fprintf(os.Stderr, "warning: pinned engine %q is not currently usable (provider disabled or credential missing); image reads will pass through until it resolves\n", settings.Engine)
}

// knownVisionEngineSlug 判定 slug 是否可作为引擎 pin：
// registry 里声明 image 的模型，或 merged 目录里的视觉原生条目。
func knownVisionEngineSlug(st *state.State, reg *registry.Registry, slug string) bool {
	for _, candidate := range visionEngineCandidates(st, reg) {
		if candidate.Slug == slug {
			return true
		}
	}
	return false
}

// visionEngineCandidates 汇总引擎候选 slug 的验证集。reg 可为 nil
// （无 registry 上下文时只验 native/local）。不查启用态与凭据 ——
// pin 是操作者意图，provider 稍后启用/配钥都不该让 pin 失效。
func visionEngineCandidates(st *state.State, reg *registry.Registry) []vision.Engine {
	candidates := []vision.Engine{}
	if reg != nil {
		for _, model := range reg.Models {
			if vision.SupportsImage(model.InputModalities) {
				candidates = append(candidates, vision.Engine{Slug: model.Slug, DisplayName: model.DisplayName})
			}
		}
	}
	exclude := map[string]bool{}
	if reg != nil {
		for _, model := range reg.Models {
			exclude[model.Slug] = true
		}
	}
	candidates = append(candidates, vision.NativeEnginesFromCatalogFile(
		filepath.Join(st.Dir, "merged-models.json"), exclude)...)
	return candidates
}

// printVisionStatus 输出与 control --json 同形状的视觉桥状态块，
// 写动作之后操作者（与 tray 的下一次刷新）立即看到生效值。
func printVisionStatus(st *state.State, reg *registry.Registry) error {
	raw, err := json.MarshalIndent(visionBridgeSnapshot(st, reg), "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(raw))
	return nil
}

// runBenchmark 对已安装的本地视觉模型测量文本读准率。
// 只测已在盘上的模型 —— 测量绝不能顺手触发多 GB 下载。
func runBenchmark(stateDir string, args []string) error {
	asJSON := false
	var only []string
	for _, arg := range args {
		if arg == "--json" {
			asJSON = true
			continue
		}
		only = append(only, arg)
	}
	baseURL := vision.DefaultLocalVisionBaseURL
	probe := vision.ProbeLocalServer(context.Background(), nil, baseURL)
	if !probe.Reachable {
		if _, err := vision.EnsureHeadless(context.Background(), stateDir, baseURL); err != nil {
			return fmt.Errorf("no local runtime at %s: %v", baseURL, err)
		}
		probe = vision.ProbeLocalServer(context.Background(), nil, baseURL)
	}
	installed := map[string]bool{}
	for _, model := range probe.Models {
		installed[model] = true
	}
	var candidates []string
	if len(only) > 0 {
		candidates = only
	} else {
		for _, model := range vision.RankedLocalVision() {
			candidates = append(candidates, model.Tag)
		}
	}
	var runnable []string
	for _, tag := range candidates {
		if installed[tag] || installed[tag+":latest"] {
			runnable = append(runnable, tag)
		}
	}
	if len(runnable) == 0 {
		return fmt.Errorf("none of the catalog models are installed; pull one first, e.g. `vision-bridge pull qwen2.5vl:3b`")
	}

	raw := vision.BenchmarkFixture()
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)

	var results []vision.BenchmarkResult
	for _, tag := range runnable {
		if !asJSON {
			fmt.Fprintf(os.Stderr, "benchmarking %s…\n", tag)
		}
		started := time.Now()
		result := vision.BenchmarkResult{Tag: tag}
		settings := vision.Settings{LocalModel: tag, LocalBaseURL: baseURL}
		reader := vision.NewReader(func(ctx context.Context, e vision.Engine, effort, question, image string) (string, error) {
			_ = e
			_ = effort
			body := vision.ChatDescribeRequest(settings.LocalModel, question, image)
			status, raw, err := vision.PostJSON(ctx, nil, vision.OllamaRootOf(baseURL)+"/chat/completions",
				map[string]string{"Accept": "application/json"}, body)
			if err != nil {
				return "", err
			}
			if status != 200 {
				return "", fmt.Errorf("HTTP %d", status)
			}
			return vision.ParseChatDescribeResponse(raw)
		})
		transcript, err := reader.Read(context.Background(), []vision.Engine{{Slug: tag, Local: true, ImageCapable: true}},
			vision.ImagePart{DataURL: dataURL})
		result.Seconds = float64(time.Since(started).Milliseconds()) / 1000.0
		if err != nil {
			result.OK = false
			result.Error = err.Error()
		} else {
			result.OK = true
			result.Transcript = transcript.Transcript
			score := vision.ScoreTranscript(transcript.Transcript)
			result.Percent = score.Percent
			result.TextPercent = score.TextPercent
			result.Tier = vision.AccuracyTier(score)
		}
		vision.SaveBenchmarkResult(stateDir, tag, result)
		results = append(results, result)
	}
	if asJSON {
		raw, _ := json.MarshalIndent(map[string]any{"results": results}, "", "  ")
		fmt.Println(string(raw))
		return nil
	}
	fmt.Printf("\n%-24s%-8s%-16stime\n", "model", "score", "tier")
	for _, result := range results {
		if !result.OK {
			fmt.Printf("%-24s%-8s%s\n", result.Tag, "error", result.Error)
			continue
		}
		fmt.Printf("%-24s%-8s%-16s%.1fs\n", result.Tag,
			fmt.Sprintf("%d%%", result.Percent), result.Tier, result.Seconds)
		score := vision.ScoreTranscript(result.Transcript)
		for _, group := range []string{"codes", "numbers", "dates", "shapes", "colors"} {
			entry := score.Groups[group]
			if entry.Found == entry.Total {
				continue
			}
			fmt.Printf("  %s: %d/%d — missed %s\n", group, entry.Found, entry.Total,
				strings.Join(entry.Missed, ", "))
		}
	}
	return nil
}

// controlLocalRuntime 处理 control local-runtime <action>。
func controlLocalRuntime(stateDir string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("local-runtime requires status|start|stop")
	}
	st, err := state.Open(stateDir)
	if err != nil {
		return err
	}
	action := args[0]
	switch action {
	case "status":
		runtimeState := vision.ReadRuntimeState(st.Dir)
		probe := vision.ProbeLocalServer(context.Background(), nil, vision.DefaultLocalVisionBaseURL)
		payload := map[string]any{
			"reachable": probe.Reachable,
			"models":    probe.Models,
			"installed": vision.OllamaCommand() != "",
		}
		if runtimeState != nil && runtimeState.Managed {
			payload["managed"] = vision.StateOwnsProcess(runtimeState)
			payload["pid"] = runtimeState.PID
		}
		raw, _ := json.MarshalIndent(payload, "", "  ")
		fmt.Println(string(raw))
		return nil
	case "start":
		result, err := vision.EnsureHeadless(context.Background(), st.Dir, vision.DefaultLocalVisionBaseURL)
		if err != nil {
			return err
		}
		raw, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(raw))
		return nil
	case "stop":
		result := vision.StopManaged(st.Dir)
		raw, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(raw))
		return nil
	default:
		return fmt.Errorf("unknown local-runtime action %q", action)
	}
}
