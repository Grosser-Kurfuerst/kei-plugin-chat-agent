package chatagent

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func extractionChat(t *testing.T, payload string) func(ChatRequest) (ChatResult, error) {
	return func(req ChatRequest) (ChatResult, error) {
		if len(req.Messages) != 2 || req.Messages[0].Role != "system" {
			t.Errorf("messages=%+v", req.Messages)
		}
		if req.Messages[0].Content != memoryExtractPrompt {
			t.Errorf("extraction prompt mismatch")
		}
		if req.Temperature != memoryExtractTemperature || req.MaxTokens != memoryExtractMaxTokens {
			t.Errorf("temperature=%v max=%d", req.Temperature, req.MaxTokens)
		}
		return ChatResult{Content: payload}, nil
	}
}

func addSessionTurns(env *testEnv, channel, user string, ids []string, texts []string) *sessionState {
	ev := groupEvent("seed", user, "seed", channel)
	s := env.p.getOrCreateSession(ev)
	for i := range texts {
		env.p.appendUserTurn(s, texts[i], nil, user, user, ids[i], env.p.now())
	}
	return s
}

func TestParseExtractionResponse(t *testing.T) {
	valid, invalid, err := parseExtractionResponse(`{"memories":[{"text":"用户喜欢 Go 语言","kind":"preference","importance":1.7},{"text":"x","kind":"fact","importance":0.5}]}`, 8)
	if err != nil || len(valid) != 1 || invalid != 1 {
		t.Fatalf("valid=%v invalid=%d err=%v", valid, invalid, err)
	}
	if valid[0].Importance != 1 || valid[0].Kind != "preference" {
		t.Fatalf("entry=%+v", valid[0])
	}
	empty, invalid, err := parseExtractionResponse(`{"memories":[]}`, 8)
	if err != nil || len(empty) != 0 || invalid != 0 {
		t.Fatalf("empty valid=%v invalid=%d err=%v", empty, invalid, err)
	}
	_, _, err = parseExtractionResponse(`{"memories":[{"text":"x","kind":"fact","importance":0.1}]}`, 8)
	if err == nil {
		t.Fatal("all invalid must fail")
	}
	for _, bad := range []string{`[]`, `{}`, `{"memories":null}`, `{"memories":[1]}`, `not json`} {
		if _, _, err := parseExtractionResponse(bad, 8); err == nil {
			t.Fatalf("payload %q accepted", bad)
		}
	}
	limited, _, err := parseExtractionResponse(`{"memories":[{"text":"aaaa","kind":"fact","importance":0.1},{"text":"bbbb","kind":"fact","importance":0.2}]}`, 1)
	if err != nil || len(limited) != 1 {
		t.Fatalf("limit valid=%v err=%v", limited, err)
	}
	longOK := strings.Repeat("x", 500)
	tooLong := strings.Repeat("x", 501)
	valid, invalid, err = parseExtractionResponse(`{"memories":[{"text":"`+longOK+`","kind":"fact","importance":0.1},{"text":"`+tooLong+`","kind":"fact","importance":0.1}]}`, 8)
	if err != nil || len(valid) != 1 || invalid != 1 {
		t.Fatalf("length valid=%d invalid=%d err=%v", len(valid), invalid, err)
	}
}

func TestConsolidateSuccessAndWatermark(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled":              false,
		"memory_extract_min_messages": 2,
		"memory_dedupe_threshold":     1.0,
	})
	s := addSessionTurns(env, "g", "u", []string{"e1", "e2"}, []string{"我住在杭州", "我喜欢 Go"})
	env.chat.fn = extractionChat(t, `{"memories":[{"text":"用户住在杭州","kind":"fact","importance":0.6},{"text":"用户喜欢 Go","kind":"preference","importance":0.8}]}`)
	summary, err := env.p.runConsolidate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary != "sessions_done=1 sessions_failed=0 memories_added=2 memories_merged=0" {
		t.Fatalf("summary=%q", summary)
	}
	s.mu.Lock()
	watermark := s.consolidatedSeq
	s.mu.Unlock()
	if watermark != 2 {
		t.Fatalf("watermark=%d", watermark)
	}
	rows := env.store.rows(tableName)
	count := 0
	for key, raw := range rows {
		if !strings.HasPrefix(key, "m:") {
			continue
		}
		count++
		var rec memoryRecordJSON
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Scope != "channel" || rec.ScopeKey != "g" || rec.Model != "test-embed" || rec.Dims != 4 {
			t.Fatalf("rec=%+v", rec)
		}
		if len(rec.Sources) != 2 {
			t.Fatalf("sources=%v", rec.Sources)
		}
	}
	if count != 2 {
		t.Fatalf("memory rows=%d", count)
	}
}

func TestConsolidateFailureKeepsWatermark(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled":              false,
		"memory_extract_min_messages": 1,
	})
	s := addSessionTurns(env, "g", "u", []string{"e1"}, []string{"hello"})
	env.chat.fn = extractionChat(t, `not-json`)
	summary, err := env.p.runConsolidate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "sessions_failed=1") {
		t.Fatalf("summary=%q", summary)
	}
	s.mu.Lock()
	watermark := s.consolidatedSeq
	s.mu.Unlock()
	if watermark != 0 {
		t.Fatalf("watermark=%d", watermark)
	}
	for key := range env.store.rows(tableName) {
		if strings.HasPrefix(key, "m:") {
			t.Fatal("failed consolidation wrote memory")
		}
	}
}

func TestConsolidateEmbeddingFailureKeepsWatermark(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled":              false,
		"memory_extract_min_messages": 1,
	})
	s := addSessionTurns(env, "g", "u", []string{"e1"}, []string{"hello"})
	env.chat.fn = extractionChat(t, `{"memories":[{"text":"用户喜欢 Go","kind":"preference","importance":0.8}]}`)
	env.emb.err = errTest
	summary, err := env.p.runConsolidate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "sessions_failed=1") {
		t.Fatalf("summary=%q", summary)
	}
	s.mu.Lock()
	watermark := s.consolidatedSeq
	s.mu.Unlock()
	if watermark != 0 {
		t.Fatalf("watermark=%d", watermark)
	}
	for key := range env.store.rows(tableName) {
		if strings.HasPrefix(key, "m:") {
			t.Fatal("memory written despite embedding failure")
		}
	}
}

func TestConsolidateMinimumAndExtractZero(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled":              false,
		"memory_extract_min_messages": 2,
	})
	s := addSessionTurns(env, "g", "u", []string{"e1"}, []string{"hello"})
	if _, err := env.p.runConsolidate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if env.chat.requestCount() != 0 {
		t.Fatal("LLM called below min messages")
	}
	s.mu.Lock()
	if s.consolidatedSeq != 0 {
		t.Fatalf("watermark=%d", s.consolidatedSeq)
	}
	s.mu.Unlock()

	env2 := newTestEnv(t, map[string]any{
		"random_enabled":              false,
		"memory_extract_max":          0,
		"memory_extract_min_messages": 1,
	})
	s2 := addSessionTurns(env2, "g", "u", []string{"e1", "e2"}, []string{"a1", "a2"})
	if _, err := env2.p.runConsolidate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if env2.chat.requestCount() != 0 {
		t.Fatal("LLM called with extract_max=0")
	}
	s2.mu.Lock()
	if s2.consolidatedSeq != 2 {
		t.Fatalf("watermark=%d", s2.consolidatedSeq)
	}
	s2.mu.Unlock()
}

func TestConsolidateLostRangeAdvances(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	s := env.p.getOrCreateSession(groupEvent("x", "u", "x", "g"))
	s.mu.Lock()
	s.seq = 5
	s.lostSeq = 5
	s.mu.Unlock()
	if _, err := env.p.runConsolidate(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	watermark := s.consolidatedSeq
	s.mu.Unlock()
	if watermark != 5 {
		t.Fatalf("watermark=%d", watermark)
	}
}

func TestMemoryMergeFields(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled":              false,
		"memory_extract_min_messages": 1,
		"memory_dedupe_threshold":     0.92,
	})
	vec := normForTest(t, []float32{1, 0, 0, 0})
	env.emb.vectors = map[string][]float32{"用户喜欢 Go": vec, "用户喜欢 Go 语言": vec}
	s := addSessionTurns(env, "g", "u", []string{"e1"}, []string{"a"})
	env.chat.replies = []string{`{"memories":[{"text":"用户喜欢 Go","kind":"preference","importance":0.5}]}`}
	if _, err := env.p.runConsolidate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var firstID string
	for key := range env.store.rows(tableName) {
		if strings.HasPrefix(key, "m:") {
			firstID = strings.TrimPrefix(key, "m:")
		}
	}
	if firstID == "" {
		t.Fatal("no memory inserted")
	}
	env.p.appendUserTurn(s, "b", nil, "u", "u", "e2", env.p.now())
	env.chat.replies = []string{`{"memories":[{"text":"用户喜欢 Go 语言","kind":"preference","importance":0.9}]}`}
	env.emb.vectors["用户喜欢 Go 语言"] = vec
	if _, err := env.p.runConsolidate(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := env.store.rows(tableName)
	countM := 0
	for key := range rows {
		if strings.HasPrefix(key, "m:") {
			countM++
		}
	}
	if countM != 1 {
		t.Fatalf("merge created new rows: %d", countM)
	}
	var rec memoryRecordJSON
	if err := json.Unmarshal(rows[memoryRowKey(firstID)], &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Text != "用户喜欢 Go 语言" || rec.Importance != 0.9 || rec.UseCount != 1 {
		t.Fatalf("rec=%+v", rec)
	}
	if strings.Join(rec.Sources, ",") != "e1,e2" {
		t.Fatalf("sources=%v", rec.Sources)
	}
}

func TestMemoryDecayAndTTL(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled":                    false,
		"memory_importance_decay_half_life": "1h",
		"memory_min_importance":             0.15,
		"memory_ttl":                        "1h",
	})
	now := time.Now().UTC()
	env.p.now = func() time.Time { return now }
	mk := func(id string, importance float64, updated, used time.Time) *memoryEntry {
		vec := normForTest(t, []float32{1, 0, 0, 0})
		entry := &memoryEntry{ID: id, Scope: "channel", ScopeKey: "g", Kind: "fact", Text: "t-" + id,
			Importance: importance, Vector: vec, Dims: 4, Model: "test-embed",
			CreatedAt: updated, UpdatedAt: updated, LastUsedAt: used}
		seedRow(t, env.store, memoryRowKey(id), marshalJSON(memoryRecordOf(entry)))
		env.p.idx.upsertMemory(entry)
		return entry
	}
	decayed := mk("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 0.5, now.Add(-2*time.Hour), now)
	expired := mk("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 1.0, now, now.Add(-2*time.Hour))
	kept := mk("cccccccccccccccccccccccccccccccc", 0.9, now, now)
	expiredCount, _ := env.p.decayMemories(context.Background())
	if expiredCount != 2 {
		t.Fatalf("expired=%d", expiredCount)
	}
	if _, ok := env.p.idx.memoryByID(decayed.ID); ok {
		t.Fatal("decayed memory kept")
	}
	if _, ok := env.p.idx.memoryByID(expired.ID); ok {
		t.Fatal("TTL memory kept")
	}
	if _, ok := env.p.idx.memoryByID(kept.ID); !ok {
		t.Fatal("valid memory removed")
	}
	if _, ok := env.store.row(tableName, memoryRowKey(decayed.ID)); ok {
		t.Fatal("decayed row not deleted")
	}
}

func TestMemoryReinforcementPersists(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled":          false,
		"memory_dedupe_threshold": 0.92,
	})
	vec := normForTest(t, []float32{1, 0, 0, 0})
	env.emb.vectors = map[string][]float32{"query": vec}
	entry := &memoryEntry{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Scope: "channel", ScopeKey: "g",
		Kind: "fact", Text: "hello", Importance: 0.5, Vector: vec, Dims: 4, Model: "test-embed",
		CreatedAt: env.p.now(), UpdatedAt: env.p.now(), LastUsedAt: env.p.now()}
	seedRow(t, env.store, memoryRowKey(entry.ID), marshalJSON(memoryRecordOf(entry)))
	env.p.idx.upsertMemory(entry)
	s := env.p.getOrCreateSession(groupEvent("x", "u", "x", "g"))
	res := env.p.retrieveRAG(context.Background(), s, ragBatch("query"), "query")
	if res.Status != "ok" || res.MemoryBlock == "" {
		t.Fatalf("rag=%+v", res)
	}
	got, _ := env.p.idx.memoryByID(entry.ID)
	if got.UseCount != 1 || math.Abs(got.Importance-0.52) > 1e-9 || !got.Dirty {
		t.Fatalf("reinforced=%+v", got)
	}
	env.p.flushReinforcements(context.Background())
	raw, _ := env.store.row(tableName, memoryRowKey(entry.ID))
	var rec memoryRecordJSON
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.UseCount != 1 || math.Abs(rec.Importance-0.52) > 1e-9 {
		t.Fatalf("persisted=%+v", rec)
	}
	if got, _ := env.p.idx.memoryByID(entry.ID); got.Dirty {
		t.Fatal("dirty flag not cleared")
	}
}

func TestMemoryCommands(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	out := runCommand(t, env.p, commandEvent("chat", "memory add hello world", "admin", "g"))
	if out != "memory: 已接受" {
		t.Fatalf("add out=%q", out)
	}
	sn := env.bot.waitSends(1, time.Second)
	if len(sn) != 1 || !strings.HasPrefix(messageText(sn[0].msg), "memory: 完成 ") {
		t.Fatalf("completion=%v", sn)
	}
	id := strings.TrimPrefix(messageText(sn[0].msg), "memory: 完成 ")
	status := runCommand(t, env.p, commandEvent("chat", "memory status", "admin", "g"))
	if !strings.Contains(status, "records=1") {
		t.Fatalf("status=%q", status)
	}
	search := runCommand(t, env.p, commandEvent("chat", "memory search hello world", "admin", "g"))
	if !strings.HasPrefix(search, "memory search: 1 条结果") || !strings.Contains(search, id) {
		t.Fatalf("search=%q", search)
	}
	out = runCommand(t, env.p, commandEvent("chat", "memory del "+id, "admin", "g"))
	if out != "memory: 已接受" {
		t.Fatalf("del out=%q", out)
	}
	sn = env.bot.waitSends(2, time.Second)
	if len(sn) < 2 || messageText(sn[1].msg) != "memory: 完成 "+id {
		t.Fatalf("del completion=%v", sn)
	}
	status = runCommand(t, env.p, commandEvent("chat", "memory status", "admin", "g"))
	if !strings.Contains(status, "records=0") {
		t.Fatalf("status=%q", status)
	}
}

func TestMemoryCommandDisabledAndBusy(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled": false,
		"memory_enabled": false,
		"rag_enabled":    true,
	})
	if got := runCommand(t, env.p, commandEvent("chat", "memory status", "admin", "g")); got != "chat-agent: 记忆功能未启用" {
		t.Fatalf("got=%q", got)
	}

	env2 := newTestEnv(t, map[string]any{"random_enabled": false})
	env2.p.idx.mu.Lock()
	env2.p.idx.state = vectorStateLoading
	env2.p.idx.mu.Unlock()
	if got := runCommand(t, env2.p, commandEvent("chat", "memory status", "admin", "g")); got != notReadyLine {
		t.Fatalf("got=%q", got)
	}
	env2.p.idx.mu.Lock()
	env2.p.idx.state = vectorStateReady
	env2.p.idx.mu.Unlock()

	env2.p.jobs.mu.Lock()
	env2.p.jobs.memoryCmdRunning = true
	env2.p.jobs.mu.Unlock()
	if got := runCommand(t, env2.p, commandEvent("chat", "memory add hello", "admin", "g")); got != "chat-agent: 忙碌: memory" {
		t.Fatalf("busy=%q", got)
	}
	env2.p.jobs.mu.Lock()
	env2.p.jobs.memoryCmdRunning = false
	env2.p.jobs.mu.Unlock()
	if got := runCommand(t, env2.p, commandEvent("chat", "memory del nope", "admin", "g")); got != "chat-agent: 未找到: nope" {
		t.Fatalf("not found=%q", got)
	}
}

func TestImageMemoryExtraction(t *testing.T) {
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("fake-png-body"))
	}))
	defer imgSrv.Close()

	env := newTestEnv(t, map[string]any{
		"random_enabled":              false,
		"vision_enabled":              true,
		"memory_extract_min_messages": 1,
	})
	env.p.visionClient = imgSrv.Client()
	ev := groupEvent("seed", "u", "seed", "g")
	s := env.p.getOrCreateSession(ev)
	env.p.appendUserTurn(s, "[图片]", []string{imgSrv.URL + "/a.png"}, "u", "u", "e1", env.p.now())
	var seenImage bool
	env.chat.fn = func(req ChatRequest) (ChatResult, error) {
		parts, ok := req.Messages[1].Content.([]ContentPart)
		if !ok {
			t.Errorf("content type %T", req.Messages[1].Content)
			return ChatResult{Content: `{"memories":[]}`}, nil
		}
		for _, p := range parts {
			if p.Type == "image_url" && p.ImageURL != nil && strings.HasPrefix(p.ImageURL.URL, "data:image/png;base64,") {
				seenImage = true
			}
		}
		return ChatResult{Content: `{"memories":[{"text":"图片里是一页部署说明","kind":"fact","importance":0.7}]}`}, nil
	}
	if _, err := env.p.runConsolidate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !seenImage {
		t.Fatal("image part not sent to extraction")
	}
	found := false
	for key, raw := range env.store.rows(tableName) {
		if !strings.HasPrefix(key, "m:") {
			continue
		}
		var rec memoryRecordJSON
		_ = json.Unmarshal(raw, &rec)
		if rec.Text == "图片里是一页部署说明" {
			found = true
			if strings.Contains(rec.Text, "http") || strings.Contains(string(raw), "fake-png-body") {
				t.Fatal("image data persisted in memory row")
			}
		}
	}
	if !found {
		t.Fatal("image memory row missing")
	}
}

func TestImageMemoryAllFailures(t *testing.T) {
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer imgSrv.Close()
	env := newTestEnv(t, map[string]any{
		"random_enabled":              false,
		"vision_enabled":              true,
		"memory_extract_min_messages": 1,
	})
	env.p.visionClient = imgSrv.Client()
	s := env.p.getOrCreateSession(groupEvent("seed", "u", "seed", "g"))
	env.p.appendUserTurn(s, "[图片]", []string{imgSrv.URL + "/bad"}, "u", "u", "e1", env.p.now())
	if _, err := env.p.runConsolidate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if env.chat.requestCount() != 0 {
		t.Fatal("LLM called when all images failed")
	}
	s.mu.Lock()
	watermark := s.consolidatedSeq
	s.mu.Unlock()
	if watermark != 1 {
		t.Fatalf("watermark=%d", watermark)
	}
}

func TestImageMemoryBatchLimit(t *testing.T) {
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("fake"))
	}))
	defer imgSrv.Close()
	env := newTestEnv(t, map[string]any{
		"random_enabled":                  false,
		"vision_enabled":                  true,
		"memory_extract_min_messages":     1,
		"memory_consolidate_max_messages": 50,
	})
	env.p.visionClient = imgSrv.Client()
	s := env.p.getOrCreateSession(groupEvent("seed", "u", "seed", "g"))
	for i := 0; i < 5; i++ {
		env.p.appendUserTurn(s, "[图片]", []string{imgSrv.URL + "/" + itoa(i)}, "u", "u", "e"+itoa(i), env.p.now())
	}
	var mu sync.Mutex
	var counts []int
	env.chat.fn = func(req ChatRequest) (ChatResult, error) {
		n := 0
		if parts, ok := req.Messages[1].Content.([]ContentPart); ok {
			for _, p := range parts {
				if p.Type == "image_url" {
					n++
				}
			}
		}
		mu.Lock()
		counts = append(counts, n)
		mu.Unlock()
		return ChatResult{Content: `{"memories":[]}`}, nil
	}
	if _, err := env.p.runConsolidate(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(counts) < 2 {
		t.Fatalf("batches=%v", counts)
	}
	for _, n := range counts {
		if n > visionMaxImagesPerRequest {
			t.Fatalf("batch images=%v", counts)
		}
	}
}

func TestMemoryStatusCountsPending(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	s := addSessionTurns(env, "g", "u", []string{"e1"}, []string{"hello"})
	status := runCommand(t, env.p, commandEvent("chat", "memory status", "admin", "g"))
	if !strings.Contains(status, "pending_sessions=1") {
		t.Fatalf("status=%q", status)
	}
	s.mu.Lock()
	s.consolidatedSeq = s.seq
	s.mu.Unlock()
	status = runCommand(t, env.p, commandEvent("chat", "memory status", "admin", "g"))
	if !strings.Contains(status, "pending_sessions=0") {
		t.Fatalf("status=%q", status)
	}
}
