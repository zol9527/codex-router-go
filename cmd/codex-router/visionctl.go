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
	"strings"
	"time"

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
func controlVisionBridge(stateDir string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("vision-bridge requires pull|pull-status|benchmark|engine|catalog")
	}
	st, err := state.Open(stateDir)
	if err != nil {
		return err
	}
	action := args[0]
	switch action {
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
	case "benchmark":
		return runBenchmark(st.Dir, args[1:])
	case "catalog":
		raw, _ := json.MarshalIndent(vision.RankedLocalVision(), "", "  ")
		fmt.Println(string(raw))
		return nil
	default:
		return fmt.Errorf("unknown vision-bridge action %q", action)
	}
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

	fixture := vision.BenchmarkFixturePath()
	if fixture == "" {
		return fmt.Errorf("benchmark fixture not found (test/fixtures/vision-benchmark.png)")
	}
	raw, err := os.ReadFile(fixture)
	if err != nil {
		return err
	}
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)

	var results []vision.BenchmarkResult
	for _, tag := range runnable {
		if !asJSON {
			fmt.Fprintf(os.Stderr, "benchmarking %s…\n", tag)
		}
		started := time.Now()
		result := vision.BenchmarkResult{Tag: tag}
		settings := vision.Settings{LocalModel: tag, LocalBaseURL: baseURL}
		engine := vision.Engine{Slug: tag, DisplayName: tag, Local: true, ImageCapable: true}
		reader := vision.NewReader(func(ctx context.Context, e vision.Engine, question, image string) (string, error) {
			_ = e
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
		_ = engine
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
