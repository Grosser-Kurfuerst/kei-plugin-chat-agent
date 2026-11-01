package chatagent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

const (
	// historyCleanupTimeout 是单轮缓存清理超时。
	historyCleanupTimeout = time.Minute
	// reindexTimeout 是单次向量重建超时。
	reindexTimeout = 5 * time.Minute
)

// jobNotify 接收异步任务完成结果；nil 表示定时触发。
type jobNotify func(summary string, err error)

// scheduler 管理两个 ticker、任务互斥与手动触发。
type scheduler struct {
	p *Plugin

	mu      sync.Mutex
	started bool
	stopped bool
	stopCh  chan struct{}

	consolidateRunning bool
	cleanupRunning     bool
	reindexRunning     bool
	memoryCmdRunning   bool
	knowledgeRunning   bool

	consolidateLast time.Time
	cleanupLast     time.Time
	consolidateNext time.Time
	cleanupNext     time.Time
}

func newScheduler(p *Plugin) *scheduler {
	return &scheduler{p: p, stopCh: make(chan struct{})}
}

// start 在索引 ready（或 disabled）后启动 ticker；只执行一次。
func (s *scheduler) start() {
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started = true
	now := s.p.now()
	if s.p.cfg.MemoryConsolidateInterval > 0 {
		s.consolidateNext = now.Add(s.p.cfg.MemoryConsolidateInterval)
	}
	if s.p.cfg.HistoryCleanupInterval > 0 {
		s.cleanupNext = now.Add(s.p.cfg.HistoryCleanupInterval)
	}
	s.mu.Unlock()

	if s.p.cfg.MemoryConsolidateInterval > 0 {
		s.startTicker("consolidate", s.p.cfg.MemoryConsolidateInterval)
	}
	if s.p.cfg.HistoryCleanupInterval > 0 {
		s.startTicker("cleanup", s.p.cfg.HistoryCleanupInterval)
	}
	if s.p.cfg.JobsRunOnStart {
		s.trigger(s.p.ctx, "consolidate", nil)
		s.trigger(s.p.ctx, "cleanup", nil)
	}
}

func (s *scheduler) startTicker(name string, interval time.Duration) {
	s.p.wg.Add(1)
	go func() {
		defer s.p.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.mu.Lock()
				if name == "consolidate" {
					s.consolidateNext = s.p.now().Add(interval)
				} else {
					s.cleanupNext = s.p.now().Add(interval)
				}
				s.mu.Unlock()
				if !s.trigger(s.p.ctx, name, nil) {
					s.p.log.Warn("job busy", "operation", name, "reason", "job_busy")
				}
			case <-s.stopCh:
				return
			case <-s.p.ctx.Done():
				return
			}
		}
	}()
}

// stop 停止 ticker 触发；不等待正在运行的任务。
func (s *scheduler) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.stopped = true
	close(s.stopCh)
}

func (s *scheduler) setRunning(name string, running bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch name {
	case "consolidate":
		s.consolidateRunning = running
		if !running {
			s.consolidateLast = s.p.now()
		}
	case "cleanup":
		s.cleanupRunning = running
		if !running {
			s.cleanupLast = s.p.now()
		}
	case "reindex":
		s.reindexRunning = running
	case "memory":
		s.memoryCmdRunning = running
	case "knowledge":
		s.knowledgeRunning = running
	}
}

// tryExclusive 尝试独占一个任务；成功返回 true。
func (s *scheduler) tryExclusive(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.p.stopping.Load() {
		return false
	}
	switch name {
	case "consolidate":
		if s.consolidateRunning {
			return false
		}
		s.consolidateRunning = true
	case "cleanup":
		if s.cleanupRunning {
			return false
		}
		s.cleanupRunning = true
	case "reindex":
		if s.reindexRunning {
			return false
		}
		s.reindexRunning = true
	case "memory":
		if s.memoryCmdRunning {
			return false
		}
		s.memoryCmdRunning = true
	case "knowledge":
		if s.knowledgeRunning {
			return false
		}
		s.knowledgeRunning = true
	default:
		return false
	}
	return true
}

func (s *scheduler) finish(name string) {
	s.setRunning(name, false)
}

// trigger 触发定时/手动任务；返回 false 表示任务已在运行或已停止。
func (s *scheduler) trigger(ctx context.Context, name string, notify jobNotify) bool {
	if !s.tryExclusive(name) {
		return false
	}
	task := func() {
		defer s.finish(name)
		start := time.Now()
		var taskCtx context.Context
		var cancel context.CancelFunc
		var summary string
		var err error
		switch name {
		case "consolidate":
			taskCtx, cancel = context.WithTimeout(s.p.ctx, s.p.cfg.MemoryConsolidateTimeout)
			summary, err = s.p.runConsolidate(taskCtx)
		case "cleanup":
			taskCtx, cancel = context.WithTimeout(s.p.ctx, historyCleanupTimeout)
			summary, err = s.p.runCleanup(taskCtx)
		default:
			err = errors.New("unknown job")
		}
		if cancel != nil {
			cancel()
		}
		if err != nil {
			s.p.log.Warn("job failed",
				"operation", name, "reason", jobFailureReason(err),
				"duration_ms", time.Since(start).Milliseconds(), "error", err.Error())
		} else {
			s.p.log.Info("job done",
				"operation", name, "duration_ms", time.Since(start).Milliseconds())
		}
		if notify != nil {
			notify(summary, err)
		}
	}
	s.p.deferStart(ctx, func() {
		if !s.p.runBackground(task) {
			s.finish(name)
		}
	})
	return true
}

// status 返回任务状态快照。
func (s *scheduler) status() (consolidateRunning, cleanupRunning bool, consolidateLast, consolidateNext, cleanupLast, cleanupNext time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.consolidateRunning, s.cleanupRunning,
		s.consolidateLast, s.consolidateNext, s.cleanupLast, s.cleanupNext
}

func jobFailureReason(err error) string {
	switch categoryOf(err) {
	case errCanceled:
		return "internal_error"
	case errTimeout:
		return "timeout"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "internal_error"
}

// runCleanup 执行一轮缓存清理。
func (p *Plugin) runCleanup(ctx context.Context) (string, error) {
	now := p.now()
	type target struct {
		s       *sessionState
		hardTTL bool
	}
	var remove []target
	trimmedSessions := 0

	p.sessionsMu.Lock()
	sessions := make([]*sessionState, 0, len(p.sessions))
	for _, s := range p.sessions {
		sessions = append(sessions, s)
	}
	p.sessionsMu.Unlock()

	for _, s := range sessions {
		if ctx.Err() != nil {
			break
		}
		if !s.mu.TryLock() {
			continue
		}
		if s.inflight || len(s.pending) > 0 || s.timer != nil {
			s.mu.Unlock()
			continue
		}
		before := len(s.history)
		p.trimHistoryLocked(s)
		if len(s.history) < before {
			trimmedSessions++
		}
		idle := now.Sub(s.lastActivity)
		consolidated := !p.cfg.MemoryEnabled || s.consolidatedSeq >= s.seq
		hardTTL := false
		shouldRemove := false
		if consolidated {
			if p.cfg.HistoryTTL > 0 && idle > p.cfg.HistoryTTL {
				shouldRemove = true
			}
		} else {
			if p.cfg.HistoryUnconsolidatedTTL > 0 && idle > p.cfg.HistoryUnconsolidatedTTL {
				shouldRemove = true
				hardTTL = true
			}
		}
		if shouldRemove {
			remove = append(remove, target{s: s, hardTTL: hardTTL})
		}
		s.mu.Unlock()
	}

	removed := 0
	for _, t := range remove {
		if t.hardTTL {
			p.log.Warn("history dropped unconsolidated",
				"operation", "cleanup", "reason", "history_dropped_unconsolidated",
				"session", sessionLogPrefix(t.s.key))
		}
		if p.removeSession(t.s.key) != nil {
			removed++
		}
	}

	// LRU 淘汰：优先已整理，再未整理。
	for ctx.Err() == nil {
		p.sessionsMu.Lock()
		if len(p.sessions) <= p.cfg.HistoryMaxSessions {
			p.sessionsMu.Unlock()
			break
		}
		type lruEntry struct {
			s        *sessionState
			last     time.Time
			active   bool
			consolid bool
		}
		list := make([]lruEntry, 0, len(p.sessions))
		for _, s := range p.sessions {
			if !s.mu.TryLock() {
				continue
			}
			list = append(list, lruEntry{
				s:        s,
				last:     s.lastActivity,
				active:   s.inflight || len(s.pending) > 0 || s.timer != nil,
				consolid: !p.cfg.MemoryEnabled || s.consolidatedSeq >= s.seq,
			})
			s.mu.Unlock()
		}
		p.sessionsMu.Unlock()

		sort.Slice(list, func(i, j int) bool { return list[i].last.Before(list[j].last) })
		victim := (*sessionState)(nil)
		for _, e := range list {
			if e.active || !e.consolid {
				continue
			}
			victim = e.s
			break
		}
		if victim == nil {
			for _, e := range list {
				if e.active {
					continue
				}
				victim = e.s
				p.log.Warn("lru evicted unconsolidated session",
					"operation", "cleanup", "reason", "history_dropped_unconsolidated",
					"session", sessionLogPrefix(e.s.key))
				break
			}
		}
		if victim == nil {
			break
		}
		if p.removeSession(victim.key) != nil {
			removed++
		}
	}

	return fmt.Sprintf("sessions_removed=%d histories_trimmed=%d", removed, trimmedSessions), nil
}

// runReindex 重新嵌入 stale 记录；部分失败时保留旧 meta。
func (p *Plugin) runReindex(ctx context.Context) (int, error) {
	state := p.idx.statusState()
	if state != vectorStateReady {
		return 0, errors.New("vector not ready")
	}
	mems, chunks := p.idx.reindexSnapshot()
	if len(mems) == 0 && len(chunks) == 0 {
		return 0, nil
	}
	type target struct {
		memory *memoryEntry
		chunk  *chunkEntry
		text   string
	}
	targets := make([]target, 0, len(mems)+len(chunks))
	for _, m := range mems {
		targets = append(targets, target{memory: m, text: m.Text})
	}
	for _, c := range chunks {
		targets = append(targets, target{chunk: c, text: c.Text})
	}
	texts := make([]string, len(targets))
	for i, t := range targets {
		texts[i] = t.text
	}
	vecs := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += embeddingBatchSize {
		end := start + embeddingBatchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch, err := p.embedQueued(ctx, texts[start:end])
		if err != nil {
			p.log.Warn("reindex embedding failed", "operation", "vector", "error", err.Error())
			break
		}
		if len(batch) != end-start {
			p.log.Warn("reindex embedding count mismatch", "operation", "vector")
			break
		}
		vecs = append(vecs, batch...)
	}
	success := 0
	allOK := len(vecs) == len(targets)
	for i := 0; i < len(vecs); i++ {
		if ctx.Err() != nil {
			break
		}
		vec, err := normalizeVector(vecs[i])
		if err != nil {
			p.log.Warn("reindex vector invalid", "operation", "vector", "error", err.Error())
			allOK = false
			continue
		}
		t := targets[i]
		if t.memory != nil {
			if p.idx.isMemoryDeleted(t.memory.ID) {
				continue
			}
			cur, ok := p.idx.memoryByID(t.memory.ID)
			if !ok || cur.Version != t.memory.Version {
				continue
			}
			rec := *t.memory
			rec.Vector = vec
			rec.Dims = len(vec)
			rec.Model = p.cfg.EmbeddingModel
			if err := p.putRowWait(ctx, memoryRowKey(rec.ID), marshalJSON(memoryRecordOf(&rec))); err != nil {
				p.log.Warn("reindex memory write failed", "operation", "vector", "error", err.Error())
				allOK = false
				continue
			}
			if !p.idx.applyReindexMemory(rec.ID, t.memory.Version, vec, p.cfg.EmbeddingModel) {
				// 并发合并或删除：用当前内存状态恢复行，避免旧快照覆盖新写入。
				if current, exists := p.idx.memoryByID(rec.ID); exists {
					if err := p.putRowWait(ctx, memoryRowKey(current.ID), marshalJSON(memoryRecordOf(current))); err != nil {
						p.log.Warn("reindex restore failed", "operation", "vector", "error", err.Error())
					}
				} else if p.idx.isMemoryDeleted(rec.ID) {
					if err := p.deleteRowWait(ctx, memoryRowKey(rec.ID)); err != nil {
						p.log.Warn("reindex deleted memory cleanup failed",
							"operation", "vector", "error", err.Error())
					}
				}
				continue
			}
			success++
			continue
		}
		if _, ok := p.idx.chunkByID(t.chunk.ID); !ok {
			continue
		}
		chunk := *t.chunk
		chunk.Vector = vec
		chunk.Dims = len(vec)
		chunk.Model = p.cfg.EmbeddingModel
		if err := p.putRowWait(ctx, chunkRowKey(chunk.ID), marshalJSON(chunkRecordOf(&chunk))); err != nil {
			p.log.Warn("reindex chunk write failed", "operation", "vector", "error", err.Error())
			allOK = false
			continue
		}
		if !p.idx.applyReindexChunk(chunk.ID, vec, p.cfg.EmbeddingModel) {
			continue
		}
		success++
	}
	if allOK && success == len(targets) {
		dims := p.cfg.EmbeddingDimensions
		if dims == 0 {
			if len(vecs) > 0 {
				norm, err := normalizeVector(vecs[0])
				if err == nil {
					dims = len(norm)
				}
			}
		}
		meta := metaJSON{Version: 1, Model: p.cfg.EmbeddingModel, Dims: dims}
		if err := p.putRowWait(ctx, tableMetaKey, marshalJSON(meta)); err != nil {
			p.log.Warn("reindex meta write failed", "operation", "vector", "error", err.Error())
		} else {
			p.idx.setMeta(meta)
		}
	}
	return success, nil
}
