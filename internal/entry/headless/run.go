package headless

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/voocel/ainovel-cli/assets"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/diag"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/entry/startup"
	"github.com/voocel/ainovel-cli/internal/host"
	"github.com/voocel/ainovel-cli/internal/logger"
	"github.com/voocel/ainovel-cli/internal/store"
	"github.com/voocel/ainovel-cli/internal/tools"
	"github.com/voocel/ainovel-cli/internal/utils"
)

type Options struct {
	Prompt string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Run 以无界面模式运行会话内核，直接消费 Engine 事件与流式输出。
// 未来若新增"续写已有小说"等共享启动方式，不应直接堆到这里，
// 而应先落到 internal/entry/startup，再由 headless 入口调用。
func Run(cfg bootstrap.Config, bundle assets.Bundle, opts Options) error {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	stdin := opts.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}

	eng, err := host.New(cfg, bundle)
	if err != nil {
		return err
	}

	// ── stdin 分发器：统一管理 stdin 读取，在"引擎问问题"和"用户干预"之间切换 ──
	disp := newStdinDispatcher(stdin)
	eng.AskUser().SetHandler(disp.askUserHandler())
	cleanup := logger.SetupFile(eng.Dir(), "headless.log", false)
	defer cleanup()
	defer eng.Close()
	// 运行结束 / 出错返回时落一份脱敏诊断，方便 headless 用户贴 issue。
	// （外部 kill 的挂死不走 defer，仍需在 TUI 里手动 /diag。）
	defer func() { _, _ = diag.Export(store.NewStore(eng.Dir())) }()

	prompt := strings.TrimSpace(opts.Prompt)
	if prompt != "" {
		plan, err := startup.PrepareQuick(startup.Request{
			Mode:        startup.ModeQuick,
			UserPrompt:  prompt,
			OutputDir:   eng.Dir(),
			Interactive: true,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(stderr, "headless 启动: %s\n", eng.Dir())
		// 启动侧确定性生成本书用户规则快照（用原始 prompt 归一化），须在 StartPrepared 前。
		if err := eng.PrepareUserRules(plan.RawPrompt); err != nil {
			return err
		}
		if err := eng.StartPrepared(plan.StartPrompt); err != nil {
			return err
		}
	} else {
		items, err := eng.ReplayQueue(0)
		if err != nil {
			return err
		}
		roundHasContent, err := replayQueue(items, stdout, stderr)
		if err != nil {
			return err
		}
		label, err := eng.Resume()
		if err != nil {
			return err
		}
		if label == "" {
			return fmt.Errorf("headless 模式需要 --prompt，或输出目录 %q 下已有可恢复会话", eng.Dir())
		}
		fmt.Fprintf(stderr, "headless 恢复: %s (%s)\n", eng.Dir(), label)
		return consume(eng, stdout, stderr, roundHasContent, disp.steerChan())
	}

	return consume(eng, stdout, stderr, false, disp.steerChan())
}

// ── stdin 分发器 ──

const (
	stateIdle   int32 = 0
	stateAsking int32 = 1
)

type stdinDispatcher struct {
	in       io.Reader
	state    atomic.Int32
	steerCh  chan string
	askCh    chan string
	startOnce sync.Once
}

func newStdinDispatcher(in io.Reader) *stdinDispatcher {
	return &stdinDispatcher{
		in:      in,
		steerCh: make(chan string, 64),
		askCh:   make(chan string, 16),
	}
}

func (d *stdinDispatcher) start() {
	d.startOnce.Do(func() {
		go d.readLoop()
	})
}

func (d *stdinDispatcher) steerChan() <-chan string {
	d.start()
	return d.steerCh
}

// readLoop 是唯一从底层 stdin 读取的 goroutine。
// 根据 state 将输入行路由到 steerCh 或 askCh。
func (d *stdinDispatcher) readLoop() {
	scanner := bufio.NewScanner(d.in)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r\n")
		if line == "" {
			continue
		}
		if d.state.Load() == stateAsking {
			d.askCh <- line
		} else {
			d.steerCh <- line
		}
	}
}

// askUserHandler 返回一个 AskUser handler，从 askCh 读取用户回答。
func (d *stdinDispatcher) askUserHandler() func(ctx context.Context, questions []tools.Question) (*tools.AskUserResponse, error) {
	d.start() // 确保 readLoop 已启动
	return func(ctx context.Context, questions []tools.Question) (*tools.AskUserResponse, error) {
		d.state.Store(stateAsking)
		defer d.state.Store(stateIdle)

		resp := &tools.AskUserResponse{
			Answers: make(map[string]string, len(questions)),
			Notes:   make(map[string]string),
		}

		for _, q := range questions {
			answer, note, err := d.askOne(ctx, q)
			if err != nil {
				return nil, err
			}
			resp.Answers[q.Question] = answer
			if strings.TrimSpace(note) != "" {
				resp.Notes[q.Question] = note
			}
		}

		return resp, nil
	}
}

func (d *stdinDispatcher) askOne(ctx context.Context, q tools.Question) (string, string, error) {
	// 问题输出到 stderr，GUI 可以看到但不要求 GUI 必须处理
	fmt.Fprintf(os.Stderr, "\n[%s] %s\n", q.Header, q.Question)
	for i, opt := range q.Options {
		fmt.Fprintf(os.Stderr, "  %d. %s - %s\n", i+1, opt.Label, opt.Description)
	}
	fmt.Fprintln(os.Stderr, "  0. 自定义输入")

	for {
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		if q.MultiSelect {
			fmt.Fprint(os.Stderr, "请输入编号，多个用逗号分隔: ")
		} else {
			fmt.Fprint(os.Stderr, "请输入编号: ")
		}

		line, err := d.readLine()
		if err != nil {
			return "", "", err
		}
		line = utils.CleanInputLine(line)
		if line == "" {
			fmt.Fprintln(os.Stderr, "输入不能为空，请重试。")
			continue
		}
		if line == "0" {
			fmt.Fprint(os.Stderr, "请输入自定义内容: ")
			note, err := d.readLine()
			if err != nil {
				return "", "", err
			}
			note = utils.CleanInputLine(note)
			if note == "" {
				fmt.Fprintln(os.Stderr, "自定义内容不能为空，请重试。")
				continue
			}
			return "自定义", note, nil
		}

		labels, err := parseSelections(line, q.Options, q.MultiSelect)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v，请重试。\n", err)
			continue
		}
		return strings.Join(labels, "、"), "", nil
	}
}

func (d *stdinDispatcher) readLine() (string, error) {
	line, ok := <-d.askCh
	if !ok {
		return "", io.EOF
	}
	return line, nil
}

func parseSelections(line string, options []tools.Option, multi bool) ([]string, error) {
	parts := strings.Split(line, ",")
	if !multi && len(parts) > 1 {
		return nil, fmt.Errorf("当前问题只允许单选")
	}

	seen := make(map[int]bool, len(parts))
	labels := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("编号不能为空")
		}

		var idx int
		if _, err := fmt.Sscanf(part, "%d", &idx); err != nil {
			return nil, fmt.Errorf("无法识别编号 %q", part)
		}
		if idx <= 0 || idx > len(options) {
			return nil, fmt.Errorf("编号 %d 超出范围", idx)
		}
		if seen[idx] {
			continue
		}
		seen[idx] = true
		labels = append(labels, options[idx-1].Label)
	}
	if len(labels) == 0 {
		return nil, fmt.Errorf("至少选择一个选项")
	}
	return labels, nil
}

// ── consume 循环 ──

func consume(eng *host.Host, stdout, stderr io.Writer, roundHasContent bool, steerCh <-chan string) error {
	for {
		select {
		case ev, ok := <-eng.Events():
			if !ok {
				return nil
			}
			writeEvent(stderr, ev)
		case delta, ok := <-eng.Stream():
			if !ok {
				continue
			}
			if delta == host.StreamClearSentinel {
				if roundHasContent {
					if _, err := io.WriteString(stdout, "\n\n"); err != nil {
						return err
					}
					roundHasContent = false
				}
				continue
			}
			if delta == "" {
				continue
			}
			if _, err := io.WriteString(stdout, delta); err != nil {
				return err
			}
			roundHasContent = true
		case text, ok := <-steerCh:
			if !ok || text == "" {
				continue
			}
			// 用户干预：通过 Host.Steer 注入到 coordinator，它会立即中断当前工作
			eng.Steer(text)
		case _, ok := <-eng.Done():
			if !ok {
				return nil
			}
			return drainPending(eng, stdout, stderr, roundHasContent, steerCh)
		}
	}
}

func drainPending(eng *host.Host, stdout, stderr io.Writer, roundHasContent bool, steerCh <-chan string) error {
	for {
		select {
		case ev, ok := <-eng.Events():
			if ok {
				writeEvent(stderr, ev)
			}
		case delta, ok := <-eng.Stream():
			if !ok {
				continue
			}
			if delta == host.StreamClearSentinel {
				if roundHasContent {
					if _, err := io.WriteString(stdout, "\n\n"); err != nil {
						return err
					}
					roundHasContent = false
				}
				continue
			}
			if delta != "" {
				if _, err := io.WriteString(stdout, delta); err != nil {
					return err
				}
				roundHasContent = true
			}
		case text, ok := <-steerCh:
			if !ok || text == "" {
				continue
			}
			// 停机态下收到干预：Steer 会自动持久化，下次启动生效
			eng.Steer(text)
		default:
			if roundHasContent {
				if _, err := io.WriteString(stdout, "\n"); err != nil {
					return err
				}
			}
			return nil
		}
	}
}

func writeEvent(w io.Writer, ev host.Event) {
	if w == nil || strings.TrimSpace(ev.Summary) == "" {
		return
	}
	ts := ev.Time.Format("15:04:05")
	if ts == "00:00:00" {
		ts = "--:--:--"
	}
	fmt.Fprintf(w, "[%s] [%s] %s\n", ts, ev.Category, ev.Summary)
}

func replayQueue(items []domain.RuntimeQueueItem, stdout, stderr io.Writer) (bool, error) {
	var roundHasContent bool
	for _, item := range items {
		switch item.Kind {
		case domain.RuntimeQueueUIEvent:
			writeEvent(stderr, host.Event{
				Time:     item.Time,
				Category: item.Category,
				Summary:  item.Summary,
			})
		case domain.RuntimeQueueStreamClear:
			if roundHasContent {
				if _, err := io.WriteString(stdout, "\n\n"); err != nil {
					return roundHasContent, err
				}
				roundHasContent = false
			}
		case domain.RuntimeQueueStreamDelta:
			text := host.ReplayDeltaText(item)
			if text == "" {
				continue
			}
			if _, err := io.WriteString(stdout, text); err != nil {
				return roundHasContent, err
			}
			roundHasContent = true
		}
	}
	return roundHasContent, nil
}
