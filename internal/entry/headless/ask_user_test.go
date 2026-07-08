package headless

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/tools"
)

func TestStdinDispatcherAskUserSingleSelect(t *testing.T) {
	r, w := io.Pipe()

	disp := newStdinDispatcher(r)
	handler := disp.askUserHandler()

	go func() {
		time.Sleep(50 * time.Millisecond) // 等待 handler 启动并设置 state=asking
		_, _ = w.Write([]byte("2\n"))
		_ = w.Close()
	}()

	resp, err := handler(context.Background(), []tools.Question{
		{
			Question: "你想要什么风格？",
			Header:   "风格",
			Options: []tools.Option{
				{Label: "热血", Description: "偏升级"},
				{Label: "悬疑", Description: "偏谜团"},
			},
		},
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if got := resp.Answers["你想要什么风格？"]; got != "悬疑" {
		t.Fatalf("unexpected answer: %q", got)
	}
}

func TestStdinDispatcherAskUserCustomInput(t *testing.T) {
	r, w := io.Pipe()

	disp := newStdinDispatcher(r)
	handler := disp.askUserHandler()

	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte("0\n不要感情线\n"))
		_ = w.Close()
	}()

	resp, err := handler(context.Background(), []tools.Question{
		{
			Question: "还有什么限制？",
			Header:   "限制",
			Options: []tools.Option{
				{Label: "黑暗", Description: "整体压抑"},
				{Label: "轻松", Description: "基调明快"},
			},
		},
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if got := resp.Answers["还有什么限制？"]; got != "自定义" {
		t.Fatalf("unexpected answer: %q", got)
	}
	if got := resp.Notes["还有什么限制？"]; got != "不要感情线" {
		t.Fatalf("unexpected note: %q", got)
	}
}

// TestSteerChannel 验证非 askUser 时的 stdin 输入走 steer 通道。
func TestSteerChannel(t *testing.T) {
	r, w := io.Pipe()

	disp := newStdinDispatcher(r)
	ch := disp.steerChan()

	go func() {
		_, _ = w.Write([]byte("把感情线提前到第4章\n"))
		_ = w.Close()
	}()

	select {
	case line := <-ch:
		if line != "把感情线提前到第4章" {
			t.Fatalf("unexpected steer line: %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for steer line")
	}
}

// TestRoutingAskDoesNotLeakToSteer 验证 askUser 期间的输入不会泄漏到 steerCh。
func TestRoutingAskDoesNotLeakToSteer(t *testing.T) {
	r, w := io.Pipe()

	disp := newStdinDispatcher(r)
	steerCh := disp.steerChan()
	handler := disp.askUserHandler()

	done := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte("1\n"))
		_ = w.Close()
		close(done)
	}()

	// 先激活 askUser
	resp, err := handler(context.Background(), []tools.Question{
		{
			Question: "选一个？",
			Header:   "测试",
			Options: []tools.Option{
				{Label: "A", Description: "选项A"},
				{Label: "B", Description: "选项B"},
			},
		},
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if got := resp.Answers["选一个？"]; got != "A" {
		t.Fatalf("unexpected answer: %q", got)
	}

	<-done

	// 验证 steerCh 没有收到此输入
	select {
	case line := <-steerCh:
		t.Fatalf("steerCh should be empty, got %q", line)
	case <-time.After(200 * time.Millisecond):
		// OK
	}
}

// TestSteerBeforeAsk 验证 askUser 前的输入走 steerCh，askUser 中的不走。
func TestSteerBeforeAsk(t *testing.T) {
	r, w := io.Pipe()

	disp := newStdinDispatcher(r)
	steerCh := disp.steerChan()
	handler := disp.askUserHandler()

	// 先在 askUser 前发送干预
	go func() {
		_, _ = w.Write([]byte("干预指令\n"))
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte("1\n"))
		_ = w.Close()
	}()

	// 验证干预指令走了 steerCh
	select {
	case line := <-steerCh:
		if line != "干预指令" {
			t.Fatalf("expected '干预指令', got %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for steer line")
	}

	// 验证 askUser 正常
	resp, err := handler(context.Background(), []tools.Question{
		{
			Question: "选一个？",
			Header:   "测试",
			Options: []tools.Option{
				{Label: "A", Description: "选项A"},
				{Label: "B", Description: "选项B"},
			},
		},
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if got := resp.Answers["选一个？"]; got != "A" {
		t.Fatalf("unexpected answer: %q", got)
	}
}

func BenchmarkParseSelections(b *testing.B) {
	opts := []tools.Option{
		{Label: "A", Description: "descA"},
		{Label: "B", Description: "descB"},
		{Label: "C", Description: "descC"},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = parseSelections("1,3", opts, true)
	}
}
