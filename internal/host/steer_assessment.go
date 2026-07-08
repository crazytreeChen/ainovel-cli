package host

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/voocel/ainovel-cli/internal/domain"
	storepkg "github.com/voocel/ainovel-cli/internal/store"
)

// SteerAssessment 是干预偏离评估的结构化结果。
type SteerAssessment struct {
	// DeviationLevel: "none" / "plan_only" / "rewrite_needed"
	DeviationLevel string

	// AffectedChapters 受影响的章节列表（全局章节号）。
	AffectedChapters []int

	// WrittenChapters 已写完的受影响章节。
	WrittenChapters []int

	// UnwrittenChapters 未写的受影响章节。
	UnwrittenChapters []int

	// Summary 评估摘要（供 coordinator 阅读）。
	Summary string

	// Instruction 明确的指令类型： "replan" / "replan_and_rewrite"
	Instruction string
}

// assessSteerDeviation 评估干预文本与当前大纲/进度的偏离。
// 返回结构化评估结果，用于构建 coordinator Inject 消息。
func assessSteerDeviation(text string, progress *domain.Progress, outlineStore *storepkg.OutlineStore, layered bool) *SteerAssessment {
	a := &SteerAssessment{
		DeviationLevel:   "none",
		AffectedChapters: nil,
		WrittenChapters:  nil,
		UnwrittenChapters: nil,
		Instruction:      "",
	}

	// 1. 收集已完成章节
	completed := progress.CompletedChapters
	if completed == nil {
		completed = []int{}
	}

	// 2. 解析干预中提到的章节号
	mentioned := parseMentionedChapters(text)

	// 3. 读取大纲信息
	var outlineChapters []domain.OutlineEntry
	if layered {
		volumes, err := outlineStore.LoadLayeredOutline()
		if err == nil && len(volumes) > 0 {
			outlineChapters = domain.FlattenOutline(volumes)
		}
	}
	if len(outlineChapters) == 0 {
		entries, err := outlineStore.LoadOutline()
		if err == nil {
			outlineChapters = entries
		}
	}

	// 构建大纲摘要上下文
	outlineSummary := buildOutlineSummary(outlineChapters)

	// 4. 判断偏离
	totalCompleted := len(completed)

	// 如果提到了指定章节号
	if len(mentioned) > 0 {
		for _, ch := range mentioned {
			a.AffectedChapters = append(a.AffectedChapters, ch)
			if contains(completed, ch) {
				a.WrittenChapters = append(a.WrittenChapters, ch)
			} else if ch <= totalCompleted {
				a.WrittenChapters = append(a.WrittenChapters, ch)
			} else {
				a.UnwrittenChapters = append(a.UnwrittenChapters, ch)
			}
		}
	} else {
		// 未指定章节号：判断为全局方向变更
		// 如果有已完成的章节 → 可能需要重写
		if totalCompleted > 0 {
			a.WrittenChapters = completed
			a.UnwrittenChapters = nil
			// 所有未写的章节
			for ch := totalCompleted + 1; ch <= progress.TotalChapters; ch++ {
				a.UnwrittenChapters = append(a.UnwrittenChapters, ch)
			}
		}
	}

	// 5. 确定偏离级别和指令
	if len(a.WrittenChapters) > 0 {
		a.DeviationLevel = "rewrite_needed"
		a.Instruction = "replan_and_rewrite"
	} else if len(a.UnwrittenChapters) > 0 || totalCompleted == 0 {
		a.DeviationLevel = "plan_only"
		a.Instruction = "replan"
	} else {
		a.DeviationLevel = "none"
		a.Instruction = "continue"
	}

	// 6. 构建摘要
	a.Summary = buildAssessmentSummary(a, text, progress, outlineSummary)

	slog.Debug("steer assessment",
		"module", "host.steer",
		"level", a.DeviationLevel,
		"instruction", a.Instruction,
		"written", a.WrittenChapters,
		"unwritten", a.UnwrittenChapters,
	)
	return a
}

// buildSteerMessage 构建增强的干预消息，包含偏离评估上下文。
func buildSteerMessage(text string, assessment *SteerAssessment) string {
	var b strings.Builder
	b.WriteString("[用户干预] ")
	b.WriteString(text)
	b.WriteString("\n\n")

	if assessment.DeviationLevel == "none" {
		b.WriteString("[偏离评估] 未检测到与现有大纲的明显偏离，按续写类处理。")
		return b.String()
	}

	b.WriteString("[偏离评估]\n")
	b.WriteString(assessment.Summary)
	b.WriteString("\n\n")

	switch assessment.Instruction {
	case "replan":
		b.WriteString("[指令] 调 architect_long 根据用户干预重新规划大纲（save_foundation / update_compass / expand_arc / append_volume）。")
		b.WriteString(" 所有受影响章节尚未写作，仅需更新大纲即可。")
		b.WriteString(" 返回后等 Host 指令继续。")

	case "replan_and_rewrite":
		b.WriteString("[指令] 两步执行：\n")
		b.WriteString("1. 先调 architect_long 重新规划大纲——按用户干预调整后续方向。\n")
		b.WriteString("2. 再调 editor——把以下已写入的受影响章节通过 save_review(verdict=rewrite, affected_chapters=[")
		b.WriteString(formatChapters(assessment.WrittenChapters))
		b.WriteString("]) 入队重写。\n")
		b.WriteString(" 受影响未写章节由新大纲覆盖，不需额外处理。\n")
		b.WriteString(" 入队后等 Host 指令——Host 会自动派 writer 逐章重写。")
	}

	return b.String()
}

// 解析干预文本中提到的章节号，如"第3章"、"第4-7章"
func parseMentionedChapters(text string) []int {
	var chapters []int
	runes := []rune(text)
	n := len(runes)

	for i := 0; i < n; i++ {
		// 匹配 "第" 开头
		if runes[i] == '第' {
			start := i + 1
			// 收集数字
			var numStr []rune
			j := start
			for j < n && runes[j] >= '0' && runes[j] <= '9' {
				numStr = append(numStr, runes[j])
				j++
			}
			if len(numStr) == 0 {
				continue
			}
			// 跳过空白
			for j < n && runes[j] == ' ' {
				j++
			}
			// 单章：第N章
			if j < n && (runes[j] == '章' || runes[j] == '节') {
				if ch, err := strconv.Atoi(string(numStr)); err == nil && ch > 0 {
					chapters = append(chapters, ch)
				}
				i = j
				continue
			}
			// 范围：第N-M章
			if j < n && runes[j] == '-' {
				j++
				for j < n && runes[j] == ' ' {
					j++
				}
				var endStr []rune
				for j < n && runes[j] >= '0' && runes[j] <= '9' {
					endStr = append(endStr, runes[j])
					j++
				}
				for j < n && runes[j] == ' ' {
					j++
				}
				if j < n && (runes[j] == '章' || runes[j] == '节') {
					startCh, err1 := strconv.Atoi(string(numStr))
					endCh, err2 := strconv.Atoi(string(endStr))
					if err1 == nil && err2 == nil && startCh > 0 && endCh > startCh {
						for ch := startCh; ch <= endCh; ch++ {
							chapters = append(chapters, ch)
						}
					}
				}
				i = j
				continue
			}
		}
	}

	return chapters
}

func buildOutlineSummary(entries []domain.OutlineEntry) string {
	if len(entries) == 0 {
		return "（无大纲信息）"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("当前大纲共 %d 章：\n", len(entries)))
	for _, e := range entries {
		b.WriteString(fmt.Sprintf("  第%d章 %s", e.Chapter, e.Title))
		if e.CoreEvent != "" {
			b.WriteString(": " + e.CoreEvent)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func buildAssessmentSummary(a *SteerAssessment, text string, progress *domain.Progress, outlineSummary string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("已完成 %d 章，当前进行到第 %d 章，共规划 %d 章。\n",
		len(progress.CompletedChapters),
		progress.InProgressChapter,
		progress.TotalChapters,
	))
	b.WriteString(fmt.Sprintf("偏离级别: %s\n", a.DeviationLevel))
	if len(a.WrittenChapters) > 0 {
		b.WriteString(fmt.Sprintf("已写的受影响章节: %v\n", a.WrittenChapters))
	}
	if len(a.UnwrittenChapters) > 0 {
		b.WriteString(fmt.Sprintf("未写的受影响章节: %v\n", a.UnwrittenChapters))
	}
	b.WriteString("指令: ")
	switch a.Instruction {
	case "replan":
		b.WriteString("仅重新规划（所有受影响章节尚未写作）")
	case "replan_and_rewrite":
		b.WriteString("先重新规划，再重写已写的受影响章节")
	default:
		b.WriteString("继续当前流程")
	}
	b.WriteString("\n\n当前大纲:\n")
	b.WriteString(outlineSummary)
	return b.String()
}

func contains(slice []int, val int) bool {
	for _, s := range slice {
		if s == val {
			return true
		}
	}
	return false
}

func formatChapters(chapters []int) string {
	if len(chapters) == 0 {
		return ""
	}
	parts := make([]string, len(chapters))
	for i, ch := range chapters {
		parts[i] = strconv.Itoa(ch)
	}
	return strings.Join(parts, ",")
}
