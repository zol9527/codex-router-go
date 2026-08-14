package vision

// 视觉模型基准，移植自 vision-benchmark.mjs。
// picker 给模型标准确性档位，档位必须是量出来的而不是猜的：
// 每个候选读同一张内容精确已知的 checked-in 图，得分是基准事实
// 有多少被逐字读回。两个最有名的本地视觉模型在此测得很差——
// 这正是这个测量必须存在的原因。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// GroundTruth 是一张已知内容图的精确串，按组分类
// （散文读得好但数字编造的模型要能被看见是这种模型）。
var GroundTruth = map[string][]string{
	"codes":   {"INV-7734-QX", "RC-2291-BB77", "Widget A-12", "Cable K-9"},
	"numbers": {"3,417.62", "1,155.00", "162.60", "2,100.02", "82.50", "27.10"},
	"dates":   {"2026-03-14", "2026-04-13"},
	"shapes":  {"pentagon", "circle"},
	"colors":  {"purple", "green"},
}

// BenchmarkFixturePath 向上查找仓库里的基准图。
func BenchmarkFixturePath() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, "test", "fixtures", "vision-benchmark.png")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// normalize：货币符号与千分位在模型间漂移（"$1,155.00" vs "1155.00"），
// 归一后比较而不是惩罚格式。
func normalizeText(text string) string {
	lower := strings.ToLower(text)
	var out strings.Builder
	for _, r := range lower {
		switch r {
		case ' ', '\t', '\n', '\r', '$', ',':
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

// GroupScore 是一组的命中明细。
type GroupScore struct {
	Found  int      `json:"found"`
	Total  int      `json:"total"`
	Missed []string `json:"missed"`
}

// Score 是一次转录的完整得分。
type Score struct {
	Percent     int                   `json:"percent"`
	TextPercent int                   `json:"textPercent"`
	Found       int                   `json:"found"`
	Total       int                   `json:"total"`
	Groups      map[string]GroupScore `json:"groups"`
}

// ScoreTranscript 对转录打分。
func ScoreTranscript(transcript string) Score {
	haystack := normalizeText(transcript)
	groups := map[string]GroupScore{}
	found, total := 0, 0
	// 组序稳定输出。
	for _, group := range []string{"codes", "numbers", "dates", "shapes", "colors"} {
		expected := GroundTruth[group]
		entry := GroupScore{Total: len(expected)}
		for _, value := range expected {
			if strings.Contains(haystack, normalizeText(value)) {
				entry.Found++
			} else {
				entry.Missed = append(entry.Missed, value)
			}
		}
		groups[group] = entry
		found += entry.Found
		total += entry.Total
	}
	percent := 0
	if total > 0 {
		percent = found * 100 / total
	}
	// 文本准确率是桥真正依赖的：一个描述得出形状但编造发票号的
	// 模型比无用更糟——下游会把编造当事实引用。
	textFound := groups["codes"].Found + groups["numbers"].Found + groups["dates"].Found
	textTotal := groups["codes"].Total + groups["numbers"].Total + groups["dates"].Total
	textPercent := 0
	if textTotal > 0 {
		textPercent = textFound * 100 / textTotal
	}
	return Score{
		Percent: percent, TextPercent: textPercent,
		Found: found, Total: total, Groups: groups,
	}
}

// AccuracyTier 按文本准确率分档。
func AccuracyTier(score Score) string {
	switch {
	case score.TextPercent >= 80:
		return "accurate"
	case score.TextPercent >= 40:
		return "partial"
	default:
		return "captions-only"
	}
}

// ---- 结果持久化 ----

// BenchmarkResult 是一个模型的完整测量记录。
type BenchmarkResult struct {
	Tag         string  `json:"tag"`
	OK          bool    `json:"ok"`
	Seconds     float64 `json:"seconds"`
	Percent     int     `json:"percent,omitempty"`
	TextPercent int     `json:"textPercent,omitempty"`
	Tier        string  `json:"tier,omitempty"`
	Transcript  string  `json:"transcript,omitempty"`
	Error       string  `json:"error,omitempty"`
}

func benchmarkPath(stateDir string) string {
	return filepath.Join(stateDir, "vision-benchmarks.json")
}

// ReadBenchmarkResults 读取全部已测结果（跨重启保留挣来的档位）。
func ReadBenchmarkResults(stateDir string) map[string]BenchmarkResult {
	raw, err := os.ReadFile(benchmarkPath(stateDir))
	if err != nil {
		return map[string]BenchmarkResult{}
	}
	var parsed struct {
		Version int                        `json:"version"`
		Results map[string]BenchmarkResult `json:"results"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.Version != 1 {
		return map[string]BenchmarkResult{}
	}
	return parsed.Results
}

// SaveBenchmarkResult 记录一次测量。
func SaveBenchmarkResult(stateDir, tag string, result BenchmarkResult) error {
	results := ReadBenchmarkResults(stateDir)
	results[tag] = result
	raw, err := json.Marshal(map[string]any{"version": 1, "results": results})
	if err != nil {
		return err
	}
	return os.WriteFile(benchmarkPath(stateDir), append(raw, '\n'), 0o600)
}

// LocalVisionCatalog 是策展的本地视觉模型目录。accuracy 档位必须
// 有 measured 数据支撑 —— llava 得 0 分、moondream 文本 0 分，而它们
// 听起来都完全可信；按档位排序让 picker 永不把"自信但读错"的模型
// 放在最上面。
type LocalVisionModel struct {
	Tag         string    `json:"tag"`
	Label       string    `json:"label"`
	SizeGb      float64   `json:"sizeGb"`
	MinRamGib   int       `json:"minRamGib"`
	Recommended bool      `json:"recommended,omitempty"`
	Accuracy    string    `json:"accuracy"`
	Measured    *Measured `json:"measured,omitempty"`
	Note        string    `json:"note"`
}

// Measured 是实测记录（没有它档位只能是 untested）。
type Measured struct {
	Percent     int     `json:"percent"`
	TextPercent int     `json:"textPercent"`
	Seconds     float64 `json:"seconds"`
}

// LocalVisionCatalog 列表（与 Node 版一致）。
var LocalVisionCatalog = []LocalVisionModel{
	{Tag: "qwen2.5vl:3b", Label: "Qwen2.5-VL 3B", SizeGb: 3.2, MinRamGib: 8,
		Recommended: true, Accuracy: "accurate",
		Measured: &Measured{Percent: 75, TextPercent: 100, Seconds: 23},
		Note:     "Reads codes, numbers, and dates exactly. The default choice."},
	{Tag: "qwen2.5vl:7b", Label: "Qwen2.5-VL 7B", SizeGb: 6.0, MinRamGib: 16,
		Accuracy: "untested", Note: "Larger sibling of the 3B. Not benchmarked here yet."},
	{Tag: "llama3.2-vision:11b", Label: "Llama 3.2 Vision 11B", SizeGb: 7.9, MinRamGib: 16,
		Accuracy: "untested", Note: "Strongest reasoning of the set. Not benchmarked here yet."},
	{Tag: "moondream", Label: "Moondream", SizeGb: 1.7, MinRamGib: 4,
		Accuracy: "captions-only",
		Measured: &Measured{Percent: 19, TextPercent: 0, Seconds: 4},
		Note:     "Tiny and quick, but transcribed none of the test text."},
	{Tag: "llava", Label: "LLaVA 7B", SizeGb: 4.7, MinRamGib: 8,
		Accuracy: "captions-only",
		Measured: &Measured{Percent: 0, TextPercent: 0, Seconds: 37},
		Note:     "Scored zero on the benchmark and is the slowest. Avoid for text."},
}

// accuracyRank：档位排序权重（proven → unmeasured → captions-only）。
var accuracyRank = map[string]int{"accurate": 0, "partial": 1, "untested": 2, "captions-only": 3}

// RankedLocalVision 按"可信读图优先、同档最小下载优先"排序。
func RankedLocalVision() []LocalVisionModel {
	ranked := make([]LocalVisionModel, len(LocalVisionCatalog))
	copy(ranked, LocalVisionCatalog)
	for i := 1; i < len(ranked); i++ {
		for j := i; j > 0; j-- {
			a, b := accuracyRank[ranked[j].Accuracy], accuracyRank[ranked[j-1].Accuracy]
			if a < b || (a == b && ranked[j].SizeGb < ranked[j-1].SizeGb) {
				ranked[j], ranked[j-1] = ranked[j-1], ranked[j]
			} else {
				break
			}
		}
	}
	return ranked
}
