package vision

import "sort"

// 本地视觉模型目录只承担“可下载候选”信息：下载体积、内存门槛与
// 展示说明。读图准确率档位来自历史人工策展值，不再随仓库维护。

// LocalVisionModel 描述一个策展的本地 Ollama 视觉模型。
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

// Measured 保留历史人工策展的实测信息，仅用于展示。
type Measured struct {
	Percent     int     `json:"percent"`
	TextPercent int     `json:"textPercent"`
	Seconds     float64 `json:"seconds"`
}

// LocalVisionCatalog 是策展的本地视觉模型目录。
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

// accuracyRank：展示排序权重（accurate → partial → untested → captions-only）。
var accuracyRank = map[string]int{"accurate": 0, "partial": 1, "untested": 2, "captions-only": 3}

// RankedLocalVision 按“可信读图优先、同档最小下载优先”排序。
func RankedLocalVision() []LocalVisionModel {
	ranked := make([]LocalVisionModel, len(LocalVisionCatalog))
	copy(ranked, LocalVisionCatalog)
	sort.SliceStable(ranked, func(i, j int) bool {
		if accuracyRank[ranked[i].Accuracy] != accuracyRank[ranked[j].Accuracy] {
			return accuracyRank[ranked[i].Accuracy] < accuracyRank[ranked[j].Accuracy]
		}
		return ranked[i].SizeGb < ranked[j].SizeGb
	})
	return ranked
}
