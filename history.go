package chatagent

import (
	"strings"
	"time"
	"unicode"
)

// Turn 是一条会话历史记录。
type Turn struct {
	Seq       uint64
	Time      time.Time
	UserID    string
	UserName  string
	Text      string
	Images    []string
	IsSelf    bool
	EventID   string
	MessageID string
	IsBot     bool
}

// replyFingerprint 是机器人回复文本与时间的去重记录。
type replyFingerprint struct {
	Text string
	At   time.Time
}

// senderMark 是活动窗口内的非机器人发送者标记。
type senderMark struct {
	ID    string
	Name  string
	IsBot bool
	At    time.Time
}

const (
	// recentReplyIDLimit 是引用寻址 ID 环容量。
	recentReplyIDLimit = 128
	// replyDedupeWindow 是回复文本去重时间窗。
	replyDedupeWindow = 10 * time.Minute
	// replyDedupeLimit 是参与去重的最近回复条数。
	replyDedupeLimit = 5
)

// appendUserTurn 追加一条普通用户消息；messageID 与 bot 标记由生产路径补充。
func (p *Plugin) appendUserTurn(s *sessionState, text string, images []string, evUserID, evUserName, eventID string, at time.Time) Turn {
	return p.appendUserTurnWithMeta(s, text, images, evUserID, evUserName, eventID, "", false, at)
}

// appendUserTurnWithMeta 在会话历史中追加一条用户消息，并返回该 Turn。
// 调用方必须保证会话未禁用且消息已通过准入。
func (p *Plugin) appendUserTurnWithMeta(s *sessionState, text string, images []string, evUserID, evUserName, eventID, messageID string, isBot bool, at time.Time) Turn {
	if at.IsZero() {
		at = p.now()
	}
	at = at.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	turn := Turn{
		Seq:       s.seq,
		Time:      at,
		UserID:    evUserID,
		UserName:  evUserName,
		Text:      text,
		Images:    append([]string(nil), images...),
		EventID:   eventID,
		MessageID: messageID,
		IsBot:     isBot,
	}
	s.seq++
	s.history = append(s.history, turn)
	p.trimHistoryLocked(s)
	s.lastActivity = at
	p.markSenderLocked(s, evUserID, evUserName, isBot, at)
	return turn
}

// appendSelfTurn 记录一次发送成功的机器人回复。
func (p *Plugin) appendSelfTurn(s *sessionState, text, userID, eventID string, random bool) {
	now := p.now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	turn := Turn{
		Seq:       s.seq,
		Time:      now,
		UserID:    userID,
		UserName:  p.cfg.BotName,
		Text:      text,
		IsSelf:    true,
		EventID:   eventID,
		MessageID: eventID,
	}
	s.seq++
	s.history = append(s.history, turn)
	p.trimHistoryLocked(s)
	s.lastActivity = now
	s.lastReplyAt = now
	if eventID != "" {
		s.recentReplyIDs = append(s.recentReplyIDs, eventID)
		if len(s.recentReplyIDs) > recentReplyIDLimit {
			s.recentReplyIDs = append([]string(nil), s.recentReplyIDs[len(s.recentReplyIDs)-recentReplyIDLimit:]...)
		}
	}
	s.recentReplyTexts = append(s.recentReplyTexts, replyFingerprint{Text: text, At: now})
	if len(s.recentReplyTexts) > replyDedupeLimit {
		s.recentReplyTexts = append([]replyFingerprint(nil), s.recentReplyTexts[len(s.recentReplyTexts)-replyDedupeLimit:]...)
	}
	if random {
		s.hourly = append(s.hourly, now)
	}
	p.pruneHourlyLocked(s, now)
}

// trimHistoryLocked 按条数与字符数裁剪历史；调用方必须持有 s.mu。
func (p *Plugin) trimHistoryLocked(s *sessionState) {
	removed := false
	var maxSeq uint64
	for len(s.history) > p.cfg.HistoryMaxMessages {
		maxSeq = s.history[0].Seq
		s.history = s.history[1:]
		removed = true
	}
	total := 0
	for _, t := range s.history {
		total += runeLen(t.Text)
	}
	for len(s.history) > 0 && total > p.cfg.HistoryMaxChars {
		first := s.history[0]
		if first.Seq > maxSeq {
			maxSeq = first.Seq
		}
		total -= runeLen(first.Text)
		s.history = s.history[1:]
		removed = true
	}
	if !removed {
		return
	}
	if s.consolidatedSeq <= maxSeq {
		if next := maxSeq + 1; next > s.lostSeq {
			s.lostSeq = next
		}
		p.log.Warn("history trimmed",
			"operation", "message",
			"reason", "history_trimmed",
			"session", sessionLogPrefix(s.key),
			"lost_seq", s.lostSeq,
		)
	}
}

// historySnapshot 返回全部历史的快照。
func (s *sessionState) historySnapshot() []Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Turn, len(s.history))
	for i, t := range s.history {
		t.Images = append([]string(nil), t.Images...)
		out[i] = t
	}
	return out
}

// isDuplicateReply 判断文本是否与最近 10 分钟内最近 5 条回复重复。
func (s *sessionState) isDuplicateReply(text string, now time.Time) bool {
	norm := normalizeReplyText(text)
	s.mu.Lock()
	defer s.mu.Unlock()
	start := len(s.recentReplyTexts) - replyDedupeLimit
	if start < 0 {
		start = 0
	}
	for _, f := range s.recentReplyTexts[start:] {
		if now.Sub(f.At) > replyDedupeWindow {
			continue
		}
		if normalizeReplyText(f.Text) == norm {
			return true
		}
	}
	return false
}

// hasRecentReplyID 判断消息 ID 是否在本会话的机器人发送 ID 环中。
func (s *sessionState) hasRecentReplyID(id string) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.recentReplyIDs {
		if x == id {
			return true
		}
	}
	return false
}

// markSenderLocked 记录活动发送者及其 bot 标记；调用方必须持有 s.mu。
func (p *Plugin) markSenderLocked(s *sessionState, id, name string, isBot bool, now time.Time) {
	if id == "" {
		return
	}
	p.pruneSendersLocked(s, now)
	for i := range s.recentSenders {
		if s.recentSenders[i].ID == id {
			s.recentSenders[i].Name = name
			s.recentSenders[i].IsBot = isBot
			s.recentSenders[i].At = now
			return
		}
	}
	s.recentSenders = append(s.recentSenders, senderMark{ID: id, Name: name, IsBot: isBot, At: now})
}

// pruneSendersLocked 丢弃活动窗口外的发送者。
func (p *Plugin) pruneSendersLocked(s *sessionState, now time.Time) {
	if len(s.recentSenders) == 0 {
		return
	}
	window := p.cfg.RandomActivityWindow
	out := s.recentSenders[:0]
	for _, m := range s.recentSenders {
		if now.Sub(m.At) <= window {
			out = append(out, m)
		}
	}
	s.recentSenders = out
}

// pruneHourlyLocked 丢弃一小时之前的随机插话时间。
func (p *Plugin) pruneHourlyLocked(s *sessionState, now time.Time) {
	if len(s.hourly) == 0 {
		return
	}
	out := s.hourly[:0]
	for _, t := range s.hourly {
		if now.Sub(t) <= time.Hour {
			out = append(out, t)
		}
	}
	s.hourly = out
}

// participantCountLocked 返回活动窗口内不同非机器人发送者数。
func (s *sessionState) participantCountLocked() int {
	seen := make(map[string]struct{}, len(s.recentSenders))
	for _, m := range s.recentSenders {
		if m.IsBot {
			continue
		}
		seen[m.ID] = struct{}{}
	}
	return len(seen)
}

// randomCountLastHourLocked 返回最近一小时随机插话次数。
func (s *sessionState) randomCountLastHourLocked(now time.Time) int {
	n := 0
	for _, t := range s.hourly {
		if now.Sub(t) <= time.Hour {
			n++
		}
	}
	return n
}

// normalizeReplyText 规范化回复文本用于去重。
func normalizeReplyText(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}
