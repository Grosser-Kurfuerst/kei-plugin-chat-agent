package chatagent

import (
	"context"
	"strings"
	"testing"
	"time"
)

func ragEnv(t *testing.T, overrides map[string]any) *testEnv {
	t.Helper()
	raw := map[string]any{"random_enabled": false, "embedding_dimensions": 4}
	for k, v := range overrides {
		raw[k] = v
	}
	return newTestEnv(t, raw)
}

func addRAGMemory(t *testing.T, env *testEnv, id, text, kind string, importance float64, vec []float32) *memoryEntry {
	t.Helper()
	entry := &memoryEntry{
		ID: id, Scope: "channel", ScopeKey: "g", Kind: kind, Text: text,
		Importance: importance, Vector: normForTest(t, vec), Dims: len(vec),
		Model: "test-embed", CreatedAt: env.p.now(), UpdatedAt: env.p.now(), LastUsedAt: env.p.now(),
	}
	env.p.idx.upsertMemory(entry)
	return entry
}

func addRAGChunk(t *testing.T, env *testEnv, docID, title, chunkID, text string, index int, vec []float32) {
	t.Helper()
	chunk := &chunkEntry{
		ID: chunkID, DocID: docID, Index: index, Text: text, Vector: normForTest(t, vec),
		Dims: len(vec), Model: "test-embed", CreatedAt: env.p.now(),
	}
	doc := &docEntry{ID: docID, Source: "https://x", Title: title, ChunkIDs: []string{chunkID},
		CreatedAt: env.p.now(), UpdatedAt: env.p.now()}
	env.p.idx.replaceDoc(doc, []*chunkEntry{chunk}, nil)
}

func ragBatch(text string) []Turn {
	return []Turn{{Seq: 0, UserID: "u", UserName: "U", Text: text}}
}

func TestRAGInjectionFormat(t *testing.T) {
	env := ragEnv(t, nil)
	vec := []float32{1, 0, 0, 0}
	env.emb.vectors = map[string][]float32{"query": vec}
	addRAGMemory(t, env, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "用户偏好简短回答", "preference", 0.8, vec)
	addRAGChunk(t, env, "dddddddddddddddddddddddddddddddd", "部署说明", "cccccccccccccccccccccccccccccccc", "持久化需要 sqlite 或 mysql 后端。", 0, vec)
	s := env.p.getOrCreateSession(groupEvent("x", "u", "x", "g"))
	res := env.p.retrieveRAG(context.Background(), s, ragBatch("query"), "query")
	if res.Status != "ok" {
		t.Fatalf("status=%s", res.Status)
	}
	wantMemory := "<long_term_memory>\n- (preference, 0.80) 用户偏好简短回答\n</long_term_memory>"
	if res.MemoryBlock != wantMemory {
		t.Fatalf("memory block=%q", res.MemoryBlock)
	}
	wantKnowledge := "<knowledge_base>\n- [部署说明#0] 持久化需要 sqlite 或 mysql 后端。\n</knowledge_base>"
	if res.KnowledgeBlock != wantKnowledge {
		t.Fatalf("knowledge block=%q", res.KnowledgeBlock)
	}
	sys := systemContent("SYS", res.MemoryBlock, res.KnowledgeBlock)
	if !strings.Contains(sys, wantMemory) || !strings.Contains(sys, wantKnowledge) {
		t.Fatalf("sys=%q", sys)
	}
}

func TestRAGStatuses(t *testing.T) {
	t.Run("disabled_flags", func(t *testing.T) {
		env := ragEnv(t, map[string]any{"memory_enabled": false, "rag_enabled": false})
		s := env.p.getOrCreateSession(groupEvent("x", "u", "x", "g"))
		if res := env.p.retrieveRAG(context.Background(), s, ragBatch("q"), "q"); res.Status != "disabled" {
			t.Fatalf("status=%s", res.Status)
		}
	})
	t.Run("disabled_budget", func(t *testing.T) {
		env := ragEnv(t, map[string]any{"rag_context_max_chars": 0})
		s := env.p.getOrCreateSession(groupEvent("x", "u", "x", "g"))
		if res := env.p.retrieveRAG(context.Background(), s, ragBatch("q"), "q"); res.Status != "disabled" {
			t.Fatalf("status=%s", res.Status)
		}
	})
	t.Run("disabled_topk", func(t *testing.T) {
		env := ragEnv(t, map[string]any{"memory_top_k": 0, "knowledge_top_k": 0})
		s := env.p.getOrCreateSession(groupEvent("x", "u", "x", "g"))
		if res := env.p.retrieveRAG(context.Background(), s, ragBatch("q"), "q"); res.Status != "disabled" {
			t.Fatalf("status=%s", res.Status)
		}
	})
	t.Run("loading", func(t *testing.T) {
		old := vectorReadyTimeout
		vectorReadyTimeout = 5 * time.Millisecond
		defer func() { vectorReadyTimeout = old }()
		env := ragEnv(t, nil)
		env.p.idx.mu.Lock()
		env.p.idx.state = vectorStateLoading
		env.p.idx.mu.Unlock()
		s := env.p.getOrCreateSession(groupEvent("x", "u", "x", "g"))
		if res := env.p.retrieveRAG(context.Background(), s, ragBatch("q"), "q"); res.Status != "loading" {
			t.Fatalf("status=%s", res.Status)
		}
	})
	t.Run("error", func(t *testing.T) {
		env := ragEnv(t, nil)
		env.emb.err = errTest
		s := env.p.getOrCreateSession(groupEvent("x", "u", "x", "g"))
		if res := env.p.retrieveRAG(context.Background(), s, ragBatch("q"), "q"); res.Status != "error" {
			t.Fatalf("status=%s", res.Status)
		}
	})
	t.Run("empty", func(t *testing.T) {
		env := ragEnv(t, nil)
		s := env.p.getOrCreateSession(groupEvent("x", "u", "x", "g"))
		if res := env.p.retrieveRAG(context.Background(), s, ragBatch("q"), "q"); res.Status != "empty" {
			t.Fatalf("status=%s", res.Status)
		}
	})
	t.Run("below_threshold", func(t *testing.T) {
		env := ragEnv(t, map[string]any{"memory_min_score": 0.99, "knowledge_min_score": 0.99})
		env.emb.vectors = map[string][]float32{"q": {1, 0, 0, 0}}
		addRAGMemory(t, env, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "m", "fact", 0.5, []float32{0, 1, 0, 0})
		s := env.p.getOrCreateSession(groupEvent("x", "u", "x", "g"))
		if res := env.p.retrieveRAG(context.Background(), s, ragBatch("q"), "q"); res.Status != "empty" {
			t.Fatalf("status=%s", res.Status)
		}
	})
}

func TestRAGScopes(t *testing.T) {
	env := ragEnv(t, nil)
	vec := []float32{1, 0, 0, 0}
	env.emb.vectors = map[string][]float32{"q": vec}
	addRAGMemory(t, env, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "channel memory", "fact", 0.5, vec)
	other := &memoryEntry{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Scope: "user", ScopeKey: "u",
		Kind: "fact", Text: "user u memory", Importance: 0.5, Vector: normForTest(t, vec), Dims: 4, Model: "test-embed"}
	env.p.idx.upsertMemory(other)
	stranger := &memoryEntry{ID: "cccccccccccccccccccccccccccccccc", Scope: "user", ScopeKey: "other",
		Kind: "fact", Text: "stranger", Importance: 0.5, Vector: normForTest(t, vec), Dims: 4, Model: "test-embed"}
	env.p.idx.upsertMemory(stranger)
	s := env.p.getOrCreateSession(groupEvent("x", "u", "x", "g"))
	res := env.p.retrieveRAG(context.Background(), s, ragBatch("q"), "q")
	if !strings.Contains(res.MemoryBlock, "channel memory") {
		t.Fatalf("channel memory missing: %q", res.MemoryBlock)
	}
	if !strings.Contains(res.MemoryBlock, "user u memory") {
		t.Fatalf("sender user memory missing: %q", res.MemoryBlock)
	}
	if strings.Contains(res.MemoryBlock, "stranger") {
		t.Fatalf("other user memory leaked: %q", res.MemoryBlock)
	}
}

func TestFitBudgetAndDropOrder(t *testing.T) {
	lines := []ragLine{
		{Score: 0.9, Text: strings.Repeat("a", 80)},
		{Score: 0.8, Text: strings.Repeat("b", 80)},
	}
	out := fitBudget(lines, 100)
	if len(out) != 1 || runeLen(out[0].Text) != 80 {
		t.Fatalf("out=%v", out)
	}
	trunc := fitBudget([]ragLine{{Score: 1, Text: strings.Repeat("x", 100)}}, 50)
	if len(trunc) != 1 || runeLen(trunc[0].Text) != 50 || !strings.HasSuffix(trunc[0].Text, "…") {
		t.Fatalf("trunc=%v", trunc)
	}
	if out := fitBudget(lines, 39); len(out) != 0 {
		t.Fatalf("tiny budget kept items: %v", out)
	}
	if got := sumLines(out) + sumLines(trunc); got <= 0 {
		t.Fatal("sum")
	}
	drop := dropLowest([]ragLine{{Score: 0.9, Text: "high"}, {Score: 0.1, Text: "low"}})
	if len(drop) != 1 || drop[0].Text != "high" {
		t.Fatalf("drop=%v", drop)
	}
}

func TestRAGTotalBudgetDropsKnowledgeFirst(t *testing.T) {
	env := ragEnv(t, map[string]any{
		"memory_context_max_chars":    200,
		"knowledge_context_max_chars": 200,
		"rag_context_max_chars":       120,
	})
	vec := []float32{1, 0, 0, 0}
	env.emb.vectors = map[string][]float32{"q": vec}
	memText := strings.Repeat("m", 60)
	addRAGMemory(t, env, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", memText, "fact", 0.5, vec)
	addRAGChunk(t, env, "dddddddddddddddddddddddddddddddd", "doc", "cccccccccccccccccccccccccccccccc", strings.Repeat("k", 60), 0, vec)
	s := env.p.getOrCreateSession(groupEvent("x", "u", "x", "g"))
	res := env.p.retrieveRAG(context.Background(), s, ragBatch("q"), "q")
	if res.MemoryBlock == "" {
		t.Fatal("memory dropped before knowledge")
	}
	if res.KnowledgeBlock != "" {
		t.Fatalf("knowledge block should be dropped: %q", res.KnowledgeBlock)
	}
	if res.Status != "ok" {
		t.Fatalf("status=%s", res.Status)
	}
}

func TestRAGEscapingAndTitleFallback(t *testing.T) {
	env := ragEnv(t, nil)
	vec := []float32{1, 0, 0, 0}
	env.emb.vectors = map[string][]float32{"q": vec}
	addRAGMemory(t, env, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "a<b>c\x01d", "fact", 0.5, vec)
	addRAGChunk(t, env, "dddddddddddddddddddddddddddddddd", "", "cccccccccccccccccccccccccccccccc", "chunk <x>", 2, vec)
	s := env.p.getOrCreateSession(groupEvent("x", "u", "x", "g"))
	res := env.p.retrieveRAG(context.Background(), s, ragBatch("q"), "q")
	if !strings.Contains(res.MemoryBlock, "a‹b›c d") {
		t.Fatalf("memory block=%q", res.MemoryBlock)
	}
	if !strings.Contains(res.KnowledgeBlock, "[dddddddddddddddddddddddddddddddd#2]") {
		t.Fatalf("knowledge block=%q", res.KnowledgeBlock)
	}
	if !strings.Contains(res.KnowledgeBlock, "chunk ‹x›") {
		t.Fatalf("knowledge escape=%q", res.KnowledgeBlock)
	}
}

func TestRAGQueryTruncation(t *testing.T) {
	batch := []Turn{{Seq: 0, Text: strings.Repeat("a", 600)}, {Seq: 1, Text: strings.Repeat("b", 600)}}
	q := ragQueryFromBatch(batch)
	if runeLen(q) != contextMessageMaxChars {
		t.Fatalf("query len=%d", runeLen(q))
	}
	if !strings.Contains(q, "\n") {
		t.Fatal("batch texts should be joined")
	}
}
