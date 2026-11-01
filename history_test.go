package chatagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

func TestHistorySequenceAndTrim(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"history_max_messages": 3,
		"history_max_chars":    1000,
		"random_enabled":       false,
	})
	s := env.p.getOrCreateSession(groupEvent("x", "u", "x", "g"))
	for i := 0; i < 5; i++ {
		turn := env.p.appendUserTurn(s, "msg"+itoa(i), nil, "u", "U", "e"+itoa(i), env.p.now())
		if turn.Seq != uint64(i) {
			t.Fatalf("seq=%d want %d", turn.Seq, i)
		}
	}
	h := s.historySnapshot()
	if len(h) != 3 {
		t.Fatalf("len=%d", len(h))
	}
	if h[0].Seq != 2 || h[2].Seq != 4 {
		t.Fatalf("history seqs=%v", []uint64{h[0].Seq, h[1].Seq, h[2].Seq})
	}
	s.mu.Lock()
	lost := s.lostSeq
	s.mu.Unlock()
	if lost != 2 {
		t.Fatalf("lostSeq=%d", lost)
	}
}

func TestHistoryCharTrimAndConsolidatedWatermark(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"history_max_messages": 100,
		"history_max_chars":    10,
		"random_enabled":       false,
	})
	s := env.p.getOrCreateSession(groupEvent("x", "u", "x", "g"))
	for i := 0; i < 4; i++ {
		env.p.appendUserTurn(s, "12345", nil, "u", "U", "e", env.p.now())
	}
	h := s.historySnapshot()
	if len(h) != 2 {
		t.Fatalf("len=%d", len(h))
	}
	s.mu.Lock()
	if s.lostSeq != 2 {
		t.Fatalf("lostSeq=%d", s.lostSeq)
	}
	s.consolidatedSeq = 3
	s.mu.Unlock()
	env.p.appendUserTurn(s, "12345", nil, "u", "U", "e", env.p.now())
	s.mu.Lock()
	if s.lostSeq != 2 {
		t.Fatalf("lostSeq advanced for consolidated entries: %d", s.lostSeq)
	}
	s.mu.Unlock()
}

func TestHistoryImagesNotCounted(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"history_max_chars": 30,
		"random_enabled":    false,
	})
	s := env.p.getOrCreateSession(groupEvent("x", "u", "x", "g"))
	env.p.appendUserTurn(s, "short", []string{strings.Repeat("u", 5000)}, "u", "U", "e1", env.p.now())
	env.p.appendUserTurn(s, "next", nil, "u", "U", "e2", env.p.now())
	h := s.historySnapshot()
	if len(h) != 2 {
		t.Fatalf("image URL counted toward budget: %d", len(h))
	}
}

func TestRenderInboundPlaceholders(t *testing.T) {
	ev := groupEvent("e", "u", "", "g")
	ev.Message.Segments = []bot.Segment{
		{Type: bot.SegText, Data: map[string]any{bot.KeyText: "hello "}},
		{Type: bot.SegImage, Data: map[string]any{bot.KeyURL: "http://x/a.png"}},
		{Type: bot.SegFace, Data: map[string]any{bot.KeyFaceID: "1"}},
		{Type: bot.SegFile, Data: map[string]any{bot.KeyURL: "http://x/f"}},
		{Type: bot.SegCard, Data: map[string]any{bot.KeyCard: "c"}},
		{Type: bot.SegReply, Data: map[string]any{bot.KeyMessageID: "m"}},
		{Type: bot.SegText, Data: map[string]any{bot.KeyText: "x"}},
	}
	text, images, hasText, hasImage := renderInboundMessage(ev, false)
	if text != "hello [图片][表情][文件][卡片][引用]x" {
		t.Fatalf("text=%q", text)
	}
	if !hasText || hasImage || len(images) != 0 {
		t.Fatalf("flags text=%v image=%v images=%v", hasText, hasImage, images)
	}
	_, images, _, hasImage = renderInboundMessage(ev, true)
	if !hasImage || len(images) != 1 || images[0] != "http://x/a.png" {
		t.Fatalf("vision images=%v", images)
	}
	unknown := groupEvent("e", "u", "", "g")
	unknown.Message.Segments = []bot.Segment{{Type: bot.SegmentType("weird"), Data: map[string]any{}}}
	if text, _, _, _ := renderInboundMessage(unknown, false); text != "[消息]" {
		t.Fatalf("unknown placeholder=%q", text)
	}
}

func TestReplyIDRingAndDedupeWindow(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	s := env.p.getOrCreateSession(groupEvent("x", "u", "x", "g"))
	for i := 0; i < 130; i++ {
		env.p.appendSelfTurn(s, "reply", "bot", "id"+itoa(i), false)
	}
	if s.hasRecentReplyID("id0") {
		t.Fatal("old reply ID must be evicted")
	}
	if !s.hasRecentReplyID("id129") {
		t.Fatal("new reply ID missing")
	}
	now := env.p.now()
	s.mu.Lock()
	s.recentReplyTexts = []replyFingerprint{{Text: "Hello  World", At: now.Add(-11 * time.Minute)}}
	s.mu.Unlock()
	if s.isDuplicateReply("hello world", now) {
		t.Fatal("dedupe window expired")
	}
	s.mu.Lock()
	s.recentReplyTexts = []replyFingerprint{{Text: "Hello  World", At: now.Add(-time.Minute)}}
	s.mu.Unlock()
	if !s.isDuplicateReply("hello world", now) {
		t.Fatal("dedupe should match normalized text")
	}
}

func TestCleanupTTLProtectedAndHardTTL(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"history_ttl":                "10m",
		"history_unconsolidated_ttl": "1h",
		"history_max_sessions":       100,
		"random_enabled":             false,
	})
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	env.p.now = func() time.Time { return now }

	consolidated := env.p.getOrCreateSession(groupEvent("x", "a", "x", "g1"))
	unconsolidated := env.p.getOrCreateSession(groupEvent("x", "a", "x", "g2"))
	env.p.appendUserTurn(unconsolidated, "hello", nil, "a", "A", "e", now)
	consolidated.mu.Lock()
	consolidated.lastActivity = now.Add(-20 * time.Minute)
	consolidated.mu.Unlock()
	unconsolidated.mu.Lock()
	unconsolidated.lastActivity = now.Add(-20 * time.Minute)
	unconsolidated.mu.Unlock()

	if _, err := env.p.runCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if env.p.getSession(consolidated.key) != nil {
		t.Fatal("consolidated idle session not removed")
	}
	if env.p.getSession(unconsolidated.key) == nil {
		t.Fatal("unconsolidated session removed by normal TTL")
	}

	old := env.p.getOrCreateSession(groupEvent("x", "b", "x", "g3"))
	env.p.appendUserTurn(old, "hello", nil, "b", "B", "e", now)
	old.mu.Lock()
	old.lastActivity = now.Add(-2 * time.Hour)
	old.mu.Unlock()
	if _, err := env.p.runCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if env.p.getSession(old.key) != nil {
		t.Fatal("hard TTL did not remove unconsolidated session")
	}
	waitLog(t, env.logs, "history_dropped_unconsolidated")
}

func TestCleanupSkipsActiveAndPreservesOverride(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"history_ttl":          "10m",
		"history_max_sessions": 100,
		"random_enabled":       false,
		"memory_enabled":       false,
		"rag_enabled":          false,
	})
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	env.p.now = func() time.Time { return now }
	s := env.p.getOrCreateSession(groupEvent("x", "a", "x", "g1"))
	s.mu.Lock()
	s.inflight = true
	s.lastActivity = now.Add(-time.Hour)
	s.mu.Unlock()
	env.p.setDisabled(s, true)
	waitStored(t, env.store, s.storageKey)
	if _, err := env.p.runCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if env.p.getSession(s.key) == nil {
		t.Fatal("in-flight session must not be removed")
	}
	s.mu.Lock()
	s.inflight = false
	s.lastActivity = now.Add(-time.Hour)
	s.mu.Unlock()
	if _, err := env.p.runCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if env.p.getSession(s.key) != nil {
		t.Fatal("idle session should be removed")
	}
	env.store.mu.Lock()
	_, ok := env.store.kv[s.storageKey]
	env.store.mu.Unlock()
	if !ok {
		t.Fatal("session override must be preserved")
	}
}

func TestCleanupLRUPreference(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"history_max_sessions": 1,
		"random_enabled":       false,
		"history_ttl":          "0s",
	})
	now := env.p.now()
	first := env.p.getOrCreateSession(groupEvent("x", "a", "x", "g1"))
	second := env.p.getOrCreateSession(groupEvent("x", "b", "x", "g2"))
	env.p.appendUserTurn(first, "u1", nil, "a", "A", "e1", now)
	env.p.appendUserTurn(second, "u2", nil, "b", "B", "e2", now)
	first.mu.Lock()
	first.consolidatedSeq = first.seq
	first.lastActivity = now.Add(-time.Minute)
	first.mu.Unlock()
	second.mu.Lock()
	second.lastActivity = now
	second.mu.Unlock()
	if _, err := env.p.runCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if env.p.getSession(first.key) != nil {
		t.Fatal("consolidated session should be evicted first")
	}
	if env.p.getSession(second.key) == nil {
		t.Fatal("second session should be evicted after first")
	}
	if got := len(env.p.sessions); got != 1 {
		t.Fatalf("sessions=%d", got)
	}
}

func TestRemoveSessionInvalidatesTimer(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	s := env.p.getOrCreateSession(groupEvent("x", "a", "x", "g"))
	fired := false
	s.mu.Lock()
	s.timer = env.timers.afterFunc(time.Minute, func() { fired = true })
	epoch := s.epoch
	s.mu.Unlock()
	env.p.removeSession(s.key)
	s.mu.Lock()
	if s.epoch != epoch+1 {
		t.Fatalf("epoch=%d want %d", s.epoch, epoch+1)
	}
	s.mu.Unlock()
	env.timers.fireLast()
	if fired {
		t.Fatal("removed session timer fired")
	}
}
