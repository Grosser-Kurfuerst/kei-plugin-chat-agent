package chatagent

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

func TestKnowledgeOrphanMismatchKept(t *testing.T) {
	store := newSeedStore(t)
	seedMeta(t, store, "test-embed", 4)
	seedDoc(t, store, "dddddddddddddddddddddddddddddddd", "cccccccccccccccccccccccccccccccc")
	seedChunk(t, store, "cccccccccccccccccccccccccccccccc", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "mismatch", "test-embed", 0, []float32{1, 0, 0, 0})
	_ = newTestEnvWithStore(t, store, map[string]any{"random_enabled": false})
	if _, ok := store.row(tableName, chunkRowKey("cccccccccccccccccccccccccccccccc")); !ok {
		t.Fatal("referenced chunk with mismatched doc_id was deleted")
	}
}

func TestLoadCapacityEvictsExcessMemories(t *testing.T) {
	store := newSeedStore(t)
	seedMeta(t, store, "test-embed", 4)
	seedMemory(t, store, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "channel", "g", "m1", "test-embed", []float32{1, 0, 0, 0}, 0.5)
	seedMemory(t, store, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "channel", "g", "m2", "test-embed", []float32{0, 1, 0, 0}, 0.5)
	env := newTestEnvWithStore(t, store, map[string]any{
		"random_enabled":     false,
		"vector_max_records": 1,
	})
	if got := env.p.idx.memoryRows(); got != 1 {
		t.Fatalf("memory rows=%d, want 1", got)
	}
	if count := len(store.rows(tableName)); count != 2 { // one m row + meta
		t.Fatalf("table rows=%d, want 2", count)
	}
}

func TestMemoryResetDeletesMalformedRows(t *testing.T) {
	store := newSeedStore(t)
	seedMeta(t, store, "test-embed", 4)
	seedRow(t, store, "m:ffffffffffffffffffffffffffffffff", []byte(`{`))
	env := newTestEnvWithStore(t, store, map[string]any{"random_enabled": false})
	if got := env.p.idx.memoryRows(); got != 1 {
		t.Fatalf("memory rows=%d, want 1", got)
	}
	out := runCommand(t, env.p, commandEvent("chat", "memory reset confirm", "admin", "g"))
	if out != "memory: 已接受" {
		t.Fatalf("reset=%q", out)
	}
	if len(env.bot.waitSends(1, time.Second)) != 1 {
		t.Fatal("reset completion not sent")
	}
	for key := range store.rows(tableName) {
		if strings.HasPrefix(key, "m:") {
			t.Fatalf("malformed memory row remains: %s", key)
		}
	}
}

func TestMemoryCapacityDoesNotEvictWhenChunksFill(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled":     false,
		"vector_max_records": 2,
	})
	vec := normForTest(t, []float32{1, 0, 0, 0})
	m := &memoryEntry{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Scope: "channel", ScopeKey: "g",
		Kind: "fact", Text: "keep", Vector: vec, Dims: 4, Model: "test-embed"}
	env.p.idx.upsertMemory(m)
	putChunkForTest(env.p.idx, &chunkEntry{ID: "cccccccccccccccccccccccccccccccc", DocID: "d1", Text: "c1", Vector: vec, Dims: 4, Model: "test-embed"})
	putChunkForTest(env.p.idx, &chunkEntry{ID: "dddddddddddddddddddddddddddddddd", DocID: "d2", Text: "c2", Vector: vec, Dims: 4, Model: "test-embed"})
	if _, err := env.p.ensureMemoryCapacity(context.Background(), 1); err == nil {
		t.Fatal("expected vector_full")
	}
	if _, ok := env.p.idx.memoryByID(m.ID); !ok {
		t.Fatal("memory was evicted even though chunks occupy the capacity")
	}
}

func TestSelfIDsArePlatformScoped(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled": false,
		"self_ids":       []string{"telegram:bot"},
	})
	ev := atEvent("e", "u", "hello", "g", "someone-else")
	_ = env.p.handleGroupMessage(context.Background(), ev, bot.NewNoopReply())
	waitLog(t, env.logs, "reason=not_addressed")
	if len(env.bot.sendsSnapshot()) != 0 {
		t.Fatal("mention of another platform ID was treated as addressed")
	}
}

func TestStaleTimerCallbackKeepsNewTimer(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	env.chat.replies = []string{"ok"}
	s := env.p.getOrCreateSession(groupEvent("seed", "u", "seed", "g"))
	s.mu.Lock()
	now := env.p.now()
	turn := Turn{Seq: s.seq, UserID: "u", UserName: "U", Text: "hello"}
	s.seq++
	env.p.startBatchLocked(s, turn, false, now)
	seq := s.timerSeq
	epoch := s.epoch
	s.mu.Unlock()

	env.p.onBatchTimer(s, epoch, seq-1)
	s.mu.Lock()
	if s.timer == nil || len(s.pending) != 1 {
		s.mu.Unlock()
		t.Fatal("stale callback cleared the current timer or pending batch")
	}
	s.mu.Unlock()

	env.timers.fireLast()
	if len(env.bot.waitSends(1, time.Second)) != 1 {
		t.Fatal("current timer did not fire")
	}
}

func TestVisionCustomDialStillChecksPrivateAddress(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()
	env := newTestEnv(t, map[string]any{"random_enabled": false, "vision_enabled": true})
	env.p.visionClient = newVisionClient(&http.Client{Transport: &http.Transport{
		DialContext: (&net.Dialer{Timeout: time.Second}).DialContext,
	}})
	if _, reason := env.p.fetchImage(context.Background(), srv.URL+"/a.png"); reason != "fetch_error" {
		t.Fatalf("reason=%s", reason)
	}
	if hits != 0 {
		t.Fatal("private image host reached the server")
	}
}

func TestKnowledgeFetcherRejectsPrivateHostname(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("secret"))
	}))
	defer srv.Close()
	env := newTestEnv(t, map[string]any{
		"random_enabled":              false,
		"knowledge_url_allow_private": false,
	})
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.p.fetcher.Fetch(context.Background(), "http://localhost:"+port+"/x"); err == nil {
		t.Fatal("private hostname was accepted")
	}
	if hits != 0 {
		t.Fatal("private knowledge host reached the server")
	}
}

func TestStopBeforeStartIsTerminal(t *testing.T) {
	store := newMemStore()
	p := &Plugin{chat: &fixedChat{}, embedder: &fixedEmbedder{dims: 4}}
	pc := bot.PluginContext{
		Name: pluginName, Config: bot.NewConfig(testRaw(nil)), Logger: testLogger(),
		Storage: store, Tables: store, HTTPClient: &http.Client{}, Bot: &fakeBot{store: store},
	}
	ctx := bot.WithPluginContext(context.Background(), pc)
	if err := p.Setup(ctx, &noopRegistrar{}); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(ctx); err == nil || !strings.Contains(err.Error(), "插件已关闭") {
		t.Fatalf("Start after Stop = %v", err)
	}
}

func TestNoWorkAfterStop(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	env.chat.replies = []string{"must not send"}
	if err := env.p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = env.p.handleGroupMessage(context.Background(), atEvent("e", "u", "hello", "g", "bot"), bot.NewNoopReply())
	_ = env.p.handleCommand(context.Background(), commandEvent("chat", "status", "admin", "g"), bot.NewNoopReply())
	env.timers.fireLast()
	if len(env.bot.sendsSnapshot()) != 0 {
		t.Fatal("work started after Stop")
	}
}

func TestChatRejectsRedirect(t *testing.T) {
	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits++
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/chat/completions", http.StatusFound)
	}))
	defer redirect.Close()
	cfg := mustConfig(t, map[string]any{
		"llm_base_url": redirect.URL,
		"llm_model":    "m",
	})
	c := newChatCompleter(cfg, redirect.Client(), testLogger())
	if _, err := c.Complete(context.Background(), ChatRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: "x"}}}); err == nil {
		t.Fatal("redirect was followed")
	}
	if targetHits != 0 {
		t.Fatal("redirect target was reached")
	}
}
