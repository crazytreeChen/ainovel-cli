package host

import (
	"strconv"
	"strings"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
)

// Event 是 TUI 消费的结构化事件。
//
// 对于 TOOL / DISPATCH 两类调用事件，同一次调用的开始与结束共用一个 ID：
// 开始时先发 FinishedAt 为零值的事件（TUI 渲染为"进行中"样式）；
// 结束时再发一条同 ID 的事件，填入 FinishedAt + Duration（+ Failed），
// TUI 按 ID 定位原行原地更新，避免"开始一行、完成又一行"的冗余。
//
// SYSTEM / ERROR / CONTEXT 等非调用类事件 ID 为空，每条独立追加。
type Event struct {
	ID         string    // 同一次调用的开始/结束共用；非调用事件为空
	Time       time.Time // 首次发出时间（开始时刻）
	FinishedAt time.Time // 零值 = 进行中；非零 = 已完成
	Failed     bool      // 已完成但失败（仅完成态有意义）
	Category   string    // DISPATCH / TOOL / SYSTEM / REVIEW / CHECK / ERROR / CONTEXT
	Agent      string    // 产生事件的 agent
	Summary    string
	Detail     string        // 完整文案，写入日志不截断供排查；为空回退 Summary。UI 只读 Summary
	Kind       string        // 错误分类（如 stream_idle），随日志输出供过滤/告警；为空不输出
	Level      string        // info / warn / error / success
	Depth      int           // 0 = coordinator 层, 1 = sub-agent 层
	Duration   time.Duration // 完成时的执行耗时
}

// Running 返回事件是否处于进行中。
// 仅调用类事件（有 ID 的 TOOL / DISPATCH）可能进行中；其它类型总是返回 false。
func (e Event) Running() bool {
	return e.ID != "" && e.FinishedAt.IsZero()
}

// UISnapshot 是 TUI 渲染所需的聚合状态快照。
type UISnapshot struct {
	Provider           string
	NovelName          string
	ModelName          string
	ModelContextWindow int // 当前默认模型的上下文窗口（随 /model 切换实时解析）
	ThinkingLevel      string
	Style              string
	RuntimeState       string // idle / running / pausing / paused / completed
	StatusLabel        string
	Phase              string
	Flow               string
	CurrentChapter     int
	TotalChapters      int
	CompletedCount     int
	TotalWordCount     int
	InProgressChapter  int
	PendingRewrites    []int
	RewriteReason      string
	PendingSteer       string
	RecoveryLabel      string
	IsRunning          bool
	Agents             []AgentSnapshot

	// 上下文
	ContextTokens         int
	ContextWindow         int
	ContextPercent        float64
	ContextScope          string
	ContextStrategy       string
	ContextActiveMessages int
	ContextSummaryCount   int
	ContextCompactedCount int
	ContextKeptCount      int

	// 累计用量（整个会话，跨所有 agent 与模型切换）
	TotalInputTokens      int
	TotalOutputTokens     int
	TotalCacheReadTokens  int
	TotalCacheWriteTokens int
	TotalCostUSD          float64
	TotalSavedUSD         float64 // 因 CacheRead 命中省下的美元（相对全按非缓存输入价计费）
	BudgetLimitUSD        float64 // 预算上限（config budget.book_usd）；0 = 未启用

	// 缓存诊断
	OverallCacheCapable    bool // 至少一个 role 跑过支持 prompt cache 的模型（区分"未启用"和"0% 命中"）
	OverallRecentCacheRead int  // 滑动窗最近 N 次的 cacheRead 总和
	OverallRecentInput     int  // 滑动窗最近 N 次的 input 总和
	OverallRecentSamples   int  // 滑动窗内的样本数（≤ recentSampleCap）

	// MissingAssistantUsage > 0 通常意味着上游 streaming 没按 OpenAI
	// stream_options.include_usage 协议发 final usage chunk（自建 proxy 常见），
	// 导致 UsageTracker 收不到任何累计数据。UI 据此明示用户排查 backend，
	// 不要让用户误以为是缓存模块本身坏了。
	MissingAssistantUsage int

	// 缓存 per-role 维度，按 CacheRead 降序，已过滤未消费 token 的 role
	CachePerAgent []AgentCacheStat
	CachePerModel []AgentCacheStat

	// 基础设定
	Premise          string
	Outline          []OutlineSnapshot
	Characters       []string
	SupportingCount  int      // 配角名册中的次要角色总数
	RecentSupporting []string // 最近活跃的次要角色（最多 5 个，按 LastSeenChapter 倒序）
	Layered          bool
	CurrentVolumeArc string
	NextVolumeTitle  string
	CompassDirection string
	CompassScale     string

	// 详情
	LastCommitSummary  string
	LastReviewSummary  string
	LastCheckpointName string
	RecentSummaries    []string
}

// OutlineSnapshot 是大纲条目的展示摘要。
type OutlineSnapshot struct {
	Chapter   int
	Title     string
	CoreEvent string
}

// AgentSnapshot 是 Agent 状态的展示投影。
type AgentSnapshot struct {
	Name      string
	State     string
	TaskID    string
	TaskKind  string
	Summary   string
	Tool      string
	Turn      int
	Context   AgentContextSnapshot
	UpdatedAt time.Time
}

// AgentCacheStat 是单个 agent 的缓存命中累计（投影到左栏）。
// HitRate = CacheRead / Input；Input 在 litellm 层已统一为"含 CacheRead"语义。
//
// CacheCapable 用来区分两种 0% 命中：
//   - true  → 模型支持 prompt cache，0% 是 prompt 设计差或前缀不稳定，需要优化
//   - false → 模型/provider 不支持 prompt cache，0% 是预期，不必排查
//
// Recent* 是滑动窗（最近 N 次调用）的命中数据，对比累计可识别"前期拖累"vs"稳态低命中"。
type AgentCacheStat struct {
	Role            string
	Model           string
	Input           int
	Output          int
	CacheRead       int
	CacheWrite      int
	Cost            float64
	Saved           float64
	CacheCapable    bool
	RecentCacheRead int
	RecentInput     int
	RecentSamples   int
}

// AgentContextSnapshot 是 Agent 上下文使用情况。
type AgentContextSnapshot struct {
	Tokens          int
	ContextWindow   int
	Percent         float64
	Scope           string
	Strategy        string
	ActiveMessages  int
	SummaryMessages int
	CompactedCount  int
	KeptCount       int
}

// CoCreateMessage 是共创对话的消息。
type CoCreateMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// CoCreateReply 是共创对话的 LLM 回复。Raw 保留模型完整四段原文，
// 用于写回 history 让下一轮模型看到自己上一轮的 [DRAFT]，从而真正在
// 已有草稿上累积更新（仅 Message 不含 [DRAFT]，会导致模型每轮凭对话重新归纳）。
// Suggestions 是 AI 主动给的"接下来你可能想说"，用户卡壳时按数字键一键填入输入框。
type CoCreateReply struct {
	Message     string
	Prompt      string
	Ready       bool
	Suggestions []string
	Raw         string
}

// ReplayDeltaText 从运行时队列项中提取可回放的流式文本。
func ReplayDeltaText(item domain.RuntimeQueueItem) string {
	if payload, ok := item.Payload.(map[string]any); ok {
		if text, ok := payload["delta"].(string); ok {
			return text
		}
	}
	return ""
}

// BuildStartPrompt 将用户需求包装为 Coordinator 的启动 prompt。
// 自动从用户输入中提取数字约束（总章数、每章字数），显式注入为结构化指令。
func BuildStartPrompt(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	extras := extractConstraints(prompt)

	header := "请根据以下创作要求开始创作一部小说。进入规划后，Premise 第一行必须输出 `# 书名`。若题材与冲突天然适合长篇连载，请优先规划为分层长篇结构，而不是压缩成短篇式梗概。\n\n[创作要求]\n"

	footer := "\n\n若某些细节未明确，请在不违背用户方向的前提下自行补全。用户明确给出的数字约束（如总章数、卷数、每章字数、女主数量等）必须严格遵从，偏差不得超过 2%。"

	if extras != "" {
		return header + prompt + "\n\n" + extras + footer
	}
	return header + prompt + footer
}

// extractConstraints 从用户原始需求中解析结构化的数字约束，返回显式指令文本块。
func extractConstraints(text string) string {
	var lines []string

	if ch := extractTotalChapters(text); ch > 0 {
		low := int(float64(ch) * 0.98)
		high := int(float64(ch) * 1.02)
		if low < 1 {
			low = 1
		}
		lines = append(lines, "【精确度约束——总章数】",
			"用户明确要求总计约 "+strconv.Itoa(ch)+" 章。",
			"这是硬锚点：所有卷的 estimated_chapters 之和必须在 "+strconv.Itoa(low)+"-"+strconv.Itoa(high)+" 章范围内。",
			"后续任何卷数调整都不得脱离此范围。",
			"")
	}

	if minW, maxW := extractChapterWords(text); minW > 0 || maxW > 0 {
		var wr string
		if minW > 0 && maxW > 0 {
			if minW == maxW {
				wr = "每章 "+strconv.Itoa(minW)+" 字"
			} else {
				wr = "每章 "+strconv.Itoa(minW)+"-"+strconv.Itoa(maxW)+" 字"
			}
		} else if minW > 0 {
			wr = "每章最少 "+strconv.Itoa(minW)+" 字"
		} else {
			wr = "每章最多 "+strconv.Itoa(maxW)+" 字"
		}
		lines = append(lines, "【字数约束】",
			wr+"。章节字数不得超过此范围，writer 和 editor 均以此作为写作质量门禁。",
			"同时每章承载的剧情密度（core_event/scenes 数量）必须匹配此字数。",
			"")
	}

	if vol := extractVolumeCount(text); vol > 0 {
		lines = append(lines, "【卷数约束】",
			"用户要求规划为 "+strconv.Itoa(vol)+" 卷。",
			"每卷承担不同的叙事功能，卷间有递进关系。",
			"")
	}

	if count, label := extractCharacterCount(text); count > 0 {
		lines = append(lines, "【角色数量约束】",
			"用户要求 "+label+" "+strconv.Itoa(count)+" 位。角色必须有差异化定位，各自承担独立功能。",
			"")
	}

	if scale := extractEstimatedScale(text); scale != "" {
		if ch := extractTotalChapters(text); ch > 0 {
			// estimated_scale 与总章数同步，避免冗余
		} else {
			lines = append(lines, "【规模约束】",
				"用户预估规模为 "+scale+"。指南针 compass.estimated_scale 应反映此目标。",
				"")
		}
	}

	if genre := extractGenre(text); genre != "" {
		lines = append(lines, "【题材约束】",
			"用户题材为："+genre+"。所有设定应与该题材一致。",
			"")
	}

	if pref := extractPreferences(text); pref != "" {
		lines = append(lines, "【写作风格偏好】",
			pref,
			"")
	}

	return strings.Join(lines, "\n")
}

// ── 约束提取辅助函数 ──

// extractTotalChapters 从用户文本中提取明确指定的总章数。
func extractTotalChapters(text string) int {
	runes := []rune(text)
	n := len(runes)

	for i := 0; i < n; i++ {
		if runes[i] == '约' || runes[i] == '~' || runes[i] == '～' {
			j := i + 1
			for j < n && runes[j] == ' ' {
				j++
			}
			if num := readNumberAt(runes, j); num > 0 {
				k := j + digitLen(runes, j)
				for k < n && runes[k] == ' ' {
					k++
				}
				if k < n && runes[k] == '章' {
					return capChapterCount(num)
				}
			}
		}
	}

	for i := 0; i < n; i++ {
		if runes[i] >= '0' && runes[i] <= '9' {
			num := readNumberAt(runes, i)
			if num > 0 {
				k := i + digitLen(runes, i)
				for k < n && runes[k] == ' ' {
					k++
				}
				if k < n && runes[k] == '章' {
					return capChapterCount(num)
				}
			}
		}
	}
	return 0
}

// extractChapterWords 提取每章字数范围，返回 (min, max)。
func extractChapterWords(text string) (int, int) {
	runes := []rune(text)
	n := len(runes)
	for i := 0; i < n-1; i++ {
		if runes[i] == '每' && runes[i+1] == '章' {
			start := i + 2
			for start < n && runes[start] == ' ' {
				start++
			}
			if start >= n || runes[start] < '0' || runes[start] > '9' {
				continue
			}
			num1 := readNumberAt(runes, start)
			if num1 <= 0 {
				continue
			}
			k := start + digitLen(runes, start)
			for k < n && runes[k] == ' ' {
				k++
			}
			if k < n && (runes[k] == '-' || runes[k] == '～' || runes[k] == '~') {
				k++
				for k < n && runes[k] == ' ' {
					k++
				}
				num2 := readNumberAt(runes, k)
				if num2 > 0 {
					k2 := k + digitLen(runes, k)
					for k2 < n && runes[k2] == ' ' {
						k2++
					}
					if k2 < n && runes[k2] == '字' {
						return num1, num2
					}
				}
			}
			if k < n && runes[k] == '字' {
				return num1, num1
			}
		}
	}
	return 0, 0
}

func readNumberAt(runes []rune, start int) int {
	if start >= len(runes) || runes[start] < '0' || runes[start] > '9' {
		return 0
	}
	n := 0
	for i := start; i < len(runes); i++ {
		if runes[i] >= '0' && runes[i] <= '9' {
			n = n*10 + int(runes[i]-'0')
		} else {
			break
		}
	}
	return n
}

func digitLen(runes []rune, start int) int {
	i := start
	for i < len(runes) && runes[i] >= '0' && runes[i] <= '9' {
		i++
	}
	return i - start
}

func capChapterCount(n int) int {
	if n <= 0 {
		return 0
	}
	if n < 5 {
		return 0
	}
	if n > 5000 {
		return 5000
	}
	return n
}

// extractVolumeCount 提取卷数，如"4卷"、"4-5卷"、"3卷"。
func extractVolumeCount(text string) int {
	runes := []rune(text)
	n := len(runes)
	for i := 0; i < n; i++ {
		if runes[i] >= '0' && runes[i] <= '9' {
			num := readNumberAt(runes, i)
			if num <= 0 {
				continue
			}
			if num > 20 {
				continue
			}
			k := i + digitLen(runes, i)
			for k < n && runes[k] == ' ' {
				k++
			}
			if k < n && runes[k] == '卷' {
				return num
			}
			// 范围格式：3-4卷
			if k < n && (runes[k] == '-' || runes[k] == '～' || runes[k] == '~') {
				k++
				for k < n && runes[k] == ' ' {
					k++
				}
				_ = readNumberAt(runes, k) // 读第二个数字但不使用
				k2 := k + digitLen(runes, k)
				for k2 < n && runes[k2] == ' ' {
					k2++
				}
				if k2 < n && runes[k2] == '卷' {
					return num // 返回起始值
				}
			}
		}
	}
	return 0
}

// extractCharacterCount 提取角色/女主数量，返回 (数量, "女主"/"角色")。
func extractCharacterCount(text string) (int, string) {
	runes := []rune(text)
	n := len(runes)

	// 匹配"3-4位女主"、"3位女主"、"4个女主"、"3-4人"
	for i := 0; i < n-1; i++ {
		if (runes[i] == '女' && runes[i+1] == '主') ||
			(runes[i] == '主' && i+1 < n && runes[i+1] == '角') {
			// 向后找数字
			start := i + 2
			for start < n && runes[start] == ' ' {
				start++
			}
			// 也向前找数字
			j := i - 1
			for j >= 0 && runes[j] == ' ' {
				j--
			}
			if j >= 0 && runes[j] >= '0' && runes[j] <= '9' {
				end := j
				for end >= 0 && runes[end] >= '0' && runes[end] <= '9' {
					end--
				}
				num := readNumberAt(runes, end+1)
				if num > 0 && num <= 20 {
					return num, "女主"
				}
			}
			continue
		}
	}

	// 匹配"X-Y位" "X人" "X位"
	for i := 0; i < n; i++ {
		if runes[i] >= '0' && runes[i] <= '9' {
			num := readNumberAt(runes, i)
			if num <= 0 || num > 20 {
				continue
			}
			k := i + digitLen(runes, i)
			for k < n && runes[k] == ' ' {
				k++
			}
			if k < n && (runes[k] == '人' || runes[k] == '位') {
				return num, "角色"
			}
		}
	}
	return 0, ""
}

// extractEstimatedScale 提取规模描述，如"4卷约100章"、"4卷100章"。
func extractEstimatedScale(text string) string {
	vol := extractVolumeCount(text)
	ch := extractTotalChapters(text)
	if vol > 0 && ch > 0 {
		return strconv.Itoa(vol) + "卷约" + strconv.Itoa(ch) + "章"
	}
	if vol > 0 {
		return strconv.Itoa(vol) + "卷"
	}
	if ch > 0 {
		return "约" + strconv.Itoa(ch) + "章"
	}
	return ""
}

// extractGenre 从文本中提取题材。返回空字符串表示无法确定性判定。
func extractGenre(text string) string {
	runes := []rune(text)
	textLower := string(runes) // 保留原文用于匹配

	genreKeywords := []struct {
		keywords []string
		genre    string
	}{
		{[]string{"修仙", "修真", "仙侠", "玄幻", "飞升", "修炼", "灵气", "灵根"}, "仙侠/玄幻"},
		{[]string{"穿越", "架空", "重生", "古代", "历史", "王朝", "帝王", "权谋", "争霸"}, "历史穿越/架空"},
		{[]string{"科幻", "星际", "未来", "赛博", "机甲", "基因", "宇宙", "AI", "人工智能"}, "科幻"},
		{[]string{"都市", "现代", "职场", "豪门", "娱乐圈"}, "都市/现代"},
		{[]string{"悬疑", "推理", "侦探", "刑侦", "破案", "谜案"}, "悬疑推理"},
		{[]string{"言情", "甜宠", "虐恋", "纯爱", "恋爱"}, "言情"},
		{[]string{"系统", "无限流", "快穿", "游戏", "升级", "爽文"}, "系统/无限流"},
	}

	matched := make(map[string]int) // genre → score
	for _, entry := range genreKeywords {
		score := 0
		for _, kw := range entry.keywords {
			if strings.Contains(textLower, kw) {
				score++
			}
		}
		if score > 0 {
			matched[entry.genre] = matched[entry.genre] + score
		}
	}

	// 找最高分
	bestGenre := ""
	bestScore := 0
	for g, s := range matched {
		if s > bestScore {
			bestScore = s
			bestGenre = g
		}
	}

	return bestGenre
}

// extractPreferences 提取自然语言写作偏好（节奏、风格等非数字约束）。
// 匹配"全程无尿点"类完整短语，返回拼接文本。
func extractPreferences(text string) string {
	runes := []rune(text)
	textStr := string(runes)

	patterns := []struct {
		keywords []string
		label    string
	}{
		{[]string{"全程无尿点", "无尿点"}, "全程无尿点"},
		{[]string{"节奏紧凑", "紧凑"}, "节奏紧凑"},
		{[]string{"剧情连贯", "连贯"}, "剧情连贯"},
		{[]string{"爽文", "爽"}, "爽文风格"},
		{[]string{"不拖沓", "不注水", "不水"}, "不拖沓"},
		{[]string{"轻松", "欢乐"}, "轻松向"},
		{[]string{"幽默", "搞笑", "有趣"}, "幽默风格"},
		{[]string{"慢热", "细水长流"}, "慢热风格"},
		{[]string{"烧脑", "硬核"}, "硬核向"},
		{[]string{"感人", "催泪", "虐"}, "情感向"},
	}

	var matched []string
	for _, p := range patterns {
		for _, kw := range p.keywords {
			if strings.Contains(textStr, kw) {
				matched = append(matched, p.label)
				break
			}
		}
	}

	if len(matched) == 0 {
		// 兜底：尝试找逗号分隔的"形容词+无"结构
		if strings.Contains(textStr, "全程") {
			return "节奏紧凑"
		}
		return ""
	}

	// 去重
	seen := make(map[string]bool)
	var unique []string
	for _, m := range matched {
		if !seen[m] {
			seen[m] = true
			unique = append(unique, m)
		}
	}
	return strings.Join(unique, "、")
}
