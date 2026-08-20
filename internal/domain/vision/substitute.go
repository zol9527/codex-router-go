package vision

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// 输入扫描与替换：识别 Responses input 里的图片 part（user message 的
// input_image 与 function_call_output 的输出图片），转录后替换成
// 带证据头部的文本 part。规格：两处都要覆盖（模型拿到转录后仍会对
// 文件路径调 view_image，工具结果里是同一张图的字节）。

// ImagePart 是从 input 里提取的一个图片及其定位。
type ImagePart struct {
	// DataURL 是图片字节的 data: URL（input_image.url 形态）。
	DataURL string
	// FilePath 是 Codex 标注的文件路径（<image path="..."> 包装或工具调用参数）。
	FilePath string
	// Question 是与这张图相关的操作者原话（最新消息优先）。
	Question string
}

// InputHasImage 报告 input 是否携带图片。
func InputHasImage(input []any) bool {
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		content, ok := item["content"].([]any)
		if !ok {
			continue
		}
		for _, partRaw := range content {
			part, ok := partRaw.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := part["type"].(string); t == "input_image" {
				return true
			}
			// 工具结果输出的图片藏在 output 字符串的 data: URL 里。
			if t, _ := item["type"].(string); t == "function_call_output" {
				if output, ok := item["output"].(string); ok && containsDataURL(output) {
					return true
				}
			}
		}
	}
	return false
}

func containsDataURL(text string) bool {
	return strings.Contains(text, "data:image/")
}

// messageQuestion 提取一条消息里操作者的原话（剥掉 Codex 的
// <image …> 包装与 # Files 前言 —— 那些是簿记不是用户的话）。
func messageQuestion(item map[string]any) string {
	parts, ok := item["content"].([]any)
	if !ok {
		if text, ok := item["content"].(string); ok {
			return cleanQuestion(text)
		}
		return ""
	}
	var texts []string
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		t, _ := part["type"].(string)
		if t == "input_text" || t == "text" {
			if text, ok := part["text"].(string); ok && strings.TrimSpace(text) != "" {
				texts = append(texts, text)
			}
		}
	}
	return cleanQuestion(strings.Join(texts, "\n"))
}

// cleanQuestion 剥簿记标记并截断到预算。
func cleanQuestion(text string) string {
	lines := strings.Split(text, "\n")
	var kept []string
	inFilesPreamble := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# Files mentioned") {
			inFilesPreamble = true
		} else if trimmed != "" && !strings.HasPrefix(trimmed, "- ") {
			inFilesPreamble = false
		}
		switch {
		case strings.HasPrefix(trimmed, "<image"):
			continue // Codex 的图片包装标记
		case strings.HasPrefix(trimmed, "# Files mentioned"):
			continue // 文件前言簿记
		case inFilesPreamble && strings.HasPrefix(trimmed, "- "):
			continue // 前言的文件列表项
		case strings.HasPrefix(trimmed, "<environment_context>") || strings.HasPrefix(trimmed, "<recommended_plugins>"):
			return strings.Join(kept, "\n") // 上下文块开始，后面都不是用户的话
		}
		kept = append(kept, line)
	}
	question := strings.TrimSpace(strings.Join(kept, "\n"))
	if question == "" {
		return ""
	}
	if len(question) > QuestionMaxChars {
		question = question[:QuestionMaxChars]
	}
	return question
}

// LatestQuestion 从 input 尾部往前找操作者最新的提问。
func LatestQuestion(input []any) string {
	for i := len(input) - 1; i >= 0; i-- {
		item, ok := input[i].(map[string]any)
		if !ok || item["role"] != "user" {
			continue
		}
		if asked := messageQuestion(item); asked != "" {
			return asked
		}
	}
	return ""
}

// FilePathOf 从消息文本里提取 Codex 的 <image path="..."> 标注。
func FilePathOf(item map[string]any) string {
	parts, _ := item["content"].([]any)
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		text, _ := part["text"].(string)
		if path := extractImagePathTag(text); path != "" {
			return path
		}
	}
	return ""
}

func extractImagePathTag(text string) string {
	start := strings.Index(text, `<image`)
	if start == -1 {
		return ""
	}
	segment := text[start:]
	end := strings.Index(segment, `>`)
	if end == -1 {
		return ""
	}
	tag := segment[:end]
	q := strings.Index(tag, `path="`)
	if q == -1 {
		return ""
	}
	rest := tag[q+len(`path="`):]
	closing := strings.Index(rest, `"`)
	if closing == -1 {
		return ""
	}
	return rest[:closing]
}

// CollectImages 提取 input 里的全部图片（按出现序）。
func CollectImages(input []any) []ImagePart {
	question := LatestQuestion(input)
	var images []ImagePart
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		path := FilePathOf(item)
		content, _ := item["content"].([]any)
		for _, partRaw := range content {
			part, ok := partRaw.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := part["type"].(string); t == "input_image" {
				url, _ := part["image_url"].(string)
				if url == "" {
					if nested, ok := part["image_url"].(map[string]any); ok {
						url, _ = nested["url"].(string)
					}
				}
				if url != "" {
					images = append(images, ImagePart{
						DataURL: url, FilePath: path, Question: question,
					})
				}
			}
		}
		// 工具结果里的 data: URL（view_image 的输出）。
		if t, _ := item["type"].(string); t == "function_call_output" {
			if output, ok := item["output"].(string); ok {
				if url := firstDataURL(output); url != "" {
					images = append(images, ImagePart{
						DataURL: url, FilePath: path, Question: question,
					})
				}
			}
		}
	}
	return images
}

func firstDataURL(text string) string {
	idx := strings.Index(text, "data:image/")
	if idx == -1 {
		return ""
	}
	rest := text[idx:]
	if end := strings.IndexAny(rest, "\"' "); end > 0 {
		return rest[:end]
	}
	return rest
}

// Evidence 是一张图的读图结果（注入的单元）。
type Evidence struct {
	Engine     string `json:"engine"`
	Question   string `json:"question,omitempty"`
	Transcript string `json:"transcript"`
	// Incomplete 标记读图被尺寸上限截断（值得再看一眼）。
	Incomplete bool `json:"incomplete,omitempty"`
}

// RenderEvidence 把证据渲染成注入回合的文本：
// 头部（引擎、文件、问题、截断标记）+ 完整转录，预算内截断。
func RenderEvidence(evidence Evidence, filePath string) string {
	var header strings.Builder
	header.WriteString("[image read by ")
	header.WriteString(evidence.Engine)
	if filePath != "" {
		header.WriteString("; file: ")
		header.WriteString(filePath)
	}
	header.WriteString("]\n")
	// 抑制"再核实原图"的冲动：下游模型看到 file: 路径（Codex 的
	// Files mentioned 提示也会给）时，会倾向发起工具轮去打开原图 ——
	// 思考型模型一轮 60-90s，纯浪费。声明转录即全部视觉信息。
	header.WriteString("[you cannot see images; this transcript is the complete visual information for the image. Do not attempt to open, view, or re-read the original file]\n")
	if evidence.Question != "" {
		header.WriteString("[read for question: ")
		header.WriteString(truncateRunes(evidence.Question, 160))
		header.WriteString("]\n")
	}
	if evidence.Incomplete {
		header.WriteString("[read incomplete: image exceeded the size cap — a second look may read more]\n")
	}
	header.WriteString("\n")
	body := evidence.Transcript
	if len(body)+header.Len() > EvidenceMaxChars {
		remaining := EvidenceMaxChars - header.Len()
		if remaining < 0 {
			remaining = 0
		}
		body = truncateRunes(body, remaining-len(TruncationNotice)) + "\n" + TruncationNotice
	}
	return header.String() + body
}

// FailureText 是读图失败的降级文本 —— 说的失败而不是丢图：
// 模型必须知道这里本该有一张它读不到的图。
func FailureText(engine string, err error) string {
	detail := ""
	if err != nil {
		detail = ": " + err.Error()
	}
	return fmt.Sprintf("[image could not be read by %s%s — the router states this failure rather than guessing at the image]", engine, detail)
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

// Substitute 用证据替换 input 里的图片 part（data URL 匹配）。
// 同一回合同一张图多次出现（贴图 + view_image 结果）只在第一处
// 打印完整记录，其余指向它 —— 指向键按图片字节，绝不按转录文本。
func Substitute(input []any, evidenceByImage map[string]Evidence, failureByImage map[string]string) []any {
	out := make([]any, len(input))
	printed := map[string]bool{}
	for i, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			out[i] = raw
			continue
		}
		content, hasContent := item["content"].([]any)
		if hasContent {
			rewritten := make([]any, len(content))
			changed := false
			for j, partRaw := range content {
				part, ok := partRaw.(map[string]any)
				if !ok || part["type"] != "input_image" {
					rewritten[j] = partRaw
					continue
				}
				url, _ := part["image_url"].(string)
				if url == "" {
					if nested, ok := part["image_url"].(map[string]any); ok {
						url, _ = nested["url"].(string)
					}
				}
				key := ImageKey(url)
				text := ""
				if failure, failed := failureByImage[key]; failed {
					text = failure
				} else if evidence, found := evidenceByImage[key]; found {
					if printed[key] {
						text = "[same image as earlier in this turn — evidence above]"
					} else {
						printed[key] = true
						text = RenderEvidence(evidence, FilePathOf(item))
					}
				}
				if text == "" {
					rewritten[j] = partRaw // 没有对应证据（未提取到），原样
					continue
				}
				rewritten[j] = map[string]any{"type": "input_text", "text": text}
				changed = true
			}
			if changed {
				next := map[string]any{}
				for k, v := range item {
					next[k] = v
				}
				next["content"] = rewritten
				out[i] = next
				continue
			}
		}
		// 工具结果输出里的 data: URL 替换。
		if t, _ := item["type"].(string); t == "function_call_output" {
			if output, ok := item["output"].(string); ok && containsDataURL(output) {
				replaced := replaceDataURL(output, evidenceByImage, failureByImage, printed)
				if replaced != output {
					next := map[string]any{}
					for k, v := range item {
						next[k] = v
					}
					next["output"] = replaced
					out[i] = next
					continue
				}
			}
		}
		out[i] = raw
	}
	return out
}

// ImageKey 按图片字节定位证据。
func ImageKey(dataURL string) string {
	digest := sha256.Sum256([]byte(dataURL))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// replaceDataURL 把工具输出字符串里的 data: URL 换成证据文本。
func replaceDataURL(output string, evidence map[string]Evidence, failures map[string]string, printed map[string]bool) string {
	url := firstDataURL(output)
	if url == "" {
		return output
	}
	key := ImageKey(url)
	text := ""
	if failure, failed := failures[key]; failed {
		text = failure
	} else if evidenceEntry, found := evidence[key]; found {
		if printed[key] {
			text = "[same image as earlier in this turn — evidence above]"
		} else {
			printed[key] = true
			text = RenderEvidence(evidenceEntry, "")
		}
	}
	if text == "" {
		return output
	}
	return strings.Replace(output, url, text, 1)
}

var _ = json.Marshal
