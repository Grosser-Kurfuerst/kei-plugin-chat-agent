package chatagent

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// handleGroupMessage 处理群聊消息。
func (p *Plugin) handleGroupMessage(ctx context.Context, ev *bot.Event, r bot.Reply) error {
	return p.handleMessage(ctx, ev, "group")
}

// handlePrivateMessage 处理私聊消息。
func (p *Plugin) handlePrivateMessage(ctx context.Context, ev *bot.Event, r bot.Reply) error {
	return p.handleMessage(ctx, ev, "private")
}

// handleMessage 执行 7.1 的固定判定顺序；Handler 只做内存操作。
func (p *Plugin) handleMessage(ctx context.Context, ev *bot.Event, kind string) error {
	start := time.Now()
	text, images, hasText, hasImage := renderInboundMessage(ev, p.cfg.VisionEnabled)
	if !hasText && !hasImage {
		p.logDecision(ev, kind, "", "empty_text", start)
		return nil
	}
	if ev.Sender == nil {
		p.logDecision(ev, kind, "", "no_sender", start)
		return nil
	}
	selfIDs := p.normalizedSelfIDs(ev.Platform)
	if p.cfg.IgnoreBots && ev.Sender.IsBot {
		p.logDecision(ev, kind, "", "bot_sender", start)
		return nil
	}
	if _, ok := selfIDs[ev.Sender.ID]; ok {
		p.logDecision(ev, kind, "", "self_sender", start)
		return nil
	}
	if ev.Command != nil && !p.cfg.RespondToCmd {
		p.logDecision(ev, kind, "", "command", start)
		return nil
	}
	if ev.Command != nil && (ev.Command.Name == "chat" || ev.Command.Name == "chat-admin") {
		p.logDecision(ev, kind, "", "command", start)
		return nil
	}
	policyID := ""
	if kind == "private" {
		policyID = ev.Sender.ID
	} else if ev.Channel != nil {
		policyID = ev.Channel.ID
	}
	if !p.policy.allows(kind, ev.Platform, policyID) {
		p.logDecision(ev, kind, "", "not_allowed", start)
		return nil
	}
	imageUsable := p.cfg.VisionEnabled && hasImage
	if runeLen(strings.TrimSpace(text)) < p.cfg.TriggerMinLen && !imageUsable {
		p.logDecision(ev, kind, "", "too_short", start)
		return nil
	}

	s := p.getOrCreateSession(ev)
	if s == nil {
		return nil
	}
	s.mu.Lock()
	disabled := s.disabled
	s.mu.Unlock()
	if disabled {
		p.logDecision(ev, kind, s.key, "channel_off", start)
		return nil
	}

	turn := p.appendUserTurn(s, text, images, ev.Sender.ID, ev.Sender.Name, ev.ID, ev.Time)
	now := p.now()
	addressed := kind == "private" || p.isAddressed(s, ev, text, selfIDs)

	if !addressed {
		if reason := p.maybeRandom(s, now, turn); reason != "" {
			p.logDecision(ev, kind, s.key, reason, start)
		}
		return nil
	}

	s.mu.Lock()
	if !s.lastReplyAt.IsZero() && now.Sub(s.lastReplyAt) < p.cfg.MentionMinInterval {
		s.mu.Unlock()
		p.logDecision(ev, kind, s.key, "cooldown", start)
		return nil
	}
	if !roll(p.randFloat, p.cfg.MentionReplyProbability) {
		s.mu.Unlock()
		p.logDecision(ev, kind, s.key, "probability", start)
		return nil
	}
	if s.inflight {
		s.pending = append(s.pending, turn)
		s.mu.Unlock()
		return nil
	}
	p.startBatchLocked(s, turn, false, now)
	s.mu.Unlock()
	return nil
}

// maybeRandom 评估随机参与；返回首个拒绝 reason，空串表示无需记录或已创建批次。
func (p *Plugin) maybeRandom(s *sessionState, now time.Time, turn Turn) string {
	s.mu.Lock()
	if s.inflight || s.timer != nil || len(s.pending) > 0 {
		s.mu.Unlock()
		return ""
	}
	if reason := p.randomRejectLocked(s, now); reason != "" {
		s.mu.Unlock()
		return reason
	}
	p.startBatchLocked(s, turn, true, now)
	s.mu.Unlock()
	return ""
}

// randomRejectLocked 按 7.3 的顺序返回首个失败 reason；空串表示通过。
func (p *Plugin) randomRejectLocked(s *sessionState, now time.Time) string {
	if !p.cfg.RandomEnabled {
		return "not_addressed"
	}
	if p.cfg.RandomMaxPerHour == 0 {
		return "hour_quota"
	}
	if !s.lastReplyAt.IsZero() && now.Sub(s.lastReplyAt) < p.cfg.RandomCooldown {
		return "cooldown"
	}
	if p.cfg.inQuietHours(now) {
		return "quiet_hours"
	}
	if s.randomCountLastHourLocked(now) >= p.cfg.RandomMaxPerHour {
		return "hour_quota"
	}
	if s.participantCountLocked() < p.cfg.RandomMinParticipants {
		return "min_participants"
	}
	if !roll(p.randFloat, p.cfg.RandomProbability) {
		return "probability"
	}
	return ""
}

func roll(randFloat func() float64, probability float64) bool {
	if probability <= 0 {
		return false
	}
	if probability >= 1 {
		return true
	}
	return randFloat() < probability
}

// startBatchLocked 把消息加入 pending 并重置批处理 timer。
func (p *Plugin) startBatchLocked(s *sessionState, turn Turn, random bool, now time.Time) {
	if len(s.pending) == 0 {
		s.firstAt = now
	}
	s.pending = append(s.pending, turn)
	if !random {
		s.pendingRandom = false
	} else if len(s.pending) == 1 {
		s.pendingRandom = true
	}
	deadline := s.firstAt.Add(p.cfg.BatchMaxWindow)
	delay := p.cfg.BatchWindow
	if remaining := deadline.Sub(now); remaining < delay {
		delay = remaining
	}
	if !s.nextBatchAt.IsZero() {
		if forced := s.nextBatchAt.Sub(now); forced < delay {
			delay = forced
		}
	}
	if delay < 0 {
		delay = 0
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	epoch := s.epoch
	s.timer = p.afterFunc(delay, func() {
		p.onBatchTimer(s, epoch)
	})
}

// onBatchTimer 取走 pending 并调度生成。
func (p *Plugin) onBatchTimer(s *sessionState, epoch uint64) {
	s.mu.Lock()
	if s.epoch != epoch {
		s.mu.Unlock()
		p.log.Warn("batch timer stale", "operation", "generate", "reason", "stale",
			"session", sessionLogPrefix(s.key))
		return
	}
	s.timer = nil
	if s.inflight {
		s.mu.Unlock()
		return
	}
	if len(s.pending) == 0 {
		s.firstAt = time.Time{}
		s.nextBatchAt = time.Time{}
		s.mu.Unlock()
		return
	}
	batch := s.pending
	random := s.pendingRandom
	s.pending = nil
	s.pendingRandom = false
	s.firstAt = time.Time{}
	s.nextBatchAt = time.Time{}
	s.mu.Unlock()
	sort.Slice(batch, func(i, j int) bool { return batch[i].Seq < batch[j].Seq })

	select {
	case p.sem <- struct{}{}:
	default:
		p.log.Warn("generation semaphore full", "operation", "generate", "reason", "semaphore_full",
			"session", sessionLogPrefix(s.key))
		return
	}

	s.mu.Lock()
	if s.epoch != epoch {
		s.mu.Unlock()
		<-p.sem
		p.log.Warn("batch stale after acquire", "operation", "generate", "reason", "stale",
			"session", sessionLogPrefix(s.key))
		return
	}
	s.inflight = true
	s.mu.Unlock()

	if !p.runBackground(func() { p.generate(s, batch, random, epoch) }) {
		<-p.sem
		s.mu.Lock()
		s.inflight = false
		s.mu.Unlock()
	}
}

// generate 执行一次后台生成与发送。
func (p *Plugin) generate(s *sessionState, batch []Turn, random bool, epoch uint64) {
	start := time.Now()
	defer func() { <-p.sem }()
	defer p.finishGeneration(s, epoch)

	p.waitOverride(s)
	s.mu.Lock()
	if s.epoch != epoch {
		s.mu.Unlock()
		p.log.Warn("generation stale", "operation", "generate", "reason", "stale",
			"session", sessionLogPrefix(s.key))
		return
	}
	if s.disabled {
		s.mu.Unlock()
		p.log.Warn("generation skipped", "operation", "generate", "reason", "channel_off",
			"session", sessionLogPrefix(s.key))
		return
	}
	if len(batch) == 0 {
		s.mu.Unlock()
		return
	}
	firstSeq := batch[0].Seq
	history := make([]Turn, 0, len(s.history))
	for _, t := range s.history {
		if t.Seq >= firstSeq {
			continue
		}
		t.Images = append([]string(nil), t.Images...)
		history = append(history, t)
	}
	s.mu.Unlock()

	query := ragQueryFromBatch(batch)
	rag := p.retrieveRAG(p.ctx, s, batch, query)
	selectedHistory := selectHistory(history, p.cfg)
	imageTurns := make([]Turn, 0, len(selectedHistory)+len(batch))
	imageTurns = append(imageTurns, selectedHistory...)
	imageTurns = append(imageTurns, batch...)
	attachments := p.fetchImagesForChat(p.ctx, imageTurns)

	last := batch[len(batch)-1]
	system := systemContent(
		p.renderSystemPromptWithSession(s, last),
		rag.MemoryBlock,
		rag.KnowledgeBlock,
	)
	content := buildUserContent(s.kind, selectedHistory, batch, attachments)
	req := ChatRequest{
		Model: p.cfg.LLMModel,
		Messages: []ChatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: content},
		},
		Temperature: p.cfg.LLMTemperature,
		MaxTokens:   p.cfg.LLMMaxTokens,
	}
	result, err := p.chat.Complete(p.ctx, req)
	if err != nil {
		p.logGenerate(s, last, "llm_error", rag.Status, start)
		return
	}
	text, skipped, empty := cleanReply(result.Content, random, p.cfg)
	if skipped {
		p.logGenerate(s, last, "skipped_by_llm", rag.Status, start)
		return
	}
	if empty {
		p.logGenerate(s, last, "empty_reply", rag.Status, start)
		return
	}
	if p.cfg.ReplyDedupe && s.isDuplicateReply(text, p.now()) {
		p.logGenerate(s, last, "duplicate_reply", rag.Status, start)
		return
	}
	text = truncateRunes(text, p.cfg.ReplyMaxChars)

	segments := make([]bot.Segment, 0, 3)
	if p.cfg.ReplyMentionSender && s.kind != "private" && last.UserID != "" {
		segments = append(segments, bot.Segment{Type: bot.SegAt, Data: map[string]any{bot.KeyUserID: last.UserID}})
	}
	segments = append(segments, bot.Segment{Type: bot.SegText, Data: map[string]any{bot.KeyText: text}})
	if p.cfg.ReplyQuote && last.EventID != "" {
		segments = append(segments, bot.Segment{Type: bot.SegReply, Data: map[string]any{bot.KeyMessageID: last.EventID}})
	}
	msg := &bot.Message{Kind: s.kindMessageKind(), Segments: segments}
	s.mu.Lock()
	disabled := s.disabled
	s.mu.Unlock()
	if disabled {
		p.logGenerate(s, last, "channel_off", rag.Status, start)
		return
	}
	if p.stopping.Load() || p.ctx.Err() != nil {
		p.logGenerate(s, last, "send_error", rag.Status, start)
		return
	}
	res, err := p.bot.Send(p.ctx, sessionTarget(s), msg)
	if err != nil {
		p.logGenerate(s, last, "send_error", rag.Status, start)
		return
	}
	messageID := ""
	if res != nil {
		messageID = res.MessageID
	}
	selfID := p.selfUserID(s.platform)
	p.appendSelfTurn(s, text, selfID, messageID, random)
	p.logGenerate(s, last, "", rag.Status, start)
}

// finishGeneration 结束在途状态并在有 pending 时 100ms 后触发下一批。
func (p *Plugin) finishGeneration(s *sessionState, epoch uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inflight = false
	if s.epoch != epoch {
		return
	}
	if len(s.pending) == 0 || s.timer != nil {
		return
	}
	s.firstAt = p.now()
	s.nextBatchAt = s.firstAt.Add(100 * time.Millisecond)
	s.timer = p.afterFunc(100*time.Millisecond, func() {
		p.onBatchTimer(s, epoch)
	})
}

func (s *sessionState) kindMessageKind() bot.MessageKind {
	if s.kind == "private" {
		return bot.MessagePrivate
	}
	return bot.MessageGroup
}

// isAddressed 判断是否被 @、引用或关键词寻址。
func (p *Plugin) isAddressed(s *sessionState, ev *bot.Event, text string, selfIDs map[string]struct{}) bool {
	if ev.Message == nil {
		return false
	}
	for _, seg := range ev.Message.Segments {
		switch seg.Type {
		case bot.SegAt:
			uid := bot.SegmentData(seg, bot.KeyUserID)
			if uid == "" || uid == "all" {
				continue
			}
			if len(selfIDs) == 0 {
				return true
			}
			if _, ok := selfIDs[uid]; ok {
				return true
			}
		case bot.SegReply:
			if s.hasRecentReplyID(bot.SegmentData(seg, bot.KeyMessageID)) {
				return true
			}
		}
	}
	for _, kw := range p.cfg.TriggerWords {
		if keywordsMatch(text, kw) {
			return true
		}
	}
	return false
}

func keywordsMatch(text, keyword string) bool {
	if keyword == "" {
		return false
	}
	return strings.Contains(strings.ToLower(text), strings.ToLower(keyword))
}

// normalizedSelfIDs 把配置条目的 platform:id 规范化为当前平台的裸 ID。
func (p *Plugin) normalizedSelfIDs(platform string) map[string]struct{} {
	out := make(map[string]struct{}, len(p.cfg.SelfIDs))
	for _, entry := range p.cfg.SelfIDs {
		if p, id, ok := strings.Cut(entry, ":"); ok {
			if p != platform || id == "" {
				continue
			}
			out[id] = struct{}{}
			continue
		}
		out[entry] = struct{}{}
	}
	return out
}

// selfUserID 返回当前平台的机器人自身用户 ID。
func (p *Plugin) selfUserID(platform string) string {
	for _, entry := range p.cfg.SelfIDs {
		if p, id, ok := strings.Cut(entry, ":"); ok {
			if p == platform {
				return id
			}
			continue
		}
		return entry
	}
	return ""
}

// renderInboundMessage 把消息段渲染为历史文本与图片 URL 列表。
func renderInboundMessage(ev *bot.Event, vision bool) (text string, images []string, hasText bool, hasImage bool) {
	if ev == nil || ev.Message == nil {
		return "", nil, false, false
	}
	var b strings.Builder
	for _, seg := range ev.Message.Segments {
		switch seg.Type {
		case bot.SegText, bot.SegMarkdown:
			s := bot.SegmentData(seg, bot.KeyText)
			b.WriteString(s)
			if strings.TrimSpace(s) != "" {
				hasText = true
			}
		case bot.SegImage:
			b.WriteString("[图片]")
			if vision {
				url := bot.SegmentData(seg, bot.KeyURL)
				if url != "" {
					images = append(images, url)
					hasImage = true
				}
			}
		case bot.SegFace:
			b.WriteString("[表情]")
		case bot.SegFile:
			b.WriteString("[文件]")
		case bot.SegCard:
			b.WriteString("[卡片]")
		case bot.SegReply:
			b.WriteString("[引用]")
		case bot.SegAt:
			// @ 段只参与寻址，不进入历史文本。
		default:
			b.WriteString("[消息]")
		}
	}
	return b.String(), images, hasText, hasImage
}

func (p *Plugin) logDecision(ev *bot.Event, kind, session, reason string, start time.Time) {
	fields := []any{
		"operation", "message",
		"reason", reason,
		"kind", kind,
		"platform", eventPlatform(ev),
		"duration_ms", time.Since(start).Milliseconds(),
	}
	if session != "" {
		fields = append(fields, "session", sessionLogPrefix(session))
	}
	if ev != nil && ev.Sender != nil {
		fields = append(fields, "user", ev.Sender.ID)
	}
	if ev != nil && ev.Channel != nil {
		fields = append(fields, "channel", ev.Channel.ID)
	}
	if ev != nil {
		fields = append(fields, "event_id", ev.ID)
	}
	p.log.Info("message decision", fields...)
}

func (p *Plugin) logGenerate(s *sessionState, last Turn, reason, ragStatus string, start time.Time) {
	fields := []any{
		"operation", "generate",
		"reason", reason,
		"rag_status", ragStatus,
		"session", sessionLogPrefix(s.key),
		"kind", s.kind,
		"platform", s.platform,
		"user", last.UserID,
		"channel", s.channelID,
		"duration_ms", time.Since(start).Milliseconds(),
	}
	p.log.Info("generation", fields...)
}

func eventPlatform(ev *bot.Event) string {
	if ev == nil {
		return ""
	}
	return ev.Platform
}
