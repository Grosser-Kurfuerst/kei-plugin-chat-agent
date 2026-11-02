package chatagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// timerHandle 是批处理定时器的可注入句柄。
type timerHandle interface {
	Stop() bool
}

// timerFunc 创建定时器；测试可替换为手动触发的实现。
type timerFunc func(interval time.Duration, fn func()) timerHandle

func defaultAfterFunc(interval time.Duration, fn func()) timerHandle {
	return time.AfterFunc(interval, fn)
}

// sessionState 是单个会话的全部内存状态。
type sessionState struct {
	mu sync.Mutex

	key         string
	storageKey  string
	kind        string
	platform    string
	botID       string
	channelID   string
	peerUserID  string
	channelName string
	peerName    string

	history         []Turn
	lastActivity    time.Time
	disabled        bool
	seq             uint64
	consolidatedSeq uint64
	lostSeq         uint64
	epoch           uint64

	timer         timerHandle
	timerSeq      uint64
	firstAt       time.Time
	nextBatchAt   time.Time
	pending       []Turn
	pendingRandom bool
	inflight      bool

	lastReplyAt      time.Time
	hourly           []time.Time
	recentReplyIDs   []string
	recentReplyTexts []replyFingerprint
	recentSenders    []senderMark

	overrideLoading bool
	overrideLoaded  bool
	overrideClosed  bool
	overrideDone    chan struct{}
}

// sessionKey 返回内存会话键。
func sessionKey(platform, botID, kind, channelID, userID string) string {
	if kind == "private" {
		return platform + ":" + botID + ":user:" + userID
	}
	return platform + ":" + botID + ":" + channelID
}

// sessionChannelKey 返回 Storage 哈希使用的频道键。
func sessionChannelKey(kind, channelID, userID string) string {
	if kind == "private" {
		return "user:" + userID
	}
	return channelID
}

// sessionLogPrefix 返回会话键哈希的前 6 位，用于日志。
func sessionLogPrefix(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:6]
}

// getSession 查找会话，不创建。
func (p *Plugin) getSession(key string) *sessionState {
	p.sessionsMu.Lock()
	defer p.sessionsMu.Unlock()
	return p.sessions[key]
}

// getOrCreateSession 返回或创建会话；新会话会异步加载 disabled 覆盖。
func (p *Plugin) getOrCreateSession(ev *bot.Event) *sessionState {
	kind := eventKind(ev)
	if kind == "" {
		return nil
	}
	platform, botID := ev.Platform, ev.BotID
	channelID, userID := "", ""
	if ev.Channel != nil {
		channelID = ev.Channel.ID
	}
	if ev.Sender != nil {
		userID = ev.Sender.ID
	}
	channelName := ""
	if ev.Channel != nil {
		channelName = ev.Channel.Name
	}
	peerName := ""
	if ev.Sender != nil {
		peerName = ev.Sender.Name
	}
	key := sessionKey(platform, botID, kind, channelID, userID)

	p.sessionsMu.Lock()
	if s := p.sessions[key]; s != nil {
		s.mu.Lock()
		if channelName != "" {
			s.channelName = channelName
		}
		if peerName != "" {
			s.peerName = peerName
		}
		s.mu.Unlock()
		p.sessionsMu.Unlock()
		return s
	}
	s := &sessionState{
		key:          key,
		storageKey:   sessionStorageKey(platform, botID, sessionChannelKey(kind, channelID, userID)),
		kind:         kind,
		platform:     platform,
		botID:        botID,
		channelID:    channelID,
		peerUserID:   userID,
		channelName:  channelName,
		peerName:     peerName,
		overrideDone: make(chan struct{}),
	}
	p.sessions[key] = s
	p.sessionsMu.Unlock()

	if p.stopping.Load() {
		s.mu.Lock()
		s.overrideLoaded = true
		s.closeOverrideLocked()
		s.mu.Unlock()
		return s
	}
	p.loadSessionOverride(s)
	return s
}

// loadSessionOverride 异步读取 session KV 覆盖。
func (p *Plugin) loadSessionOverride(s *sessionState) {
	s.mu.Lock()
	if s.overrideLoading || s.overrideLoaded {
		s.mu.Unlock()
		return
	}
	s.overrideLoading = true
	epoch := s.epoch
	s.mu.Unlock()

	started := p.runBackground(func() {
		raw, err := p.readKey(p.ctx, s.storageKey)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.overrideLoading = false
		s.overrideLoaded = true
		if s.overrideClosed || s.epoch != epoch {
			s.closeOverrideLocked()
			return
		}
		if err != nil {
			p.log.Warn("session override load failed",
				"operation", "storage", "session", sessionLogPrefix(s.key), "error", err.Error())
		} else if len(raw) > 0 {
			var v struct {
				Disabled  bool   `json:"disabled"`
				UpdatedAt string `json:"updated_at"`
			}
			if jerr := json.Unmarshal(raw, &v); jerr != nil {
				p.log.Warn("session override corrupt",
					"operation", "storage", "session", sessionLogPrefix(s.key), "error", jerr.Error())
			} else {
				s.disabled = v.Disabled
			}
		}
		s.closeOverrideLocked()
	})
	if !started {
		s.mu.Lock()
		s.overrideLoading = false
		s.overrideLoaded = true
		s.closeOverrideLocked()
		s.mu.Unlock()
	}
}

// waitOverride 在生成前等待覆盖加载，最多 1s。
func (p *Plugin) waitOverride(s *sessionState) {
	timeout := time.Second
	s.mu.Lock()
	if s.overrideLoaded {
		s.mu.Unlock()
		return
	}
	done := s.overrideDone
	s.mu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		p.log.Warn("session override wait timeout",
			"operation", "storage", "session", sessionLogPrefix(s.key))
	case <-p.ctx.Done():
	}
}

// closeOverrideLocked 关闭覆盖加载通道；调用方必须持有 s.mu。
func (s *sessionState) closeOverrideLocked() {
	if s.overrideClosed {
		return
	}
	s.overrideClosed = true
	close(s.overrideDone)
}

// setDisabled 设置当前会话的 disabled 覆盖。
func (p *Plugin) setDisabled(s *sessionState, disabled bool) {
	now := p.now().UTC()
	s.mu.Lock()
	s.disabled = disabled
	s.overrideLoaded = true
	s.overrideLoading = false
	s.closeOverrideLocked()
	s.lastActivity = now
	s.mu.Unlock()
	p.kvSet(s.storageKey, marshalJSON(sessionJSON{
		Disabled:  disabled,
		UpdatedAt: now.Format(rfc3339Nano),
	}))
}

// resetDisabled 删除会话覆盖并恢复 enabled。
func (p *Plugin) resetDisabled(s *sessionState) {
	now := p.now().UTC()
	s.mu.Lock()
	s.disabled = false
	s.overrideLoaded = true
	s.overrideLoading = false
	s.closeOverrideLocked()
	s.lastActivity = now
	s.mu.Unlock()
	p.kvDelete(s.storageKey)
}

// sessionJSON 是 `session:<hash>` KV 的 JSON 形状。
type sessionJSON struct {
	Disabled  bool   `json:"disabled"`
	UpdatedAt string `json:"updated_at"`
}

// eventKind 返回事件对应的会话类型；只处理群聊与私聊。
func eventKind(ev *bot.Event) string {
	if ev == nil || ev.Message == nil {
		return ""
	}
	switch ev.Message.Kind {
	case bot.MessageGroup:
		return "group"
	case bot.MessagePrivate:
		return "private"
	default:
		return ""
	}
}

// removeSession 删除会话并失效其 timer/pending。
func (p *Plugin) removeSession(key string) *sessionState {
	p.sessionsMu.Lock()
	s := p.sessions[key]
	delete(p.sessions, key)
	p.sessionsMu.Unlock()
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.pending = nil
	s.firstAt = time.Time{}
	s.nextBatchAt = time.Time{}
	s.epoch++
	s.closeOverrideLocked()
	s.mu.Unlock()
	return s
}

// invalidateAllSessions 停止全部 timer、清空 pending 并递增 epoch。
func (p *Plugin) invalidateAllSessions() {
	p.sessionsMu.Lock()
	sessions := make([]*sessionState, 0, len(p.sessions))
	for _, s := range p.sessions {
		sessions = append(sessions, s)
	}
	p.sessionsMu.Unlock()
	for _, s := range sessions {
		s.mu.Lock()
		if s.timer != nil {
			s.timer.Stop()
			s.timer = nil
		}
		s.pending = nil
		s.firstAt = time.Time{}
		s.nextBatchAt = time.Time{}
		s.epoch++
		s.closeOverrideLocked()
		s.mu.Unlock()
	}
}

// sessionTarget 由会话状态构造发送目标。
func sessionTarget(s *sessionState) bot.Target {
	if s.kind == "private" {
		return bot.Target{
			Platform: s.platform,
			BotID:    s.botID,
			Kind:     bot.MessagePrivate,
			UserID:   s.peerUserID,
		}
	}
	return bot.Target{
		Platform:  s.platform,
		BotID:     s.botID,
		Kind:      bot.MessageGroup,
		ChannelID: s.channelID,
	}
}

// sessionTargetFromEvent 由命令事件构造发送目标。
func sessionTargetFromEvent(ev *bot.Event) bot.Target {
	t := bot.Target{Platform: ev.Platform, BotID: ev.BotID}
	if ev.Message != nil {
		t.Kind = ev.Message.Kind
	}
	switch t.Kind {
	case bot.MessagePrivate:
		if ev.Sender != nil {
			t.UserID = ev.Sender.ID
		}
	default:
		t.Kind = bot.MessageGroup
		if ev.Channel != nil {
			t.ChannelID = ev.Channel.ID
		}
	}
	return t
}

// sendText 向目标发送纯文本；Stop 后拒绝发送。
func (p *Plugin) sendText(ctx context.Context, target bot.Target, text string) error {
	if p.stopping.Load() {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	msg := &bot.Message{Kind: target.Kind, Segments: []bot.Segment{
		{Type: bot.SegText, Data: map[string]any{bot.KeyText: text}},
	}}
	_, err := p.bot.Send(ctx, target, msg)
	return err
}
