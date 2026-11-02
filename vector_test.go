package chatagent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

func newSeedStore(t *testing.T) *memStore {
	t.Helper()
	s := newMemStore()
	if err := s.CreateTable(context.Background(), tableName); err != nil {
		t.Fatal(err)
	}
	return s
}

func seedRow(t *testing.T, store *memStore, key string, value []byte) {
	t.Helper()
	if err := store.InsertRow(context.Background(), tableName, key, value); err != nil {
		t.Fatalf("insert %s: %v", key, err)
	}
}

func seedMeta(t *testing.T, store *memStore, model string, dims int) {
	seedRow(t, store, tableMetaKey, marshalJSON(metaJSON{Version: 1, Model: model, Dims: dims}))
}

func seedMemory(t *testing.T, store *memStore, id, scope, scopeKey, text, model string, vec []float32, importance float64) {
	t.Helper()
	norm, err := normalizeVector(vec)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(rfc3339Nano)
	rec := memoryRecordJSON{
		ID:         id,
		Scope:      scope,
		ScopeKey:   scopeKey,
		Kind:       "fact",
		Text:       text,
		Importance: importance,
		Vector:     encodeVector(norm),
		Dims:       len(norm),
		Model:      model,
		CreatedAt:  now,
		UpdatedAt:  now,
		LastUsedAt: now,
	}
	seedRow(t, store, memoryRowKey(id), marshalJSON(rec))
}

func seedDoc(t *testing.T, store *memStore, id string, chunkIDs ...string) {
	t.Helper()
	now := time.Now().UTC().Format(rfc3339Nano)
	rec := docRecordJSON{
		ID: id, Source: "https://example.com/" + id, Title: "title-" + id,
		ContentSHA256: "hash", ChunkIDs: chunkIDs, CreatedAt: now, UpdatedAt: now,
	}
	seedRow(t, store, docRowKey(id), marshalJSON(rec))
}

func seedChunk(t *testing.T, store *memStore, id, docID, text, model string, index int, vec []float32) {
	t.Helper()
	norm, err := normalizeVector(vec)
	if err != nil {
		t.Fatal(err)
	}
	rec := chunkRecordJSON{
		ID: id, DocID: docID, Index: index, Text: text,
		Vector: encodeVector(norm), Dims: len(norm), Model: model,
		CreatedAt: time.Now().UTC().Format(rfc3339Nano),
	}
	seedRow(t, store, chunkRowKey(id), marshalJSON(rec))
}

func TestVectorLoadCountsAndCleanup(t *testing.T) {
	store := newSeedStore(t)
	seedMeta(t, store, "test-embed", 4)
	seedMemory(t, store, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "channel", "g", "m1", "test-embed", []float32{1, 0, 0, 0}, 0.5)
	seedMemory(t, store, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "channel", "g", "m2", "old-model", []float32{0, 1, 0, 0}, 0.5)
	seedDoc(t, store, "dddddddddddddddddddddddddddddddd", "cccccccccccccccccccccccccccccccc")
	seedChunk(t, store, "cccccccccccccccccccccccccccccccc", "dddddddddddddddddddddddddddddddd", "chunk", "test-embed", 0, []float32{0, 0, 1, 0})
	seedChunk(t, store, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "dddddddddddddddddddddddddddddddd", "orphan", "test-embed", 1, []float32{0, 0, 0, 1})
	env := newTestEnvWithStore(t, store, map[string]any{"random_enabled": false})
	state, model, dims, memoryCount, knowledge, stale := env.p.idx.statusSnapshot()
	if state != vectorStateReady || model != "test-embed" || dims != 4 {
		t.Fatalf("state=%s model=%s dims=%d", state, model, dims)
	}
	if memoryCount != 1 || knowledge != 1 || stale != 1 {
		t.Fatalf("memory=%d knowledge=%d stale=%d", memoryCount, knowledge, stale)
	}
	if env.p.idx.memoryRows() != 2 {
		t.Fatalf("memory rows=%d", env.p.idx.memoryRows())
	}
	if _, ok := store.row(tableName, chunkRowKey("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")); ok {
		t.Fatal("orphan chunk not removed")
	}
	if _, ok := store.row(tableName, chunkRowKey("cccccccccccccccccccccccccccccccc")); !ok {
		t.Fatal("referenced chunk removed")
	}
}

func TestVectorInvalidDocSkipsCleanup(t *testing.T) {
	store := newSeedStore(t)
	seedMeta(t, store, "test-embed", 4)
	seedRow(t, store, docRowKey("dddddddddddddddddddddddddddddddd"), []byte(`{`))
	seedChunk(t, store, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "dddddddddddddddddddddddddddddddd", "orphan", "test-embed", 0, []float32{1, 0, 0, 0})
	env := newTestEnvWithStore(t, store, map[string]any{"random_enabled": false})
	if env.p.idx.statusState() != vectorStateReady {
		t.Fatal("index not ready")
	}
	if _, ok := store.row(tableName, chunkRowKey("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")); !ok {
		t.Fatal("invalid doc must skip orphan cleanup")
	}
	waitLog(t, env.logs, "invalid_doc_rows")
}

func TestVectorBadRowsSkipped(t *testing.T) {
	store := newSeedStore(t)
	seedMeta(t, store, "test-embed", 4)
	seedRow(t, store, "m:ffffffffffffffffffffffffffffffff", []byte(`{`))
	seedRow(t, store, "c:ffffffffffffffffffffffffffffffff", marshalJSON(chunkRecordJSON{
		ID: "ffffffffffffffffffffffffffffffff", DocID: "d", Text: "x", Vector: "!!!",
		Dims: 4, Model: "test-embed", CreatedAt: time.Now().Format(time.RFC3339Nano),
	}))
	seedRow(t, store, "x:unknown", []byte(`{}`))
	seedMemory(t, store, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "channel", "g", "ok", "test-embed", []float32{1, 0, 0, 0}, 0.5)
	env := newTestEnvWithStore(t, store, map[string]any{"random_enabled": false})
	if env.p.idx.memoryRows() != 2 {
		t.Fatalf("memory rows=%d", env.p.idx.memoryRows())
	}
	if _, ok := store.row(tableName, "x:unknown"); !ok {
		t.Fatal("unknown row must not be deleted")
	}
	waitLog(t, env.logs, "unknown_prefix")
}

func TestVectorMetaMissingAndInvalid(t *testing.T) {
	store := newSeedStore(t)
	seedMemory(t, store, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "channel", "g", "ok", "test-embed", []float32{1, 0, 0, 0}, 0.5)
	env := newTestEnvWithStore(t, store, map[string]any{"random_enabled": false, "embedding_dimensions": 4})
	state, model, dims, _, _, _ := env.p.idx.statusSnapshot()
	if state != vectorStateReady || model != "test-embed" || dims != 0 {
		t.Fatalf("state=%s model=%s dims=%d", state, model, dims)
	}
	raw, ok := store.row(tableName, tableMetaKey)
	if !ok {
		t.Fatal("meta missing was not written back")
	}
	var meta metaJSON
	if err := json.Unmarshal(raw, &meta); err != nil || meta.Version != 1 || meta.Model != "test-embed" || meta.Dims != 4 {
		t.Fatalf("meta=%+v err=%v", meta, err)
	}

	store2 := newSeedStore(t)
	seedRow(t, store2, tableMetaKey, []byte(`{"version":2,"model":"x","dims":9}`))
	env2 := newTestEnvWithStore(t, store2, map[string]any{"random_enabled": false})
	if _, model, dims, _, _, _ := env2.p.idx.statusSnapshot(); model != "test-embed" || dims != 0 {
		t.Fatalf("invalid meta status model=%s dims=%d", model, dims)
	}
}

func TestVectorTopKOrderingAndFilters(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false, "embedding_dimensions": 4})
	q := normForTest(t, []float32{1, 0, 0, 0})
	env.p.idx.upsertMemory(&memoryEntry{
		ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Scope: "channel", ScopeKey: "g", Kind: "fact",
		Text: "b", Vector: normForTest(t, []float32{0.8, 0.6, 0, 0}), Dims: 4, Model: "test-embed",
	})
	env.p.idx.upsertMemory(&memoryEntry{
		ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Scope: "channel", ScopeKey: "g", Kind: "fact",
		Text: "a", Vector: normForTest(t, []float32{0.8, 0.6, 0, 0}), Dims: 4, Model: "test-embed",
	})
	env.p.idx.upsertMemory(&memoryEntry{
		ID: "cccccccccccccccccccccccccccccccc", Scope: "channel", ScopeKey: "g", Kind: "fact",
		Text: "c", Vector: normForTest(t, []float32{1, 0, 0, 0}), Dims: 4, Model: "test-embed",
	})
	env.p.idx.upsertMemory(&memoryEntry{
		ID: "dddddddddddddddddddddddddddddddd", Scope: "channel", ScopeKey: "g", Kind: "fact",
		Text: "stale", Vector: normForTest(t, []float32{1, 0, 0, 0}), Dims: 4, Model: "test-embed", Stale: true,
	})
	env.p.idx.upsertMemory(&memoryEntry{
		ID: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", Scope: "user", ScopeKey: "u", Kind: "fact",
		Text: "user", Vector: normForTest(t, []float32{0.6, 0.8, 0, 0}), Dims: 4, Model: "test-embed",
	})
	env.p.idx.upsertMemory(&memoryEntry{
		ID: "ffffffffffffffffffffffffffffffff", Scope: "channel", ScopeKey: "g", Kind: "fact",
		Text: "dims", Vector: normForTest(t, []float32{1, 0, 0}), Dims: 3, Model: "test-embed",
	})
	hits, mismatch := env.p.idx.searchMemories(q, "g", map[string]struct{}{"u": {}}, 10)
	if mismatch != 1 {
		t.Fatalf("mismatch=%d", mismatch)
	}
	if len(hits) != 4 {
		t.Fatalf("hits=%v", hits)
	}
	if hits[0].ID != "cccccccccccccccccccccccccccccccc" {
		t.Fatalf("top=%v", hits)
	}
	// 同分按 ID 升序
	if hits[1].ID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || hits[2].ID != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("tie order=%v", hits)
	}
	foundUser := false
	for _, h := range hits {
		if h.ID == "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" {
			foundUser = true
		}
		if h.ID == "dddddddddddddddddddddddddddddddd" {
			t.Fatal("stale memory returned")
		}
	}
	if !foundUser {
		t.Fatal("user scope memory not searched")
	}
	top2, _ := env.p.idx.searchMemories(q, "g", nil, 2)
	if len(top2) != 2 || top2[0].ID != "cccccccccccccccccccccccccccccccc" {
		t.Fatalf("top2=%v", top2)
	}
}

func normForTest(t *testing.T, v []float32) []float32 {
	t.Helper()
	n, err := normalizeVector(v)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestVectorWaitReady(t *testing.T) {
	cfg := mustConfig(t, nil)
	idx := newVectorIndex(cfg)
	idx.state = vectorStateLoading
	start := time.Now()
	if state := idx.waitReady(context.Background(), 5*time.Millisecond); state != vectorStateLoading {
		t.Fatalf("state=%s", state)
	}
	if elapsed := time.Since(start); elapsed < 4*time.Millisecond {
		t.Fatalf("returned too early: %v", elapsed)
	}
	go func() {
		time.Sleep(5 * time.Millisecond)
		idx.mu.Lock()
		idx.state = vectorStateReady
		idx.mu.Unlock()
		idx.markReady()
	}()
	if state := idx.waitReady(context.Background(), time.Second); state != vectorStateReady {
		t.Fatalf("state=%s", state)
	}
	idx2 := newVectorIndex(cfg)
	idx2.state = vectorStateDisabled
	if state := idx2.waitReady(context.Background(), time.Second); state != vectorStateDisabled {
		t.Fatalf("state=%s", state)
	}
}

func TestVectorCapacityEviction(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled":     false,
		"vector_max_records": 2,
	})
	now := time.Now().UTC()
	env.p.now = func() time.Time { return now }
	storeRow := func(id string, stale bool, importance float64, age time.Duration) *memoryEntry {
		vec := normForTest(t, []float32{1, 0, 0, 0})
		entry := &memoryEntry{
			ID: id, Scope: "channel", ScopeKey: "g", Kind: "fact", Text: "t-" + id,
			Importance: importance, Vector: vec, Dims: 4, Model: "test-embed",
			CreatedAt: now.Add(-age), UpdatedAt: now.Add(-age), LastUsedAt: now.Add(-age), Stale: stale,
		}
		seedRow(t, env.store, memoryRowKey(id), marshalJSON(memoryRecordOf(entry)))
		env.p.idx.upsertMemory(entry)
		return entry
	}
	stale := storeRow("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true, 1.0, time.Hour)
	low := storeRow("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", false, 0.1, time.Hour)
	evicted, err := env.p.ensureMemoryCapacity(context.Background(), 1)
	if err != nil || evicted != 1 {
		t.Fatalf("evicted=%d err=%v", evicted, err)
	}
	if _, ok := env.p.idx.memoryByID(stale.ID); ok {
		t.Fatal("stale memory not evicted first")
	}
	env.p.idx.releaseSlots(1)
	vec := normForTest(t, []float32{1, 0, 0, 0})
	putChunkForTest(env.p.idx, &chunkEntry{ID: "cccccccccccccccccccccccccccccccc", DocID: "d", Text: "c",
		Vector: vec, Dims: 4, Model: "test-embed"})
	evicted, err = env.p.ensureMemoryCapacity(context.Background(), 1)
	if err != nil || evicted != 1 {
		t.Fatalf("evicted=%d err=%v", evicted, err)
	}
	if _, ok := env.p.idx.memoryByID(low.ID); ok {
		t.Fatal("lowest effective memory not evicted")
	}
	env.p.idx.releaseSlots(1)
	putChunkForTest(env.p.idx, &chunkEntry{ID: "dddddddddddddddddddddddddddddddd", DocID: "d", Text: "d",
		Vector: vec, Dims: 4, Model: "test-embed"})
	if _, err := env.p.ensureMemoryCapacity(context.Background(), 1); !errors.Is(err, errVectorFull) {
		t.Fatalf("err=%v", err)
	}
	if !env.p.idx.reserveKnowledgeSlots(2, 2, 2) {
		t.Fatal("unchanged knowledge projection should fit")
	}
	env.p.idx.releaseSlots(2)
	if env.p.idx.reserveKnowledgeSlots(0, 3, 2) {
		t.Fatal("projection over capacity should fail")
	}
}

func TestStartupErrors(t *testing.T) {
	cfgRaw := testRaw(nil)
	makePlugin := func(store *memStore, tables bot.TableStorage) (*Plugin, context.Context) {
		p := &Plugin{chat: &fixedChat{}, embedder: &fixedEmbedder{dims: 4}}
		pc := bot.PluginContext{
			Name: pluginName, Config: bot.NewConfig(cfgRaw), Logger: testLogger(),
			Storage: store, Tables: tables, HTTPClient: &http.Client{}, Bot: &fakeBot{store: store},
		}
		return p, bot.WithPluginContext(context.Background(), pc)
	}
	store := newMemStore()
	p, ctx := makePlugin(store, nil)
	if err := p.Setup(ctx, &noopRegistrar{}); err == nil || err.Error() != "chat-agent: 需要 TableStorage 支持" {
		t.Fatalf("nil tables error=%v", err)
	}

	off := testRaw(map[string]any{"memory_enabled": false, "rag_enabled": false})
	p = &Plugin{chat: &fixedChat{}, embedder: &fixedEmbedder{dims: 4}}
	pc := bot.PluginContext{Name: pluginName, Config: bot.NewConfig(off), Logger: testLogger(),
		Storage: store, Tables: nil, HTTPClient: &http.Client{}, Bot: &fakeBot{store: store}}
	ctx = bot.WithPluginContext(context.Background(), pc)
	if err := p.Setup(ctx, &noopRegistrar{}); err != nil {
		t.Fatalf("setup off: %v", err)
	}
	if err := p.Start(ctx); err != nil {
		t.Fatalf("start off: %v", err)
	}
	if p.idx.statusState() != vectorStateDisabled {
		t.Fatalf("state=%s", p.idx.statusState())
	}
	if len(store.tables) != 0 {
		t.Fatal("disabled features must not create tables")
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	createFail := newMemStore()
	createFail.createErr = errTest
	p, ctx = makePlugin(createFail, createFail)
	if err := p.Setup(ctx, &noopRegistrar{}); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(ctx); err == nil || !strings.Contains(err.Error(), "chat-agent: 存储表初始化失败") {
		t.Fatalf("create error=%v", err)
	}

	listFail := newMemStore()
	listFail.listErr = errTest
	p, ctx = makePlugin(listFail, listFail)
	if err := p.Setup(ctx, &noopRegistrar{}); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(ctx); err == nil || !strings.Contains(err.Error(), "chat-agent: 存储表加载失败") {
		t.Fatalf("list error=%v", err)
	}
}

func TestReindex(t *testing.T) {
	store := newSeedStore(t)
	seedMeta(t, store, "old-model", 4)
	seedMemory(t, store, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "channel", "g", "stale", "old-model", []float32{1, 0, 0, 0}, 0.5)
	seedDoc(t, store, "dddddddddddddddddddddddddddddddd", "cccccccccccccccccccccccccccccccc")
	seedChunk(t, store, "cccccccccccccccccccccccccccccccc", "dddddddddddddddddddddddddddddddd", "chunk", "old-model", 0, []float32{0, 1, 0, 0})
	env := newTestEnvWithStore(t, store, map[string]any{"random_enabled": false})
	n, err := env.p.runReindex(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if _, _, _, _, _, stale := env.p.idx.statusSnapshot(); stale != 0 {
		t.Fatalf("stale=%d", stale)
	}
	raw, ok := store.row(tableName, tableMetaKey)
	if !ok {
		t.Fatal("meta missing")
	}
	var meta metaJSON
	if err := json.Unmarshal(raw, &meta); err != nil || meta.Model != "test-embed" || meta.Dims != 4 {
		t.Fatalf("meta=%+v err=%v", meta, err)
	}
	raw, _ = store.row(tableName, memoryRowKey("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	var rec memoryRecordJSON
	if err := json.Unmarshal(raw, &rec); err != nil || rec.Model != "test-embed" {
		t.Fatalf("memory row model=%s err=%v", rec.Model, err)
	}
	if n, err := env.p.runReindex(context.Background()); err != nil || n != 0 {
		t.Fatalf("second reindex n=%d err=%v", n, err)
	}
}

func TestVectorEncoding(t *testing.T) {
	vec := []float32{1.5, -2.25, 0}
	decoded, err := decodeVector(encodeVector(vec))
	if err != nil {
		t.Fatal(err)
	}
	for i := range vec {
		if math.Abs(float64(decoded[i]-vec[i])) > 1e-6 {
			t.Fatalf("decoded=%v", decoded)
		}
	}
	if _, err := decodeVector("!!!"); err == nil {
		t.Fatal("bad base64 accepted")
	}
	if _, err := normalizeVector([]float32{0, 0}); err == nil {
		t.Fatal("zero vector accepted")
	}
	if _, err := normalizeVector([]float32{float32(math.Inf(1))}); err == nil {
		t.Fatal("inf vector accepted")
	}
	if _, err := decodeVector("AAAA"); err == nil {
		t.Fatal("non-multiple-of-4 vector accepted")
	}
}
