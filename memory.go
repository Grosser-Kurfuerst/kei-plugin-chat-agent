package chatagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	// memoryExtractTemperature 是抽取请求温度。
	memoryExtractTemperature = 0.2
	// memoryExtractMaxTokens 是抽取请求上限。
	memoryExtractMaxTokens = 800
	// memorySourceLimit 是单条记忆保留的来源数。
	memorySourceLimit = 8
	// memoryTextMinRunes / memoryTextMaxRunes 是抽取文本长度范围。
	memoryTextMinRunes = 4
	memoryTextMaxRunes = 500
)

// memoryExtractPrompt 是逐字固定的记忆抽取提示词。
const memoryExtractPrompt = `你是记忆整理器。阅读下面的对话，抽取值得长期记住的信息。
如果消息包含图片，图片内容同样属于对话内容；把图片中值得长期记住的信息写入 text，不要只写“用户发了一张图片”。
只输出 JSON，不要解释。格式：
{"memories":[{"text":"...","kind":"fact|preference|relationship|commitment|event","importance":0.0}]}
规则：
- 只记录用户明确表达、未来对话仍有用的事实、偏好、关系、承诺或重要事件。
- 不记录寒暄、一次性问题、助手自己的推测、敏感凭据（密码、令牌、身份证号）。
- text 用对话原语言，≤200 字，独立成句。
- importance 为 0..1 的小数；越稳定越重要。
- 没有可记内容时输出 {"memories":[]}。`

// extractedMemory 是一条通过校验的抽取结果。
type extractedMemory struct {
	Text       string
	Kind       string
	Importance float64
}

// memoryScope 返回会话的记忆 scope。
func memoryScope(s *sessionState) (scope, key string) {
	if s.kind == "private" {
		return "user", s.peerUserID
	}
	return "channel", s.channelID
}

// memoryIDFor 计算记忆 ID。
func memoryIDFor(scope, scopeKey, text string) string {
	return hash16(scope, scopeKey, normalizeText(text))
}

// runConsolidate 执行一轮记忆整理。
func (p *Plugin) runConsolidate(ctx context.Context) (string, error) {
	if !p.cfg.MemoryEnabled {
		return "sessions_done=0 sessions_failed=0 memories_added=0 memories_merged=0", nil
	}
	p.flushReinforcements(ctx)
	sessionsDone, sessionsFailed := 0, 0
	added, merged := 0, 0

	type candidate struct {
		s          *sessionState
		lastActive time.Time
	}
	p.sessionsMu.Lock()
	candidates := make([]candidate, 0, len(p.sessions))
	for _, s := range p.sessions {
		s.mu.Lock()
		if s.consolidatedSeq < s.seq {
			candidates = append(candidates, candidate{s: s, lastActive: s.lastActivity})
		}
		s.mu.Unlock()
	}
	p.sessionsMu.Unlock()
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].lastActive.Before(candidates[j].lastActive)
	})
	if len(candidates) > p.cfg.MemoryConsolidateSessions {
		candidates = candidates[:p.cfg.MemoryConsolidateSessions]
	}

	for _, c := range candidates {
		if ctx.Err() != nil {
			break
		}
		a, m, err := p.consolidateSession(ctx, c.s)
		if err != nil {
			sessionsFailed++
			p.log.Warn("consolidate session failed",
				"operation", "consolidate", "session", sessionLogPrefix(c.s.key),
				"reason", memoryFailureReason(err), "error", err.Error())
			continue
		}
		sessionsDone++
		added += a
		merged += m
	}

	expired, evicted := p.decayMemories(ctx)
	if expired > 0 || evicted > 0 {
		p.log.Debug("memory expiry", "operation", "consolidate", "expired", expired, "evicted", evicted)
	}
	summary := fmt.Sprintf("sessions_done=%d sessions_failed=%d memories_added=%d memories_merged=%d",
		sessionsDone, sessionsFailed, added, merged)
	p.log.Info("consolidate done",
		"operation", "consolidate",
		"sessions_done", sessionsDone,
		"sessions_failed", sessionsFailed,
		"memories_added", added,
		"memories_merged", merged,
		"expired", expired,
		"evicted", evicted,
	)
	return summary, nil
}

// consolidateSession 整理单个会话；成功后推进水位。
func (p *Plugin) consolidateSession(ctx context.Context, s *sessionState) (added, merged int, err error) {
	s.mu.Lock()
	consolidated := s.consolidatedSeq
	start := consolidated
	lost := s.lostSeq
	epoch := s.epoch
	turns := make([]Turn, 0, len(s.history))
	for _, t := range s.history {
		t.Images = append([]string(nil), t.Images...)
		turns = append(turns, t)
	}
	s.mu.Unlock()

	if lost > start {
		start = lost
		p.log.Warn("memory lost range skipped",
			"operation", "consolidate", "session", sessionLogPrefix(s.key), "lost_seq", lost)
	}
	selected := make([]Turn, 0, p.cfg.MemoryConsolidateMessages)
	for _, t := range turns {
		if t.Seq < start {
			continue
		}
		selected = append(selected, t)
		if len(selected) >= p.cfg.MemoryConsolidateMessages {
			break
		}
	}
	if len(selected) == 0 {
		if lost > consolidated {
			p.advanceWatermark(s, epoch, lost)
		}
		return 0, 0, nil
	}
	if len(selected) < p.cfg.MemoryExtractMinMessages {
		return 0, 0, nil
	}
	lastSeq := selected[len(selected)-1].Seq + 1
	if p.cfg.MemoryExtractMax == 0 {
		p.advanceWatermark(s, epoch, lastSeq)
		return 0, 0, nil
	}

	batches := p.extractionBatches(selected)
	var extracted []extractedMemory
	for _, batch := range batches {
		if ctx.Err() != nil {
			return 0, 0, ctx.Err()
		}
		mems, err := p.extractBatch(ctx, s, batch)
		if err != nil {
			return 0, 0, err
		}
		extracted = append(extracted, mems...)
	}
	if len(extracted) > p.cfg.MemoryExtractMax {
		extracted = extracted[:p.cfg.MemoryExtractMax]
	}
	sources := sourceEventIDs(selected)
	added, merged, _, err = p.storeMemories(ctx, s, extracted, sources)
	if err != nil {
		return added, merged, err
	}
	p.advanceWatermark(s, epoch, lastSeq)
	return added, merged, nil
}

// advanceWatermark 在会话代次未变化时推进整理水位。
func (p *Plugin) advanceWatermark(s *sessionState, epoch, seq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.epoch != epoch {
		return
	}
	if seq > s.consolidatedSeq {
		s.consolidatedSeq = seq
	}
}

// extractionBatches 按消息与图片上限切分抽取批次。
func (p *Plugin) extractionBatches(turns []Turn) [][]Turn {
	if !p.cfg.VisionEnabled {
		return [][]Turn{turns}
	}
	var out [][]Turn
	var cur []Turn
	images := 0
	for _, t := range turns {
		n := len(t.Images)
		if n > visionMaxImagesPerRequest {
			n = visionMaxImagesPerRequest
		}
		if len(cur) > 0 && (len(cur) >= p.cfg.MemoryConsolidateMessages || images+n > visionMaxImagesPerRequest) {
			out = append(out, cur)
			cur = nil
			images = 0
		}
		cur = append(cur, t)
		images += n
		if len(cur) >= p.cfg.MemoryConsolidateMessages {
			out = append(out, cur)
			cur = nil
			images = 0
		}
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// extractBatch 对一批消息调用 LLM 抽取。
func (p *Plugin) extractBatch(ctx context.Context, s *sessionState, turns []Turn) ([]extractedMemory, error) {
	attachments := p.fetchImagesForExtraction(ctx, turns, visionMaxImagesPerRequest)
	content := p.buildExtractionContent(s, turns, attachments)
	if !hasMeaningfulContent(turns, attachments) {
		return nil, nil
	}
	req := ChatRequest{
		Model:       p.cfg.LLMModel,
		Messages:    []ChatMessage{{Role: "system", Content: memoryExtractPrompt}, {Role: "user", Content: content}},
		Temperature: memoryExtractTemperature,
		MaxTokens:   memoryExtractMaxTokens,
	}
	result, err := p.chat.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	mems, invalid, perr := parseExtractionResponse(result.Content, p.cfg.MemoryExtractMax)
	if invalid > 0 {
		p.log.Warn("memory extraction entries dropped", "operation", "consolidate", "invalid", invalid)
	}
	if perr != nil {
		return nil, perr
	}
	return mems, nil
}

// buildExtractionContent 渲染抽取输入，图片按占位符顺序附加。
func (p *Plugin) buildExtractionContent(s *sessionState, turns []Turn, attachments map[uint64][]*imageAttachment) any {
	var buf strings.Builder
	var parts []ContentPart
	hasImage := false
	flush := func() {
		if buf.Len() == 0 {
			return
		}
		parts = append(parts, ContentPart{Type: "text", Text: buf.String()})
		buf.Reset()
	}
	s.mu.Lock()
	kind := s.kind
	sessionName := s.channelName
	peerName := s.peerName
	peerUserID := s.peerUserID
	channelID := s.channelID
	s.mu.Unlock()
	if kind == "private" {
		sessionName = peerName
		if sessionName == "" {
			sessionName = peerUserID
		}
	} else if sessionName == "" {
		sessionName = channelID
	}
	buf.WriteString("[会话类型] ")
	buf.WriteString(chatKindLabel(kind))
	buf.WriteString("\n[会话] ")
	buf.WriteString(sessionName)
	buf.WriteString("\n[消息]\n")
	for _, t := range turns {
		line := displayName(t) + " (" + t.UserID + "): " + takePrefix(t.Text, contextMessageMaxChars)
		buf.WriteString(line)
		for _, att := range attachments[t.Seq] {
			if att == nil {
				continue
			}
			flush()
			parts = append(parts, ContentPart{Type: "image_url", ImageURL: &ImageURL{URL: att.DataURL}})
			hasImage = true
		}
		buf.WriteString("\n")
	}
	flush()
	if !hasImage {
		var plain strings.Builder
		for _, part := range parts {
			plain.WriteString(part.Text)
		}
		return plain.String()
	}
	return parts
}

// hasMeaningfulContent 判断批次是否有可抽取文本或成功图片。
func hasMeaningfulContent(turns []Turn, attachments map[uint64][]*imageAttachment) bool {
	for _, imgs := range attachments {
		for _, att := range imgs {
			if att != nil {
				return true
			}
		}
	}
	replacer := strings.NewReplacer(
		"[图片]", "", "[表情]", "", "[文件]", "", "[卡片]", "", "[引用]", "", "[消息]", "",
	)
	for _, t := range turns {
		if strings.TrimSpace(replacer.Replace(t.Text)) != "" {
			return true
		}
	}
	return false
}

// parseExtractionResponse 解析并校验抽取响应。
func parseExtractionResponse(content string, limit int) ([]extractedMemory, int, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &top); err != nil {
		return nil, 1, err
	}
	raw, ok := top["memories"]
	if !ok {
		return nil, 1, errors.New("memories missing")
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, 1, errors.New("memories is null")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, 1, err
	}
	valid := make([]extractedMemory, 0, len(items))
	invalid := 0
	for _, item := range items {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(item, &obj); err != nil {
			invalid++
			continue
		}
		var text, kind string
		var importance float64
		if err := json.Unmarshal(obj["text"], &text); err != nil {
			invalid++
			continue
		}
		text = strings.TrimSpace(text)
		if runeLen(text) < memoryTextMinRunes || runeLen(text) > memoryTextMaxRunes {
			invalid++
			continue
		}
		if err := json.Unmarshal(obj["kind"], &kind); err != nil || !validMemoryKind(kind) {
			invalid++
			continue
		}
		if err := json.Unmarshal(obj["importance"], &importance); err != nil {
			invalid++
			continue
		}
		importance = math.Max(0, math.Min(1, importance))
		valid = append(valid, extractedMemory{Text: text, Kind: kind, Importance: importance})
	}
	if len(items) > 0 && len(valid) == 0 {
		return nil, invalid, errors.New("all extraction entries invalid")
	}
	if limit >= 0 && len(valid) > limit {
		valid = valid[:limit]
	}
	return valid, invalid, nil
}

// sourceEventIDs 返回去重后的来源事件 ID，最多 8 条。
func sourceEventIDs(turns []Turn) []string {
	out := make([]string, 0, len(turns))
	for _, t := range turns {
		if t.EventID == "" || t.IsSelf {
			continue
		}
		out = append(out, t.EventID)
	}
	out = dedupeStrings(out)
	if len(out) > memorySourceLimit {
		out = out[:memorySourceLimit]
	}
	return out
}

// storeMemories 嵌入、合并或插入记忆，返回新增与合并数量及最终 ID。
func (p *Plugin) storeMemories(ctx context.Context, s *sessionState, mems []extractedMemory, sources []string) (added, merged int, ids []string, err error) {
	if len(mems) == 0 {
		return 0, 0, nil, nil
	}
	scope, scopeKey := memoryScope(s)
	texts := make([]string, len(mems))
	for i, m := range mems {
		texts[i] = m.Text
	}
	vecs, err := p.embedQueuedBatches(ctx, texts)
	if err != nil {
		return 0, 0, nil, kerr("embedding_error", err)
	}
	if len(vecs) != len(mems) {
		return 0, 0, nil, kerr("embedding_error", errors.New("embedding count mismatch"))
	}
	now := p.now().UTC()
	ids = make([]string, 0, len(mems))
	for i, m := range mems {
		vec, nerr := normalizeVector(vecs[i])
		if nerr != nil {
			return added, merged, ids, kerr("embedding_error", nerr)
		}
		id, mok, merr := p.mergeTarget(ctx, scope, scopeKey, m, vec, now, sources)
		if merr != nil {
			return added, merged, ids, merr
		}
		if mok {
			merged++
			ids = append(ids, id)
			continue
		}
		id = memoryIDFor(scope, scopeKey, m.Text)
		if existing, ok := p.idx.memoryByID(id); ok {
			if err := p.mergeMemoryRecord(ctx, existing, m, vec, now, sources); err != nil {
				return added, merged, ids, err
			}
			merged++
			ids = append(ids, id)
			continue
		}
		// 新插入
		_, cerr := p.ensureMemoryCapacity(ctx, 1)
		if cerr != nil {
			if errors.Is(cerr, errVectorFull) {
				return added, merged, ids, cerr
			}
			return added, merged, ids, kerr("storage_error", cerr)
		}
		rec := &memoryEntry{
			ID:         id,
			Scope:      scope,
			ScopeKey:   scopeKey,
			Kind:       m.Kind,
			Text:       m.Text,
			Importance: m.Importance,
			Vector:     vec,
			Dims:       len(vec),
			Model:      p.cfg.EmbeddingModel,
			Sources:    append([]string(nil), sources...),
			CreatedAt:  now,
			UpdatedAt:  now,
			LastUsedAt: now,
		}
		key := memoryRowKey(rec.ID)
		if err := p.putRowWait(ctx, key, marshalJSON(memoryRecordOf(rec))); err != nil {
			p.idx.releaseSlots(1)
			return added, merged, ids, kerr("storage_error", err)
		}
		current, applied, deleted := p.idx.applyMemoryWrite(rec, memoryInsertGuard)
		if !applied {
			p.idx.releaseSlots(1)
			if deleted {
				_ = p.deleteRowWait(ctx, key)
			} else if current != nil {
				if err := p.putRowWait(ctx, key, marshalJSON(memoryRecordOf(current))); err != nil {
					p.log.Warn("memory insert rollback failed",
						"operation", "consolidate", "reason", "storage_error", "memory", rec.ID, "error", err.Error())
				}
			}
			return added, merged, ids, kerr("storage_error", errors.New("memory insert conflict"))
		}
		p.idx.releaseSlots(1)
		added++
		ids = append(ids, rec.ID)
	}
	return added, merged, ids, nil
}

// mergeTarget 尝试把候选合并到同 scope 的相似记忆。
func (p *Plugin) mergeTarget(ctx context.Context, scope, scopeKey string, m extractedMemory, vec []float32, now time.Time, sources []string) (string, bool, error) {
	hits := p.idx.searchSameScope(vec, scope, scopeKey, 1)
	if len(hits) == 0 || hits[0].Score < p.cfg.MemoryDedupeThreshold {
		return "", false, nil
	}
	if p.idx.isMemoryDeleted(hits[0].ID) {
		return "", false, nil
	}
	old, ok := p.idx.memoryByID(hits[0].ID)
	if !ok {
		return "", false, nil
	}
	if err := p.mergeMemoryRecord(ctx, old, m, vec, now, sources); err != nil {
		return "", false, err
	}
	return old.ID, true, nil
}

// mergeMemoryRecord 用新候选更新已有记忆；行写入后用版本 CAS 切换内存快照。
func (p *Plugin) mergeMemoryRecord(ctx context.Context, old *memoryEntry, m extractedMemory, vec []float32, now time.Time, sources []string) error {
	rec := *old
	rec.Text = m.Text
	rec.Kind = m.Kind
	rec.Vector = vec
	rec.Dims = len(vec)
	rec.Model = p.cfg.EmbeddingModel
	rec.Importance = math.Max(old.Importance, m.Importance)
	rec.Sources = mergeSources(old.Sources, sources)
	rec.UpdatedAt = now
	rec.LastUsedAt = now
	rec.UseCount++
	rec.Version = old.Version + 1
	key := memoryRowKey(rec.ID)
	if err := p.putRowWait(ctx, key, marshalJSON(memoryRecordOf(&rec))); err != nil {
		return kerr("storage_error", err)
	}
	current, applied, deleted := p.idx.applyMemoryWrite(&rec, old.Version)
	if applied {
		return nil
	}
	if deleted {
		_ = p.deleteRowWait(ctx, key)
		return kerr("storage_error", errors.New("memory deleted concurrently"))
	}
	if current != nil {
		if err := p.putRowWait(ctx, key, marshalJSON(memoryRecordOf(current))); err != nil {
			p.log.Warn("memory merge rollback failed",
				"operation", "consolidate", "reason", "storage_error", "memory", rec.ID, "error", err.Error())
		}
	}
	return kerr("storage_error", errors.New("memory version conflict"))
}

func mergeSources(old, add []string) []string {
	merged := make([]string, 0, len(old)+len(add))
	merged = append(merged, old...)
	merged = append(merged, add...)
	merged = dedupeStrings(merged)
	if len(merged) > memorySourceLimit {
		merged = merged[:memorySourceLimit]
	}
	return merged
}

// ensureMemoryCapacity 为新记忆腾出空间并预留一个名额。
func (p *Plugin) ensureMemoryCapacity(ctx context.Context, need int) (evicted int, err error) {
	for {
		p.idx.mu.Lock()
		visible := p.idx.visibleLocked()
		nonMemory := visible - len(p.idx.memories)
		if nonMemory+need > p.cfg.VectorMaxRecords {
			p.idx.mu.Unlock()
			return evicted, errVectorFull
		}
		if visible+need <= p.cfg.VectorMaxRecords {
			p.idx.reserved += need
			p.idx.mu.Unlock()
			return evicted, nil
		}
		victim := p.pickMemoryVictimLocked()
		if victim == nil {
			p.idx.mu.Unlock()
			return evicted, errVectorFull
		}
		delete(p.idx.memories, victim.ID)
		p.idx.deletedMemories[victim.ID] = struct{}{}
		p.idx.mu.Unlock()

		if derr := p.deleteRowWait(ctx, memoryRowKey(victim.ID)); derr != nil {
			p.idx.mu.Lock()
			p.idx.memories[victim.ID] = victim
			delete(p.idx.deletedMemories, victim.ID)
			p.idx.mu.Unlock()
			return evicted, derr
		}
		evicted++
	}
}

// enforceLoadedCapacity 在启动加载后把可见记录压回上限。只淘汰记忆；
// 知识块占用超过上限时记录 Error，等待管理员删除文档或提高上限。
func (p *Plugin) enforceLoadedCapacity() {
	for {
		p.idx.mu.Lock()
		visible := p.idx.visibleLocked()
		if visible <= p.cfg.VectorMaxRecords {
			p.idx.mu.Unlock()
			return
		}
		if visible-len(p.idx.memories) > p.cfg.VectorMaxRecords {
			p.idx.mu.Unlock()
			p.log.Error("vector capacity exceeded",
				"operation", "vector", "reason", "capacity_exceeded",
				"visible", visible, "limit", p.cfg.VectorMaxRecords)
			return
		}
		victim := p.pickMemoryVictimLocked()
		if victim == nil {
			p.idx.mu.Unlock()
			p.log.Error("vector capacity exceeded",
				"operation", "vector", "reason", "capacity_exceeded",
				"visible", visible, "limit", p.cfg.VectorMaxRecords)
			return
		}
		delete(p.idx.memories, victim.ID)
		p.idx.deletedMemories[victim.ID] = struct{}{}
		p.idx.mu.Unlock()

		if err := p.deleteRowWait(p.ctx, memoryRowKey(victim.ID)); err != nil {
			p.idx.mu.Lock()
			p.idx.memories[victim.ID] = victim
			delete(p.idx.deletedMemories, victim.ID)
			p.idx.mu.Unlock()
			p.log.Warn("vector capacity eviction failed",
				"operation", "vector", "reason", "storage_error", "memory", victim.ID, "error", err.Error())
			return
		}
	}
}

// pickMemoryVictimLocked 选择 stale 优先、其次 effective 最低的记忆。
func (p *Plugin) pickMemoryVictimLocked() *memoryEntry {
	var stale *memoryEntry
	var lowest *memoryEntry
	lowestEff := math.Inf(1)
	now := p.now().UTC()
	for _, m := range p.idx.memories {
		if m.Stale {
			if stale == nil || m.ID < stale.ID {
				stale = m
			}
			continue
		}
		eff := effectiveImportance(m, now, p.cfg.MemoryDecayHalfLife)
		if lowest == nil || eff < lowestEff || (eff == lowestEff && m.ID < lowest.ID) {
			lowest = m
			lowestEff = eff
		}
	}
	if stale != nil {
		cp := *stale
		return &cp
	}
	if lowest != nil {
		cp := *lowest
		return &cp
	}
	return nil
}

func effectiveImportance(m *memoryEntry, now time.Time, halfLife time.Duration) float64 {
	age := now.Sub(m.UpdatedAt)
	if age < 0 {
		age = 0
	}
	return m.Importance * math.Pow(0.5, float64(age)/float64(halfLife))
}

// decayMemories 删除衰减过低或 TTL 过期的记忆。
func (p *Plugin) decayMemories(ctx context.Context) (expired, evicted int) {
	now := p.now().UTC()
	for _, m := range p.idx.memorySnapshot() {
		if ctx.Err() != nil {
			break
		}
		remove := false
		if effectiveImportance(m, now, p.cfg.MemoryDecayHalfLife) < p.cfg.MemoryMinImportance {
			remove = true
		}
		if p.cfg.MemoryTTL > 0 && now.Sub(m.LastUsedAt) > p.cfg.MemoryTTL {
			remove = true
		}
		if !remove {
			continue
		}
		entry, claimed := p.idx.claimMemoryVersion(m.ID, m.Version)
		if !claimed {
			continue
		}
		if err := p.deleteRowWait(ctx, memoryRowKey(m.ID)); err != nil {
			p.idx.restoreMemory(entry)
			p.log.Warn("memory delete failed",
				"operation", "consolidate", "reason", "storage_error", "memory", m.ID, "error", err.Error())
			continue
		}
		expired++
	}
	return expired, evicted
}

// flushReinforcements 把内存中的强化批量落库。
func (p *Plugin) flushReinforcements(ctx context.Context) {
	for _, m := range p.idx.memorySnapshot() {
		if ctx.Err() != nil {
			break
		}
		if !m.Dirty || p.idx.isMemoryDeleted(m.ID) {
			continue
		}
		key := memoryRowKey(m.ID)
		if err := p.putRowWait(ctx, key, marshalJSON(memoryRecordOf(m))); err != nil {
			p.log.Warn("memory reinforcement write failed",
				"operation", "consolidate", "reason", "storage_error", "memory", m.ID, "error", err.Error())
			continue
		}
		current, applied, deleted := p.idx.applyReinforcement(m)
		if applied {
			continue
		}
		if deleted {
			if err := p.deleteRowWait(ctx, key); err != nil {
				p.log.Warn("memory delete after reinforcement failed",
					"operation", "consolidate", "reason", "storage_error", "memory", m.ID, "error", err.Error())
			}
			continue
		}
		if current != nil {
			if err := p.putRowWait(ctx, key, marshalJSON(memoryRecordOf(current))); err != nil {
				p.log.Warn("memory reinforcement rollback failed",
					"operation", "consolidate", "reason", "storage_error", "memory", m.ID, "error", err.Error())
			}
		}
	}
}

// memoryFailureReason 把内部错误映射为固定失败 reason。
func memoryFailureReason(err error) string {
	if errors.Is(err, errVectorFull) {
		return "vector_full"
	}
	var ke *knowledgeError
	if errors.As(err, &ke) {
		return ke.reason
	}
	switch categoryOf(err) {
	case errCanceled:
		return "internal_error"
	case errTimeout:
		return "timeout"
	}
	return "internal_error"
}
