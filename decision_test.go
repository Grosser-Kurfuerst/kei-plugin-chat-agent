package chatagent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

func waitLog(t *testing.T, buf *syncBuffer, substr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), substr) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("log %q not found:\n%s", substr, buf.String())
}

func TestDecisionFilterReasons(t *testing.T) {
	t.Run("empty_text", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false})
		ev := groupEvent("e", "u", "", "g")
		ev.Message.Segments = []bot.Segment{{Type: bot.SegFace, Data: map[string]any{bot.KeyFaceID: "1"}}}
		_ = env.p.handleGroupMessage(context.Background(), ev, bot.NewNoopReply())
		waitLog(t, env.logs, "reason=empty_text")
		if len(env.p.sessions) != 0 {
			t.Fatal("empty_text created session")
		}
	})
	t.Run("no_sender", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false})
		ev := groupEvent("e", "u", "hello", "g")
		ev.Sender = nil
		_ = env.p.handleGroupMessage(context.Background(), ev, bot.NewNoopReply())
		waitLog(t, env.logs, "reason=no_sender")
	})
	t.Run("bot_sender", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false})
		ev := groupEvent("e", "u", "hello", "g")
		ev.Sender.IsBot = true
		_ = env.p.handleGroupMessage(context.Background(), ev, bot.NewNoopReply())
		waitLog(t, env.logs, "reason=bot_sender")
	})
	t.Run("self_sender", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false})
		ev := groupEvent("e", "bot", "hello", "g")
		_ = env.p.handleGroupMessage(context.Background(), ev, bot.NewNoopReply())
		waitLog(t, env.logs, "reason=self_sender")
	})
	t.Run("command", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false})
		ev := commandEvent("echo", "hello", "u", "g")
		_ = env.p.handleGroupMessage(context.Background(), ev, bot.NewNoopReply())
		waitLog(t, env.logs, "reason=command")
	})
	t.Run("not_allowed", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false, "group_policy": "off"})
		_ = env.p.handleGroupMessage(context.Background(), groupEvent("e", "u", "hello", "g"), bot.NewNoopReply())
		waitLog(t, env.logs, "reason=not_allowed")
	})
	t.Run("too_short", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false})
		_ = env.p.handleGroupMessage(context.Background(), groupEvent("e", "u", "a", "g"), bot.NewNoopReply())
		waitLog(t, env.logs, "reason=too_short")
		if len(env.p.sessions) != 0 {
			t.Fatal("too_short created session")
		}
	})
	t.Run("channel_off", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false})
		runCommand(t, env.p, commandEvent("chat", "off", "admin", "g"))
		_ = env.p.handleGroupMessage(context.Background(), groupEvent("e", "u", "hello", "g"), bot.NewNoopReply())
		waitLog(t, env.logs, "reason=channel_off")
		if len(env.bot.sendsSnapshot()) != 0 {
			t.Fatal("disabled session replied")
		}
	})
	t.Run("not_addressed", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false})
		_ = env.p.handleGroupMessage(context.Background(), groupEvent("e", "u", "hello", "g"), bot.NewNoopReply())
		waitLog(t, env.logs, "reason=not_addressed")
	})
	t.Run("probability", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{
			"mention_reply_probability": 0.0,
			"random_enabled":            false,
		})
		_ = env.p.handleGroupMessage(context.Background(), atEvent("e", "u", "hello", "g", "bot"), bot.NewNoopReply())
		waitLog(t, env.logs, "reason=probability")
	})
	t.Run("cooldown", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false})
		env.chat.replies = []string{"one"}
		_ = env.p.handleGroupMessage(context.Background(), atEvent("e1", "u", "hello", "g", "bot"), bot.NewNoopReply())
		env.timers.fireLast()
		env.bot.waitSends(1, time.Second)
		_ = env.p.handleGroupMessage(context.Background(), atEvent("e2", "u", "again", "g", "bot"), bot.NewNoopReply())
		waitLog(t, env.logs, "reason=cooldown")
		if len(env.bot.sendsSnapshot()) != 1 {
			t.Fatal("cooldown did not block second reply")
		}
	})
}

func TestRandomRejectReasons(t *testing.T) {
	t.Run("random_disabled", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false})
		_ = env.p.handleGroupMessage(context.Background(), groupEvent("e", "u", "hello", "g"), bot.NewNoopReply())
		waitLog(t, env.logs, "reason=not_addressed")
	})
	t.Run("hour_quota_zero", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_max_per_hour": 0})
		_ = env.p.handleGroupMessage(context.Background(), groupEvent("e", "u", "hello", "g"), bot.NewNoopReply())
		waitLog(t, env.logs, "reason=hour_quota")
	})
	t.Run("quiet_hours", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{
			"random_probability":      1.0,
			"random_min_participants": 0,
			"random_quiet_hours":      "23:00-07:00",
			"random_timezone":         "UTC",
		})
		env.p.now = func() time.Time {
			return time.Date(2026, 1, 1, 23, 30, 0, 0, time.UTC)
		}
		_ = env.p.handleGroupMessage(context.Background(), groupEvent("e", "u", "hello", "g"), bot.NewNoopReply())
		waitLog(t, env.logs, "reason=quiet_hours")
	})
	t.Run("min_participants", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{
			"random_probability":      1.0,
			"random_min_participants": 2,
		})
		_ = env.p.handleGroupMessage(context.Background(), groupEvent("e", "u", "hello", "g"), bot.NewNoopReply())
		waitLog(t, env.logs, "reason=min_participants")
	})
	t.Run("probability", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{
			"random_probability":      0.0,
			"random_min_participants": 0,
		})
		_ = env.p.handleGroupMessage(context.Background(), groupEvent("e", "u", "hello", "g"), bot.NewNoopReply())
		waitLog(t, env.logs, "reason=probability")
	})
	t.Run("hour_quota_limit", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{
			"random_probability":      1.0,
			"random_min_participants": 0,
			"random_max_per_hour":     1,
		})
		s := env.p.getOrCreateSession(groupEvent("x", "u", "x", "g"))
		now := env.p.now()
		s.mu.Lock()
		s.hourly = []time.Time{now}
		s.mu.Unlock()
		_ = env.p.handleGroupMessage(context.Background(), groupEvent("e", "u", "hello", "g"), bot.NewNoopReply())
		waitLog(t, env.logs, "reason=hour_quota")
	})
	t.Run("min_participants_reached", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{
			"random_probability":      1.0,
			"random_min_participants": 2,
		})
		_ = env.p.handleGroupMessage(context.Background(), groupEvent("e1", "u1", "hello", "g"), bot.NewNoopReply())
		waitLog(t, env.logs, "reason=min_participants")
		env.chat.replies = []string{"random reply"}
		_ = env.p.handleGroupMessage(context.Background(), groupEvent("e2", "u2", "hello again", "g"), bot.NewNoopReply())
		env.timers.fireLast()
		if got := env.bot.waitSends(1, time.Second); len(got) != 1 {
			t.Fatalf("sends=%d", len(got))
		}
	})
}

func TestAddressing(t *testing.T) {
	t.Run("at_self_id", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false})
		env.chat.replies = []string{"ok"}
		_ = env.p.handleGroupMessage(context.Background(), atEvent("e", "u", "hello", "g", "bot"), bot.NewNoopReply())
		env.timers.fireLast()
		if len(env.bot.waitSends(1, time.Second)) != 1 {
			t.Fatal("no reply")
		}
	})
	t.Run("at_other_id", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false})
		_ = env.p.handleGroupMessage(context.Background(), atEvent("e", "u", "hello", "g", "other"), bot.NewNoopReply())
		if len(env.bot.sendsSnapshot()) != 0 {
			t.Fatal("unexpected reply")
		}
	})
	t.Run("at_all_never", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false})
		_ = env.p.handleGroupMessage(context.Background(), atEvent("e", "u", "hello", "g", "all"), bot.NewNoopReply())
		if len(env.bot.sendsSnapshot()) != 0 {
			t.Fatal("all must not address")
		}
	})
	t.Run("at_any_when_self_ids_empty", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false, "self_ids": []string{}})
		env.chat.replies = []string{"ok"}
		_ = env.p.handleGroupMessage(context.Background(), atEvent("e", "u", "hello", "g", "anyone"), bot.NewNoopReply())
		env.timers.fireLast()
		if len(env.bot.waitSends(1, time.Second)) != 1 {
			t.Fatal("any at should address when self_ids empty")
		}
	})
	t.Run("quote", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false, "mention_min_interval": "0s"})
		env.chat.replies = []string{"one", "two"}
		_ = env.p.handleGroupMessage(context.Background(), atEvent("e1", "u", "hello", "g", "bot"), bot.NewNoopReply())
		env.timers.fireLast()
		if len(env.bot.waitSends(1, time.Second)) != 1 {
			t.Fatal("no first reply")
		}
		// fakeBot 返回 sent-1；引用它应寻址。
		_ = env.p.handleGroupMessage(context.Background(), replyEvent("e2", "v", "follow up", "g", "sent-1"), bot.NewNoopReply())
		env.timers.fireLast()
		if len(env.bot.waitSends(2, time.Second)) != 2 {
			t.Fatal("quote did not address")
		}
	})
	t.Run("keyword_case_insensitive", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false, "trigger_keywords": []string{"Hello"}})
		env.chat.replies = []string{"ok"}
		_ = env.p.handleGroupMessage(context.Background(), groupEvent("e", "u", "say hELLo there", "g"), bot.NewNoopReply())
		env.timers.fireLast()
		if len(env.bot.waitSends(1, time.Second)) != 1 {
			t.Fatal("keyword did not address")
		}
	})
	t.Run("keyword_chinese", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false, "trigger_keywords": []string{"小助手"}})
		env.chat.replies = []string{"ok"}
		_ = env.p.handleGroupMessage(context.Background(), groupEvent("e", "u", "问问小助手吧", "g"), bot.NewNoopReply())
		env.timers.fireLast()
		if len(env.bot.waitSends(1, time.Second)) != 1 {
			t.Fatal("chinese keyword did not address")
		}
	})
	t.Run("private_always", func(t *testing.T) {
		env := newTestEnv(t, nil)
		env.chat.replies = []string{"ok"}
		_ = env.p.handlePrivateMessage(context.Background(), privateEvent("e", "u", "hello"), bot.NewNoopReply())
		env.timers.fireLast()
		sn := env.bot.waitSends(1, time.Second)
		if len(sn) != 1 {
			t.Fatal("private did not reply")
		}
		if sn[0].target.Kind != bot.MessagePrivate || sn[0].target.UserID != "u" || sn[0].target.ChannelID != "" {
			t.Fatalf("target=%+v", sn[0].target)
		}
	})
}

func TestProbabilityAndQuotaRandom(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_probability":      1.0,
		"random_min_participants": 0,
		"random_max_per_hour":     2,
		"random_cooldown":         "0s",
	})
	env.chat.replies = []string{"r1", "r2", "r3", "r4"}
	for i := 0; i < 2; i++ {
		ev := groupEvent("e"+itoa(i), "u", "hello", "g")
		_ = env.p.handleGroupMessage(context.Background(), ev, bot.NewNoopReply())
		env.timers.fireLast()
		if len(env.bot.waitSends(i+1, time.Second)) != i+1 {
			t.Fatalf("random reply %d not sent", i+1)
		}
	}
	_ = env.p.handleGroupMessage(context.Background(), groupEvent("e3", "u", "third", "g"), bot.NewNoopReply())
	waitLog(t, env.logs, "reason=hour_quota")
}

func TestRandomProbabilityInjection(t *testing.T) {
	base := map[string]any{
		"random_probability":      0.5,
		"random_min_participants": 0,
		"random_cooldown":         "0s",
	}
	hit := newTestEnv(t, base)
	hit.p.randFloat = func() float64 { return 0.4 }
	hit.chat.replies = []string{"hit"}
	_ = hit.p.handleGroupMessage(context.Background(), groupEvent("e1", "u", "hello", "g"), bot.NewNoopReply())
	hit.timers.fireLast()
	if len(hit.bot.waitSends(1, time.Second)) != 1 {
		t.Fatal("probability hit did not trigger")
	}
	miss := newTestEnv(t, base)
	miss.p.randFloat = func() float64 { return 0.6 }
	_ = miss.p.handleGroupMessage(context.Background(), groupEvent("e2", "u", "hello", "g"), bot.NewNoopReply())
	waitLog(t, miss.logs, "reason=probability")
	if len(miss.bot.sendsSnapshot()) != 0 {
		t.Fatal("probability miss sent")
	}
}

func TestBatchMergeAndMaxWindow(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled":   false,
		"batch_window":     "50ms",
		"batch_max_window": "100ms",
	})
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := base
	env.p.now = func() time.Time { return now }
	env.chat.replies = []string{"merged"}
	_ = env.p.handleGroupMessage(context.Background(), atEvent("e1", "u", "first", "g", "bot"), bot.NewNoopReply())
	if got := env.timers.lastDelay(); got != 50*time.Millisecond {
		t.Fatalf("first delay=%v", got)
	}
	now = base.Add(30 * time.Millisecond)
	_ = env.p.handleGroupMessage(context.Background(), atEvent("e2", "v", "second", "g", "bot"), bot.NewNoopReply())
	if got := env.timers.lastDelay(); got != 50*time.Millisecond {
		t.Fatalf("second delay=%v", got)
	}
	now = base.Add(80 * time.Millisecond)
	_ = env.p.handleGroupMessage(context.Background(), atEvent("e3", "w", "third", "g", "bot"), bot.NewNoopReply())
	if got := env.timers.lastDelay(); got != 20*time.Millisecond {
		t.Fatalf("third delay=%v, want 20ms", got)
	}
	env.timers.fireLast()
	if len(env.bot.waitSends(1, time.Second)) != 1 {
		t.Fatal("no reply")
	}
	req, ok := env.chat.lastRequest()
	if !ok {
		t.Fatal("no request")
	}
	content, _ := req.Messages[1].Content.(string)
	for _, want := range []string{"[本轮消息]", "u: first", "v: second", "w: third"} {
		if !strings.Contains(content, want) {
			t.Fatalf("content missing %q:\n%s", want, content)
		}
	}
}

func TestPendingDuringGeneration(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	calls := 0
	env.chat.fn = func(req ChatRequest) (ChatResult, error) {
		once.Do(func() { close(started) })
		<-release
		calls++
		if calls == 1 {
			return ChatResult{Content: "first"}, nil
		}
		return ChatResult{Content: "second"}, nil
	}
	_ = env.p.handleGroupMessage(context.Background(), atEvent("e1", "u", "one", "g", "bot"), bot.NewNoopReply())
	env.timers.fireLast()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("generation did not start")
	}
	_ = env.p.handleGroupMessage(context.Background(), atEvent("e2", "u", "two", "g", "bot"), bot.NewNoopReply())
	close(release)
	if len(env.bot.waitSends(1, time.Second)) != 1 {
		t.Fatal("first reply not sent")
	}
	waitLog(t, env.logs, "operation=generate")
	if got := env.timers.lastDelay(); got != 100*time.Millisecond {
		t.Fatalf("next batch delay=%v", got)
	}
	env.timers.fireLast()
	if len(env.bot.waitSends(2, time.Second)) != 2 {
		t.Fatal("pending batch not sent")
	}
}

func TestBatchSortedBySeq(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	s := env.p.getOrCreateSession(groupEvent("seed", "u", "seed", "g"))
	s.mu.Lock()
	s.pending = []Turn{
		{Seq: 1, UserID: "u", UserName: "U", Text: "one"},
		{Seq: 0, UserID: "u", UserName: "U", Text: "zero"},
	}
	s.firstAt = env.p.now()
	s.mu.Unlock()
	env.chat.replies = []string{"ok"}
	env.p.onBatchTimer(s, s.epoch)
	if len(env.bot.waitSends(1, time.Second)) != 1 {
		t.Fatal("no reply")
	}
	req, ok := env.chat.lastRequest()
	if !ok {
		t.Fatal("no request")
	}
	content, _ := req.Messages[1].Content.(string)
	zero := strings.Index(content, "U: zero")
	one := strings.Index(content, "U: one")
	if zero < 0 || one < 0 || zero > one {
		t.Fatalf("batch order:\n%s", content)
	}
}

func TestPendingNextBatchKeeps100msDeadline(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled":   false,
		"batch_window":     "500ms",
		"batch_max_window": "1s",
	})
	s := env.p.getOrCreateSession(groupEvent("seed", "u", "seed", "g"))
	s.mu.Lock()
	s.pending = []Turn{{Seq: 0, UserID: "u", UserName: "U", Text: "pending"}}
	s.mu.Unlock()
	env.p.finishGeneration(s, s.epoch)
	if got := env.timers.lastDelay(); got != 100*time.Millisecond {
		t.Fatalf("finish delay=%v", got)
	}
	_ = env.p.handleGroupMessage(context.Background(), atEvent("e", "u", "new", "g", "bot"), bot.NewNoopReply())
	got := env.timers.lastDelay()
	if got <= 0 || got > 100*time.Millisecond {
		t.Fatalf("new message rescheduled beyond 100ms: %v", got)
	}
}

func TestConcurrentHandlersSameGroup(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	var wg sync.WaitGroup
	const n = 12
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ev := atEvent("e"+itoa(i), "u"+itoa(i), "message "+itoa(i), "g", "bot")
			_ = env.p.handleGroupMessage(context.Background(), ev, bot.NewNoopReply())
		}(i)
	}
	wg.Wait()
	env.timers.fireLast()
	if len(env.bot.waitSends(1, 2*time.Second)) != 1 {
		t.Fatal("no reply")
	}
	s := env.p.getSession(sessionKey("mock", "bot", "group", "g", "u0"))
	if s == nil {
		t.Fatal("session missing")
	}
	s.mu.Lock()
	seq := s.seq
	history := len(s.history)
	s.mu.Unlock()
	if seq < n || history == 0 {
		t.Fatalf("seq=%d history=%d", seq, history)
	}
	req, ok := env.chat.lastRequest()
	if !ok {
		t.Fatal("no chat request")
	}
	content, _ := req.Messages[1].Content.(string)
	for i := 0; i < n; i++ {
		if !strings.Contains(content, "message "+itoa(i)) {
			t.Fatalf("missing message %d in batch:\n%s", i, content)
		}
	}
}

func TestSemaphoreFullAndStale(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	for i := 0; i < cap(env.p.sem); i++ {
		env.p.sem <- struct{}{}
	}
	_ = env.p.handleGroupMessage(context.Background(), atEvent("e", "u", "hello", "g", "bot"), bot.NewNoopReply())
	env.timers.fireLast()
	waitLog(t, env.logs, "reason=semaphore_full")
	for i := 0; i < cap(env.p.sem); i++ {
		<-env.p.sem
	}
	if len(env.bot.sendsSnapshot()) != 0 {
		t.Fatal("semaphore full sent a message")
	}

	s := env.p.getOrCreateSession(atEvent("x", "u", "hello", "g", "bot"))
	env.p.onBatchTimer(s, s.epoch+1)
	waitLog(t, env.logs, "reason=stale")
}

func TestReplyCleaningAndErrors(t *testing.T) {
	t.Run("llm_error", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false})
		env.chat.err = errors.New("boom")
		_ = env.p.handleGroupMessage(context.Background(), atEvent("e", "u", "hello", "g", "bot"), bot.NewNoopReply())
		env.timers.fireLast()
		waitLog(t, env.logs, "reason=llm_error")
	})
	t.Run("skipped", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{
			"random_probability":      1.0,
			"random_min_participants": 0,
		})
		env.chat.replies = []string{"[SKIP]"}
		_ = env.p.handleGroupMessage(context.Background(), groupEvent("e", "u", "hello", "g"), bot.NewNoopReply())
		env.timers.fireLast()
		waitLog(t, env.logs, "reason=skipped_by_llm")
		if len(env.bot.sendsSnapshot()) != 0 {
			t.Fatal("skip sent a message")
		}
	})
	t.Run("empty_reply", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false})
		env.chat.replies = []string{"   "}
		_ = env.p.handleGroupMessage(context.Background(), atEvent("e", "u", "hello", "g", "bot"), bot.NewNoopReply())
		env.timers.fireLast()
		waitLog(t, env.logs, "reason=empty_reply")
	})
	t.Run("duplicate_reply", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false, "mention_min_interval": "0s"})
		env.chat.replies = []string{"same", "same"}
		_ = env.p.handleGroupMessage(context.Background(), atEvent("e1", "u", "one", "g", "bot"), bot.NewNoopReply())
		env.timers.fireLast()
		env.bot.waitSends(1, time.Second)
		_ = env.p.handleGroupMessage(context.Background(), atEvent("e2", "u", "two", "g", "bot"), bot.NewNoopReply())
		env.timers.fireLast()
		waitLog(t, env.logs, "reason=duplicate_reply")
		if len(env.bot.sendsSnapshot()) != 1 {
			t.Fatal("duplicate sent")
		}
	})
	t.Run("send_error", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false})
		env.bot.sendErr = errors.New("send failed")
		env.chat.replies = []string{"reply"}
		_ = env.p.handleGroupMessage(context.Background(), atEvent("e", "u", "hello", "g", "bot"), bot.NewNoopReply())
		env.timers.fireLast()
		waitLog(t, env.logs, "reason=send_error")
	})
	t.Run("skip_not_applied_to_addressed", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false})
		env.chat.replies = []string{"[SKIP]"}
		_ = env.p.handleGroupMessage(context.Background(), atEvent("e", "u", "hello", "g", "bot"), bot.NewNoopReply())
		env.timers.fireLast()
		sn := env.bot.waitSends(1, time.Second)
		if len(sn) != 1 || messageText(sn[0].msg) != "[SKIP]" {
			t.Fatal("addressed reply must not treat skip token specially")
		}
	})
}

func TestImageAdmissionAndHistory(t *testing.T) {
	t.Run("vision_off_pure_image", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false})
		ev := imageEvent("e", "u", "", "g", "http://example.com/a.png")
		_ = env.p.handleGroupMessage(context.Background(), ev, bot.NewNoopReply())
		waitLog(t, env.logs, "reason=empty_text")
	})
	t.Run("vision_on_pure_image", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false, "vision_enabled": true})
		ev := imageEvent("e", "u", "", "g", "http://example.com/a.png")
		_ = env.p.handleGroupMessage(context.Background(), ev, bot.NewNoopReply())
		s := env.p.getSession(sessionKey("mock", "bot", "group", "g", "u"))
		if s == nil {
			t.Fatal("image message did not create session")
		}
		h := s.historySnapshot()
		if len(h) != 1 || h[0].Text != "[图片]" || len(h[0].Images) != 1 || h[0].Images[0] != "http://example.com/a.png" {
			t.Fatalf("history=%+v", h)
		}
	})
	t.Run("keyfile_only", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false, "vision_enabled": true})
		ev := groupEvent("e", "u", "", "g")
		ev.Message.Segments = []bot.Segment{{Type: bot.SegImage, Data: map[string]any{bot.KeyFile: "/tmp/x.png"}}}
		_ = env.p.handleGroupMessage(context.Background(), ev, bot.NewNoopReply())
		waitLog(t, env.logs, "reason=empty_text")
	})
	t.Run("image_skips_trigger_min", func(t *testing.T) {
		env := newTestEnv(t, map[string]any{"random_enabled": false, "vision_enabled": true})
		env.chat.replies = []string{"ok"}
		ev := imageEvent("e", "u", "a", "g", "http://127.0.0.1:1/a.png")
		ev.Message.Segments = append(ev.Message.Segments, bot.Segment{Type: bot.SegAt, Data: map[string]any{bot.KeyUserID: "bot"}})
		_ = env.p.handleGroupMessage(context.Background(), ev, bot.NewNoopReply())
		env.timers.fireLast()
		if len(env.bot.waitSends(1, time.Second)) != 1 {
			t.Fatal("image message with short text should pass")
		}
	})
}
