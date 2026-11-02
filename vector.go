package chatagent

import (
	"container/heap"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

const (
	vectorStateLoading  = "loading"
	vectorStateReady    = "ready"
	vectorStateDisabled = "disabled"
)

// memoryRecordJSON 是 `m:<id32>` 行的 JSON 形状。
type memoryRecordJSON struct {
	ID         string   `json:"id"`
	Scope      string   `json:"scope"`
	ScopeKey   string   `json:"scope_key"`
	Kind       string   `json:"kind"`
	Text       string   `json:"text"`
	Importance float64  `json:"importance"`
	Vector     string   `json:"vector"`
	Dims       int      `json:"dims"`
	Model      string   `json:"model"`
	Sources    []string `json:"sources"`
	CreatedAt  string   `json:"created_at"`
	UpdatedAt  string   `json:"updated_at"`
	LastUsedAt string   `json:"last_used_at"`
	UseCount   int      `json:"use_count"`
}

// docRecordJSON 是 `d:<id32>` 行的 JSON 形状。
type docRecordJSON struct {
	ID            string   `json:"id"`
	Source        string   `json:"source"`
	Title         string   `json:"title"`
	ContentSHA256 string   `json:"content_sha256"`
	ChunkIDs      []string `json:"chunk_ids"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
}

// chunkRecordJSON 是 `c:<id32>` 行的 JSON 形状。
type chunkRecordJSON struct {
	ID        string `json:"id"`
	DocID     string `json:"doc_id"`
	Index     int    `json:"index"`
	Text      string `json:"text"`
	Vector    string `json:"vector"`
	Dims      int    `json:"dims"`
	Model     string `json:"model"`
	CreatedAt string `json:"created_at"`
}

// metaJSON 是 `meta` 行的 JSON 形状。
type metaJSON struct {
	Version int    `json:"version"`
	Model   string `json:"model"`
	Dims    int    `json:"dims"`
}

// memoryEntry 是内存中的记忆记录。
type memoryEntry struct {
	ID         string
	Scope      string
	ScopeKey   string
	Kind       string
	Text       string
	Importance float64
	Vector     []float32
	Dims       int
	Model      string
	Sources    []string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	LastUsedAt time.Time
	UseCount   int

	Stale   bool
	Dirty   bool
	Version uint64
}

// docEntry 是内存中的文档元数据。
type docEntry struct {
	ID            string
	Source        string
	Title         string
	ContentSHA256 string
	ChunkIDs      []string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// chunkEntry 是内存中可见的知识分块。
type chunkEntry struct {
	ID        string
	DocID     string
	Index     int
	Text      string
	Vector    []float32
	Dims      int
	Model     string
	CreatedAt time.Time
	Stale     bool
}

// vectorIndex 是 memory 与 knowledge 共用的内存索引。
type vectorIndex struct {
	mu sync.RWMutex

	state         string
	closed        bool
	meta          metaJSON
	metaFromTable bool
	expectedModel string
	expectedDims  int

	memories         map[string]*memoryEntry
	docs             map[string]*docEntry
	chunks           map[string]*chunkEntry
	deletedMemories  map[string]struct{}
	badMemoryRowKeys map[string]struct{}

	reserved int

	readyCh   chan struct{}
	readyOnce sync.Once
}

func newVectorIndex(cfg *config) *vectorIndex {
	return &vectorIndex{
		state:            vectorStateDisabled,
		expectedModel:    cfg.EmbeddingModel,
		expectedDims:     cfg.EmbeddingDimensions,
		memories:         make(map[string]*memoryEntry),
		docs:             make(map[string]*docEntry),
		chunks:           make(map[string]*chunkEntry),
		deletedMemories:  make(map[string]struct{}),
		badMemoryRowKeys: make(map[string]struct{}),
		readyCh:          make(chan struct{}),
	}
}

func (i *vectorIndex) setLoading() {
	i.mu.Lock()
	i.state = vectorStateLoading
	i.mu.Unlock()
}

func (i *vectorIndex) setDisabled() {
	i.mu.Lock()
	i.state = vectorStateDisabled
	i.mu.Unlock()
}

// close 使索引进入终止状态；之后 loadVector 不得再置 ready。
func (i *vectorIndex) close() {
	i.mu.Lock()
	i.closed = true
	i.mu.Unlock()
	i.markReady()
}

func (i *vectorIndex) markReady() {
	i.readyOnce.Do(func() { close(i.readyCh) })
}

// waitReady 等待索引就绪，最多 timeout。
func (i *vectorIndex) waitReady(ctx context.Context, timeout time.Duration) string {
	i.mu.RLock()
	state := i.state
	ch := i.readyCh
	i.mu.RUnlock()
	if state != vectorStateLoading {
		return state
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
	case <-ctx.Done():
	case <-timer.C:
	}
	i.mu.RLock()
	state = i.state
	i.mu.RUnlock()
	return state
}

func (i *vectorIndex) isStaleLocked(model string, dims int) bool {
	if model != i.expectedModel {
		return true
	}
	if i.expectedDims > 0 && dims != i.expectedDims {
		return true
	}
	return false
}

func (i *vectorIndex) statusSnapshot() (state, model string, dims, memoryCount, knowledgeCount, stale int) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	state = i.state
	model = i.expectedModel
	dims = 0
	if i.metaFromTable {
		model = i.meta.Model
		dims = i.meta.Dims
	}
	for _, m := range i.memories {
		if m.Stale {
			stale++
			continue
		}
		memoryCount++
	}
	for _, c := range i.chunks {
		if c.Stale {
			stale++
			continue
		}
		knowledgeCount++
	}
	return
}

func (i *vectorIndex) visibleLocked() int {
	return len(i.memories) + len(i.chunks) + i.reserved
}

func (i *vectorIndex) memoryRows() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.memories) + len(i.badMemoryRowKeys)
}

// badMemoryKeys 返回无法解析、尚未删除的 m: 行键快照。
func (i *vectorIndex) badMemoryKeys() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]string, 0, len(i.badMemoryRowKeys))
	for key := range i.badMemoryRowKeys {
		out = append(out, key)
	}
	return out
}

// removeBadMemoryKey 在对应行删除成功后清理记录。
func (i *vectorIndex) removeBadMemoryKey(key string) {
	i.mu.Lock()
	delete(i.badMemoryRowKeys, key)
	i.mu.Unlock()
}

func (i *vectorIndex) addBadMemoryKey(key string) {
	i.mu.Lock()
	i.badMemoryRowKeys[key] = struct{}{}
	i.mu.Unlock()
}

func (i *vectorIndex) staleMemoryRows() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	n := 0
	for _, m := range i.memories {
		if m.Stale {
			n++
		}
	}
	return n
}

func (i *vectorIndex) knowledgeStatus() (docs, chunks int) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.docs), len(i.chunks)
}

// memoryRowKey 返回记忆行键。
func memoryRowKey(id string) string { return "m:" + id }

// docRowKey 返回文档行键。
func docRowKey(id string) string { return "d:" + id }

// chunkRowKey 返回分块行键。
func chunkRowKey(id string) string { return "c:" + id }

func isHex32(s string) bool {
	if len(s) != 32 || s != strings.ToLower(s) {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func parseRFC3339Nano(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

// recordJSON 构造行 JSON。
func memoryRecordOf(e *memoryEntry) memoryRecordJSON {
	sources := append([]string{}, e.Sources...)
	return memoryRecordJSON{
		ID:         e.ID,
		Scope:      e.Scope,
		ScopeKey:   e.ScopeKey,
		Kind:       e.Kind,
		Text:       e.Text,
		Importance: e.Importance,
		Vector:     encodeVector(e.Vector),
		Dims:       e.Dims,
		Model:      e.Model,
		Sources:    sources,
		CreatedAt:  e.CreatedAt.UTC().Format(rfc3339Nano),
		UpdatedAt:  e.UpdatedAt.UTC().Format(rfc3339Nano),
		LastUsedAt: e.LastUsedAt.UTC().Format(rfc3339Nano),
		UseCount:   e.UseCount,
	}
}

func docRecordOf(e *docEntry) docRecordJSON {
	return docRecordJSON{
		ID:            e.ID,
		Source:        e.Source,
		Title:         e.Title,
		ContentSHA256: e.ContentSHA256,
		ChunkIDs:      append([]string{}, e.ChunkIDs...),
		CreatedAt:     e.CreatedAt.UTC().Format(rfc3339Nano),
		UpdatedAt:     e.UpdatedAt.UTC().Format(rfc3339Nano),
	}
}

func chunkRecordOf(e *chunkEntry) chunkRecordJSON {
	return chunkRecordJSON{
		ID:        e.ID,
		DocID:     e.DocID,
		Index:     e.Index,
		Text:      e.Text,
		Vector:    encodeVector(e.Vector),
		Dims:      e.Dims,
		Model:     e.Model,
		CreatedAt: e.CreatedAt.UTC().Format(rfc3339Nano),
	}
}

// parseMemoryRow 解析一行 m: 记录。
func parseMemoryRow(row bot.TableRow) (*memoryEntry, error) {
	id, ok := strings.CutPrefix(row.Key, "m:")
	if !ok || !isHex32(id) {
		return nil, fmt.Errorf("bad memory key %q", row.Key)
	}
	var rec memoryRecordJSON
	if err := json.Unmarshal(row.Value, &rec); err != nil {
		return nil, err
	}
	if rec.ID != id {
		return nil, fmt.Errorf("memory id mismatch")
	}
	if rec.Scope != "channel" && rec.Scope != "user" {
		return nil, fmt.Errorf("bad memory scope")
	}
	if !validMemoryKind(rec.Kind) {
		return nil, fmt.Errorf("bad memory kind")
	}
	vec, err := decodeVector(rec.Vector)
	if err != nil {
		return nil, err
	}
	if rec.Dims != len(vec) || len(vec) == 0 {
		return nil, fmt.Errorf("memory dims mismatch")
	}
	norm, err := normalizeVector(vec)
	if err != nil {
		return nil, err
	}
	created, err := parseRFC3339Nano(rec.CreatedAt)
	if err != nil {
		return nil, err
	}
	updated, err := parseRFC3339Nano(rec.UpdatedAt)
	if err != nil {
		return nil, err
	}
	lastUsed, err := parseRFC3339Nano(rec.LastUsedAt)
	if err != nil {
		return nil, err
	}
	importance := rec.Importance
	if importance < 0 {
		importance = 0
	} else if importance > 1 {
		importance = 1
	}
	return &memoryEntry{
		ID:         id,
		Scope:      rec.Scope,
		ScopeKey:   rec.ScopeKey,
		Kind:       rec.Kind,
		Text:       rec.Text,
		Importance: importance,
		Vector:     norm,
		Dims:       rec.Dims,
		Model:      rec.Model,
		Sources:    dedupeStrings(rec.Sources),
		CreatedAt:  created,
		UpdatedAt:  updated,
		LastUsedAt: lastUsed,
		UseCount:   rec.UseCount,
	}, nil
}

func parseDocRow(row bot.TableRow) (*docEntry, error) {
	id, ok := strings.CutPrefix(row.Key, "d:")
	if !ok || !isHex32(id) {
		return nil, fmt.Errorf("bad doc key %q", row.Key)
	}
	var rec docRecordJSON
	if err := json.Unmarshal(row.Value, &rec); err != nil {
		return nil, err
	}
	if rec.ID != id {
		return nil, fmt.Errorf("doc id mismatch")
	}
	created, err := parseRFC3339Nano(rec.CreatedAt)
	if err != nil {
		return nil, err
	}
	updated, err := parseRFC3339Nano(rec.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &docEntry{
		ID:            id,
		Source:        rec.Source,
		Title:         rec.Title,
		ContentSHA256: rec.ContentSHA256,
		ChunkIDs:      append([]string(nil), rec.ChunkIDs...),
		CreatedAt:     created,
		UpdatedAt:     updated,
	}, nil
}

func parseChunkRow(row bot.TableRow) (*chunkEntry, error) {
	id, ok := strings.CutPrefix(row.Key, "c:")
	if !ok || !isHex32(id) {
		return nil, fmt.Errorf("bad chunk key %q", row.Key)
	}
	var rec chunkRecordJSON
	if err := json.Unmarshal(row.Value, &rec); err != nil {
		return nil, err
	}
	if rec.ID != id {
		return nil, fmt.Errorf("chunk id mismatch")
	}
	vec, err := decodeVector(rec.Vector)
	if err != nil {
		return nil, err
	}
	if rec.Dims != len(vec) || len(vec) == 0 {
		return nil, fmt.Errorf("chunk dims mismatch")
	}
	norm, err := normalizeVector(vec)
	if err != nil {
		return nil, err
	}
	created, err := parseRFC3339Nano(rec.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &chunkEntry{
		ID:        id,
		DocID:     rec.DocID,
		Index:     rec.Index,
		Text:      rec.Text,
		Vector:    norm,
		Dims:      rec.Dims,
		Model:     rec.Model,
		CreatedAt: created,
	}, nil
}

func validMemoryKind(kind string) bool {
	switch kind {
	case "fact", "preference", "relationship", "commitment", "event":
		return true
	default:
		return false
	}
}

// parseRowsConcurrently 用固定 worker 数并发解析行，结果索引与输入一致。
func parseRowsConcurrently[T any](ctx context.Context, rows []bot.TableRow, workers int, fn func(bot.TableRow) (T, error)) []struct {
	value T
	err   error
} {
	out := make([]struct {
		value T
		err   error
	}, len(rows))
	if len(rows) == 0 {
		return out
	}
	if workers < 1 {
		workers = 1
	}
	if workers > len(rows) {
		workers = len(rows)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if ctx.Err() != nil {
					continue
				}
				out[idx].value, out[idx].err = fn(rows[idx])
			}
		}()
	}
	for i := range rows {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return out
}

// loadVector 解析表行、回收未引用分块并构造索引。
func (p *Plugin) loadVector(rows []bot.TableRow) {
	idx := p.idx
	var dRows, mRows, cRows []bot.TableRow
	var metaRaw []byte
	for _, row := range rows {
		switch {
		case strings.HasPrefix(row.Key, "d:"):
			dRows = append(dRows, row)
		case strings.HasPrefix(row.Key, "m:"):
			mRows = append(mRows, row)
		case strings.HasPrefix(row.Key, "c:"):
			cRows = append(cRows, row)
		case row.Key == tableMetaKey:
			metaRaw = row.Value
		default:
			p.log.Warn("vector unknown row skipped",
				"operation", "vector", "reason", "unknown_prefix", "key", row.Key)
		}
	}

	docs := make(map[string]*docEntry)
	docsValid := true
	dRes := parseRowsConcurrently(p.ctx, dRows, p.vectorLoadWorkers, parseDocRow)
	if p.ctx.Err() != nil {
		idx.markReady()
		return
	}
	for i, res := range dRes {
		if res.err != nil {
			docsValid = false
			p.log.Warn("vector doc row skipped",
				"operation", "vector", "reason", "bad_row", "key", dRows[i].Key, "error", res.err.Error())
			continue
		}
		docs[res.value.ID] = res.value
	}

	memories := make(map[string]*memoryEntry)
	if p.ctx.Err() != nil {
		idx.markReady()
		return
	}
	mRes := parseRowsConcurrently(p.ctx, mRows, p.vectorLoadWorkers, parseMemoryRow)
	if p.ctx.Err() != nil {
		idx.markReady()
		return
	}
	for i, res := range mRes {
		if res.err != nil {
			idx.addBadMemoryKey(mRows[i].Key)
			p.log.Warn("vector memory row skipped",
				"operation", "vector", "reason", "bad_row", "key", mRows[i].Key, "error", res.err.Error())
			continue
		}
		memories[res.value.ID] = res.value
	}

	allChunks := make(map[string]*chunkEntry)
	if p.ctx.Err() != nil {
		idx.markReady()
		return
	}
	cRes := parseRowsConcurrently(p.ctx, cRows, p.vectorLoadWorkers, parseChunkRow)
	if p.ctx.Err() != nil {
		idx.markReady()
		return
	}
	for i, res := range cRes {
		if res.err != nil {
			p.log.Warn("vector chunk row skipped",
				"operation", "vector", "reason", "bad_row", "key", cRows[i].Key, "error", res.err.Error())
			continue
		}
		allChunks[res.value.ID] = res.value
	}

	// meta 先解析，用于 stale 判定与状态报告。
	meta, metaValid := parseMetaRow(metaRaw)
	if !metaValid {
		meta = metaJSON{Version: 1, Model: p.cfg.EmbeddingModel, Dims: p.cfg.EmbeddingDimensions}
		if meta.Dims == 0 {
			meta.Dims = inferDims(memories, allChunks)
		}
	}

	expectedDims := p.cfg.EmbeddingDimensions
	if expectedDims == 0 && metaValid {
		expectedDims = meta.Dims
	}

	// 只有被合法 d: 行引用的分块进入索引。引用集合独立于 doc_id 校验：
	// 被任何文档 chunk_ids 引用的行都不能在启动回收中删除。
	visibleChunks := make(map[string]*chunkEntry)
	referenced := make(map[string]struct{})
	for _, doc := range docs {
		for _, cid := range doc.ChunkIDs {
			chunk, ok := allChunks[cid]
			if !ok {
				p.log.Warn("vector chunk missing",
					"operation", "vector", "reason", "missing_chunk", "doc", doc.ID, "chunk", cid)
				continue
			}
			referenced[cid] = struct{}{}
			if chunk.DocID != doc.ID {
				p.log.Warn("vector chunk doc mismatch",
					"operation", "vector", "reason", "bad_row", "doc", doc.ID, "chunk", cid)
				continue
			}
			visibleChunks[cid] = chunk
		}
	}

	if p.stopping.Load() || p.ctx.Err() != nil {
		idx.markReady()
		return
	}

	idx.mu.Lock()
	if idx.closed {
		idx.mu.Unlock()
		idx.markReady()
		return
	}
	idx.meta = meta
	idx.metaFromTable = metaValid
	idx.expectedDims = expectedDims
	idx.docs = docs
	for id, m := range memories {
		m.Stale = idx.isStaleLocked(m.Model, m.Dims)
		idx.memories[id] = m
	}
	for id, c := range visibleChunks {
		c.Stale = idx.isStaleLocked(c.Model, c.Dims)
		idx.chunks[id] = c
	}
	idx.mu.Unlock()

	if p.stopping.Load() || p.ctx.Err() != nil {
		idx.markReady()
		return
	}
	p.enforceLoadedCapacity()

	if p.stopping.Load() || p.ctx.Err() != nil {
		idx.markReady()
		return
	}
	if !metaValid {
		if err := p.putRowWait(p.ctx, tableMetaKey, marshalJSON(meta)); err != nil {
			p.log.Warn("vector meta write failed", "operation", "vector", "error", err.Error())
		}
	}

	if docsValid {
		for id := range allChunks {
			if p.ctx.Err() != nil {
				break
			}
			if _, ok := referenced[id]; ok {
				continue
			}
			if err := p.deleteRowWait(p.ctx, chunkRowKey(id)); err != nil {
				p.log.Warn("vector orphan chunk delete failed",
					"operation", "vector", "reason", "storage_error", "chunk", id, "error", err.Error())
				continue
			}
			p.log.Debug("vector orphan chunk deleted", "operation", "vector", "chunk", id)
		}
	} else {
		p.log.Warn("vector orphan cleanup skipped",
			"operation", "vector", "reason", "invalid_doc_rows")
	}

	if p.stopping.Load() || p.ctx.Err() != nil {
		idx.markReady()
		return
	}
	idx.mu.Lock()
	if idx.closed {
		idx.mu.Unlock()
		idx.markReady()
		return
	}
	idx.state = vectorStateReady
	idx.mu.Unlock()
	idx.markReady()
	p.jobs.start()
}

func parseMetaRow(raw []byte) (metaJSON, bool) {
	if len(raw) == 0 {
		return metaJSON{}, false
	}
	var m metaJSON
	if err := json.Unmarshal(raw, &m); err != nil {
		return metaJSON{}, false
	}
	if m.Version != 1 || m.Dims < 0 || strings.TrimSpace(m.Model) == "" {
		return metaJSON{}, false
	}
	return m, true
}

func inferDims(memories map[string]*memoryEntry, chunks map[string]*chunkEntry) int {
	dims := 0
	for _, m := range memories {
		if dims == 0 {
			dims = m.Dims
		} else if dims != m.Dims {
			return 0
		}
	}
	for _, c := range chunks {
		if dims == 0 {
			dims = c.Dims
		} else if dims != c.Dims {
			return 0
		}
	}
	return dims
}

// scoredHit 是一次检索命中。
type scoredHit struct {
	ID    string
	Score float64
}

// hitHeap 用最小堆维护 top-k；分数相同时 ID 较大的先出堆。
type hitHeap []scoredHit

func (h hitHeap) Len() int { return len(h) }
func (h hitHeap) Less(i, j int) bool {
	if h[i].Score != h[j].Score {
		return h[i].Score < h[j].Score
	}
	return h[i].ID > h[j].ID
}
func (h hitHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *hitHeap) Push(x any)   { *h = append(*h, x.(scoredHit)) }
func (h *hitHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

func topKFromHeap(h *hitHeap, k int) []scoredHit {
	out := make([]scoredHit, 0, len(*h))
	for len(*h) > 0 {
		out = append(out, heap.Pop(h).(scoredHit))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > k {
		out = out[:k]
	}
	return out
}

// searchMemories 返回指定 scope 的 top-k 记忆命中，未过滤阈值。
func (i *vectorIndex) searchMemories(query []float32, channelID string, userIDs map[string]struct{}, k int) ([]scoredHit, int) {
	if k <= 0 {
		return nil, 0
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	h := &hitHeap{}
	heap.Init(h)
	dimsMismatch := 0
	for _, m := range i.memories {
		if m.Stale {
			continue
		}
		if m.Dims != len(query) {
			dimsMismatch++
			continue
		}
		matched := false
		switch m.Scope {
		case "channel":
			matched = m.ScopeKey == channelID
		case "user":
			_, matched = userIDs[m.ScopeKey]
		}
		if !matched {
			continue
		}
		score := dot(query, m.Vector)
		if h.Len() < k {
			heap.Push(h, scoredHit{ID: m.ID, Score: score})
			continue
		}
		if betterHit(scoredHit{ID: m.ID, Score: score}, (*h)[0]) {
			heap.Pop(h)
			heap.Push(h, scoredHit{ID: m.ID, Score: score})
		}
	}
	return topKFromHeap(h, k), dimsMismatch
}

// searchChunks 返回全部知识块的 top-k 命中。
func (i *vectorIndex) searchChunks(query []float32, k int) ([]scoredHit, int) {
	if k <= 0 {
		return nil, 0
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	h := &hitHeap{}
	heap.Init(h)
	dimsMismatch := 0
	for _, c := range i.chunks {
		if c.Stale {
			continue
		}
		if c.Dims != len(query) {
			dimsMismatch++
			continue
		}
		score := dot(query, c.Vector)
		if h.Len() < k {
			heap.Push(h, scoredHit{ID: c.ID, Score: score})
			continue
		}
		if betterHit(scoredHit{ID: c.ID, Score: score}, (*h)[0]) {
			heap.Pop(h)
			heap.Push(h, scoredHit{ID: c.ID, Score: score})
		}
	}
	return topKFromHeap(h, k), dimsMismatch
}

func betterHit(a, b scoredHit) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	return a.ID < b.ID
}

// searchSameScope 在同 scope 中搜索 top-k，供合并判定使用。
func (i *vectorIndex) searchSameScope(query []float32, scope, scopeKey string, k int) []scoredHit {
	i.mu.RLock()
	defer i.mu.RUnlock()
	h := &hitHeap{}
	heap.Init(h)
	for _, m := range i.memories {
		if m.Stale || m.Scope != scope || m.ScopeKey != scopeKey || m.Dims != len(query) {
			continue
		}
		score := dot(query, m.Vector)
		if h.Len() < k {
			heap.Push(h, scoredHit{ID: m.ID, Score: score})
			continue
		}
		if betterHit(scoredHit{ID: m.ID, Score: score}, (*h)[0]) {
			heap.Pop(h)
			heap.Push(h, scoredHit{ID: m.ID, Score: score})
		}
	}
	return topKFromHeap(h, k)
}

// reinforce 在内存中更新检索命中的记忆。
func (i *vectorIndex) reinforce(ids []string, now time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, id := range ids {
		m, ok := i.memories[id]
		if !ok {
			continue
		}
		m.UseCount++
		m.LastUsedAt = now
		m.Importance += 0.02
		if m.Importance > 1 {
			m.Importance = 1
		}
		m.Dirty = true
		m.Version++
	}
}

// memoryByID 返回记忆记录深拷贝。
func (i *vectorIndex) memoryByID(id string) (*memoryEntry, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	m, ok := i.memories[id]
	if !ok {
		return nil, false
	}
	cp := *m
	cp.Vector = append([]float32(nil), m.Vector...)
	cp.Sources = append([]string(nil), m.Sources...)
	return &cp, true
}

// docByID 返回文档元数据深拷贝。
func (i *vectorIndex) docByID(id string) (*docEntry, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	d, ok := i.docs[id]
	if !ok {
		return nil, false
	}
	cp := *d
	cp.ChunkIDs = append([]string(nil), d.ChunkIDs...)
	return &cp, true
}

// chunkByID 返回分块深拷贝。
func (i *vectorIndex) chunkByID(id string) (*chunkEntry, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	c, ok := i.chunks[id]
	if !ok {
		return nil, false
	}
	cp := *c
	cp.Vector = append([]float32(nil), c.Vector...)
	return &cp, true
}

// memorySnapshot 返回全部记忆深拷贝。
func (i *vectorIndex) memorySnapshot() []*memoryEntry {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]*memoryEntry, 0, len(i.memories))
	for _, m := range i.memories {
		cp := *m
		cp.Vector = append([]float32(nil), m.Vector...)
		cp.Sources = append([]string(nil), m.Sources...)
		out = append(out, &cp)
	}
	return out
}

// docSnapshot 返回全部文档深拷贝，按 ID 升序。
func (i *vectorIndex) docSnapshot() []*docEntry {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]*docEntry, 0, len(i.docs))
	for _, d := range i.docs {
		cp := *d
		cp.ChunkIDs = append([]string(nil), d.ChunkIDs...)
		out = append(out, &cp)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

func copyMemoryEntry(m *memoryEntry) *memoryEntry {
	cp := *m
	cp.Vector = append([]float32(nil), m.Vector...)
	cp.Sources = append([]string(nil), m.Sources...)
	return &cp
}

// upsertMemory 在内存索引中插入或替换记忆。
func (i *vectorIndex) upsertMemory(m *memoryEntry) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.memories[m.ID] = copyMemoryEntry(m)
	delete(i.deletedMemories, m.ID)
}

const memoryInsertGuard = ^uint64(0)

// applyMemoryWrite 在行写入成功后用版本校验切换内存快照。
// guard 为 memoryInsertGuard 表示期望不存在；否则必须匹配当前 Version。
func (i *vectorIndex) applyMemoryWrite(rec *memoryEntry, guard uint64) (current *memoryEntry, applied, deleted bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, ok := i.deletedMemories[rec.ID]; ok {
		return nil, false, true
	}
	cur, exists := i.memories[rec.ID]
	if guard == memoryInsertGuard {
		if exists {
			return copyMemoryEntry(cur), false, false
		}
	} else if !exists || cur.Version != guard {
		if exists {
			return copyMemoryEntry(cur), false, false
		}
		return nil, false, false
	}
	i.memories[rec.ID] = copyMemoryEntry(rec)
	delete(i.deletedMemories, rec.ID)
	return nil, true, false
}

// claimMemory 在物理删除前移除记录并设置 tombstone，阻止并发写复活。
func (i *vectorIndex) claimMemory(id string) (*memoryEntry, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	m, ok := i.memories[id]
	if !ok {
		return nil, false
	}
	delete(i.memories, id)
	i.deletedMemories[id] = struct{}{}
	return copyMemoryEntry(m), true
}

// claimMemoryVersion 只在版本未变化时 claim，避免删除刚被检索强化的记录。
func (i *vectorIndex) claimMemoryVersion(id string, version uint64) (*memoryEntry, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	m, ok := i.memories[id]
	if !ok || m.Version != version {
		return nil, false
	}
	delete(i.memories, id)
	i.deletedMemories[id] = struct{}{}
	return copyMemoryEntry(m), true
}

// restoreMemory 在删除失败时恢复 claim 前的快照；若已有更新的记录则不动。
func (i *vectorIndex) restoreMemory(m *memoryEntry) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, ok := i.memories[m.ID]; ok {
		return
	}
	i.memories[m.ID] = copyMemoryEntry(m)
	delete(i.deletedMemories, m.ID)
}

// applyReinforcement 只为版本未变化的记录清除 Dirty。
func (i *vectorIndex) applyReinforcement(m *memoryEntry) (current *memoryEntry, applied, deleted bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, ok := i.deletedMemories[m.ID]; ok {
		return nil, false, true
	}
	cur, ok := i.memories[m.ID]
	if !ok {
		return nil, false, false
	}
	if cur.Version != m.Version {
		return copyMemoryEntry(cur), false, false
	}
	cur.Dirty = false
	return nil, true, false
}

// isMemoryDeleted 返回记录是否已被删除（用于阻止延迟写入复活）。
func (i *vectorIndex) isMemoryDeleted(id string) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	_, ok := i.deletedMemories[id]
	return ok
}

// applyReindexMemory 只在记录版本未变化时更新重建相关字段。
func (i *vectorIndex) applyReindexMemory(id string, version uint64, vec []float32, model string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	m, ok := i.memories[id]
	if !ok || m.Version != version {
		return false
	}
	m.Vector = append([]float32(nil), vec...)
	m.Dims = len(vec)
	m.Model = model
	m.Stale = false
	return true
}

// applyReindexChunk 只更新仍存在的分块的重建字段。
func (i *vectorIndex) applyReindexChunk(id string, vec []float32, model string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	c, ok := i.chunks[id]
	if !ok {
		return false
	}
	c.Vector = append([]float32(nil), vec...)
	c.Dims = len(vec)
	c.Model = model
	c.Stale = false
	return true
}

// statusState 返回当前索引状态。
func (i *vectorIndex) statusState() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.state
}

// replaceDoc 在文件提交后原子替换文档与其可见分块。
func (i *vectorIndex) replaceDoc(doc *docEntry, chunks []*chunkEntry, oldChunkIDs []string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	remove := make(map[string]struct{}, len(oldChunkIDs))
	for _, id := range oldChunkIDs {
		remove[id] = struct{}{}
	}
	for id := range remove {
		delete(i.chunks, id)
	}
	for _, c := range chunks {
		cp := *c
		cp.Vector = append([]float32(nil), c.Vector...)
		i.chunks[c.ID] = &cp
	}
	cp := *doc
	cp.ChunkIDs = append([]string(nil), doc.ChunkIDs...)
	i.docs[doc.ID] = &cp
}

// updateDocMeta 只更新文档元数据（标题/更新时间）。
func (i *vectorIndex) updateDocMeta(doc *docEntry) {
	i.mu.Lock()
	defer i.mu.Unlock()
	cp := *doc
	cp.ChunkIDs = append([]string(nil), doc.ChunkIDs...)
	i.docs[doc.ID] = &cp
}

// removeDoc 删除文档及其可见分块。
func (i *vectorIndex) removeDoc(id string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	doc, ok := i.docs[id]
	if !ok {
		return
	}
	for _, cid := range doc.ChunkIDs {
		delete(i.chunks, cid)
	}
	delete(i.docs, id)
}

func (i *vectorIndex) releaseSlots(n int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.reserved -= n
	if i.reserved < 0 {
		i.reserved = 0
	}
}

// reserveKnowledgeSlots 判断知识库更新后的容量并预留新块名额。
func (i *vectorIndex) reserveKnowledgeSlots(oldChunks, newChunks, max int) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.visibleLocked()-oldChunks+newChunks > max {
		return false
	}
	i.reserved += newChunks
	return true
}

// reindexSnapshot 返回需要重建的记录快照。
func (i *vectorIndex) reindexSnapshot() ([]*memoryEntry, []*chunkEntry) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	var mems []*memoryEntry
	var chunks []*chunkEntry
	for _, m := range i.memories {
		if m.Stale {
			cp := *m
			cp.Vector = append([]float32(nil), m.Vector...)
			cp.Sources = append([]string(nil), m.Sources...)
			mems = append(mems, &cp)
		}
	}
	for _, c := range i.chunks {
		if c.Stale {
			cp := *c
			cp.Vector = append([]float32(nil), c.Vector...)
			chunks = append(chunks, &cp)
		}
	}
	return mems, chunks
}

// setMeta 在 reindex 全部成功后更新 meta。
func (i *vectorIndex) setMeta(meta metaJSON) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.meta = meta
	i.metaFromTable = true
	i.expectedDims = meta.Dims
}

// errVectorFull 是容量不足的固定错误。
var errVectorFull = errors.New("vector_full")
