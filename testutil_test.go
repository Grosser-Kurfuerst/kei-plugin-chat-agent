package chatagent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// memStore 是并发安全的 Storage + TableStorage 测试桩。
type memStore struct {
	mu        sync.Mutex
	kv        map[string][]byte
	tables    map[string]map[string][]byte
	failSet   bool
	failRow   bool
	createErr error
	listErr   error
}

func newMemStore() *memStore {
	return &memStore{kv: map[string][]byte{}, tables: map[string]map[string][]byte{}}
}

func (s *memStore) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.kv[key]
	if !ok {
		return nil, bot.ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

func (s *memStore) Set(_ context.Context, key string, value []byte, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failSet {
		return errors.New("set failed")
	}
	s.kv[key] = append([]byte(nil), value...)
	return nil
}

func (s *memStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.kv, key)
	return nil
}

func (s *memStore) CreateTable(_ context.Context, table string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return s.createErr
	}
	if _, ok := s.tables[table]; ok {
		return bot.ErrTableExists
	}
	s.tables[table] = map[string][]byte{}
	return nil
}

func (s *memStore) ListTables(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.tables))
	for k := range s.tables {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func (s *memStore) RenameTable(context.Context, string, string) error { return nil }
func (s *memStore) DropTable(_ context.Context, table string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tables, table)
	return nil
}

func (s *memStore) InsertRow(_ context.Context, table, key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tables[table]
	if !ok {
		return bot.ErrTableNotFound
	}
	if _, exists := t[key]; exists {
		return bot.ErrRowExists
	}
	t[key] = append([]byte(nil), value...)
	return nil
}

func (s *memStore) GetRow(_ context.Context, table, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tables[table]
	if !ok {
		return nil, bot.ErrTableNotFound
	}
	v, ok := t[key]
	if !ok {
		return nil, bot.ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

func (s *memStore) UpdateRow(_ context.Context, table, key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tables[table]
	if !ok {
		return bot.ErrTableNotFound
	}
	if _, ok := t[key]; !ok {
		return bot.ErrNotFound
	}
	if s.failRow {
		return errors.New("row failed")
	}
	t[key] = append([]byte(nil), value...)
	return nil
}

func (s *memStore) DeleteRow(_ context.Context, table, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tables[table]
	if !ok {
		return bot.ErrTableNotFound
	}
	delete(t, key)
	return nil
}

func (s *memStore) ListRows(_ context.Context, table string) ([]bot.TableRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	t, ok := s.tables[table]
	if !ok {
		return nil, bot.ErrTableNotFound
	}
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]bot.TableRow, 0, len(keys))
	for _, k := range keys {
		out = append(out, bot.TableRow{Key: k, Value: append([]byte(nil), t[k]...)})
	}
	return out, nil
}

func (s *memStore) row(table, key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tables[table]
	if !ok {
		return nil, false
	}
	v, ok := t[key]
	return append([]byte(nil), v...), ok
}

func (s *memStore) rows(table string) map[string][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]byte{}
	for k, v := range s.tables[table] {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

// fixedChat 是可编程的 Chat 桩。
type fixedChat struct {
	mu       sync.Mutex
	requests []ChatRequest
	replies  []string
	err      error
	fn       func(ChatRequest) (ChatResult, error)
}

func (c *fixedChat) Complete(_ context.Context, req ChatRequest) (ChatResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, req)
	if c.fn != nil {
		return c.fn(req)
	}
	if c.err != nil {
		return ChatResult{}, c.err
	}
	if len(c.replies) == 0 {
		return ChatResult{Content: "默认回复"}, nil
	}
	r := c.replies[0]
	c.replies = c.replies[1:]
	return ChatResult{Content: r}, nil
}

func (c *fixedChat) requestCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

func (c *fixedChat) lastRequest() (ChatRequest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) == 0 {
		return ChatRequest{}, false
	}
	return c.requests[len(c.requests)-1], true
}

// fixedEmbedder 是确定性的 Embedding 桩，默认维度 4。
type fixedEmbedder struct {
	mu      sync.Mutex
	dims    int
	inputs  [][]string
	err     error
	vectors map[string][]float32
}

func (e *fixedEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inputs = append(e.inputs, append([]string(nil), inputs...))
	if e.err != nil {
		return nil, e.err
	}
	out := make([][]float32, len(inputs))
	for i, s := range inputs {
		if e.vectors != nil {
			if v, ok := e.vectors[s]; ok {
				out[i] = append([]float32(nil), v...)
				continue
			}
		}
		dims := e.dims
		if dims <= 0 {
			dims = 4
		}
		v := make([]float32, dims)
		var sum byte
		for j := 0; j < len(s); j++ {
			sum += s[j]
		}
		for j := range v {
			v[j] = float32(int(sum)%7+1) + float32(j)
		}
		out[i] = v
	}
	return out, nil
}

func (e *fixedEmbedder) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.inputs)
}

// fakeBot 记录 Send 并返回递增消息 ID。
type fakeBot struct {
	mu      sync.Mutex
	sends   []sent
	nextID  int
	log     *slog.Logger
	store   bot.Storage
	sendErr error
}

type sent struct {
	target bot.Target
	msg    *bot.Message
}

func (b *fakeBot) Send(_ context.Context, target bot.Target, msg *bot.Message) (*bot.SendResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sendErr != nil {
		return nil, b.sendErr
	}
	b.sends = append(b.sends, sent{target: target, msg: msg})
	b.nextID++
	return &bot.SendResult{MessageID: "sent-" + itoa(b.nextID)}, nil
}

func (b *fakeBot) Reply(ctx context.Context, ev *bot.Event, msg *bot.Message) (*bot.SendResult, error) {
	return b.Send(ctx, sessionTargetFromEvent(ev), msg)
}

func (b *fakeBot) Logger() *slog.Logger { return b.log }
func (b *fakeBot) Storage() bot.Storage { return b.store }

func (b *fakeBot) sendsSnapshot() []sent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]sent(nil), b.sends...)
}

func (b *fakeBot) waitSends(n int, timeout time.Duration) []sent {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sn := b.sendsSnapshot()
		if len(sn) >= n {
			return sn
		}
		time.Sleep(5 * time.Millisecond)
	}
	return b.sendsSnapshot()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// syncBuffer 是并发安全的日志缓冲。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// fakeTimer 是手动触发的定时器。
type fakeTimer struct {
	mu      sync.Mutex
	delay   time.Duration
	fn      func()
	stopped bool
}

func (t *fakeTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}

func (t *fakeTimer) fire() {
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return
	}
	t.stopped = true
	fn := t.fn
	t.mu.Unlock()
	fn()
}

type fakeTimers struct {
	mu     sync.Mutex
	timers []*fakeTimer
}

func (f *fakeTimers) afterFunc(delay time.Duration, fn func()) timerHandle {
	t := &fakeTimer{delay: delay, fn: fn}
	f.mu.Lock()
	f.timers = append(f.timers, t)
	f.mu.Unlock()
	return t
}

func (f *fakeTimers) last() *fakeTimer {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.timers) == 0 {
		return nil
	}
	return f.timers[len(f.timers)-1]
}

func (f *fakeTimers) lastDelay() time.Duration {
	if t := f.last(); t != nil {
		return t.delay
	}
	return -1
}

func (f *fakeTimers) fireLast() {
	if t := f.last(); t != nil {
		t.fire()
	}
}

// noopRegistrar 记录注册规则但不产生路由。
type noopRegistrar struct {
	mu   sync.Mutex
	seen int
}

func (r *noopRegistrar) OnCommand(string, bot.Handler, ...bot.Option) {
	r.mu.Lock()
	r.seen++
	r.mu.Unlock()
}
func (r *noopRegistrar) OnRegex(string, bot.Handler, ...bot.Option) {
	r.mu.Lock()
	r.seen++
	r.mu.Unlock()
}
func (r *noopRegistrar) OnKeyword([]string, bot.Handler, ...bot.Option) {
	r.mu.Lock()
	r.seen++
	r.mu.Unlock()
}
func (r *noopRegistrar) OnEvent(bot.EventType, bot.Handler, ...bot.Option) {
	r.mu.Lock()
	r.seen++
	r.mu.Unlock()
}
func (r *noopRegistrar) OnAll(bot.Handler, ...bot.Option) {
	r.mu.Lock()
	r.seen++
	r.mu.Unlock()
}
func (r *noopRegistrar) Use(bot.Middleware) {}

var errTest = errors.New("test error")

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func testRaw(overrides map[string]any) map[string]any {
	raw := map[string]any{
		"group_policy":                "open",
		"private_policy":              "open",
		"self_ids":                    []string{"bot"},
		"super_admins":                []string{"mock:admin"},
		"llm_model":                   "test-chat",
		"embedding_model":             "test-embed",
		"embedding_dimensions":        4,
		"batch_window":                "30ms",
		"batch_max_window":            "100ms",
		"memory_consolidate_interval": "0",
		"history_cleanup_interval":    "0",
		"llm_max_retries":             0,
		"embedding_max_retries":       0,
		"knowledge_url_allow_private": true,
	}
	for k, v := range overrides {
		raw[k] = v
	}
	return raw
}

type testEnv struct {
	p      *Plugin
	store  *memStore
	bot    *fakeBot
	chat   *fixedChat
	emb    *fixedEmbedder
	reg    *noopRegistrar
	cfg    *config
	logs   *syncBuffer
	timers *fakeTimers
}

func newTestEnv(t *testing.T, overrides map[string]any) *testEnv {
	t.Helper()
	return newTestEnvWithStore(t, newMemStore(), overrides)
}

func newTestEnvWithStore(t *testing.T, store *memStore, overrides map[string]any) *testEnv {
	t.Helper()
	fb := &fakeBot{log: testLogger(), store: store}
	chat := &fixedChat{}
	emb := &fixedEmbedder{dims: 4}
	reg := &noopRegistrar{}
	logs := &syncBuffer{}
	timers := &fakeTimers{}
	p := &Plugin{chat: chat, embedder: emb, afterFunc: timers.afterFunc}
	raw := testRaw(overrides)
	pc := bot.PluginContext{
		Name:       pluginName,
		Config:     bot.NewConfig(raw),
		Logger:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Storage:    store,
		Tables:     store,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		Bot:        fb,
	}
	ctx := bot.WithPluginContext(context.Background(), pc)
	if err := p.Setup(ctx, reg); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := p.Stop(stctx); err != nil {
			t.Logf("Stop: %v", err)
		}
	})
	if p.cfg.MemoryEnabled || p.cfg.RAGEnabled {
		if !waitIndexReady(p, 3*time.Second) {
			t.Fatalf("index not ready, state=%s", p.idx.statusState())
		}
	}
	if !waitSchedulerStarted(p, 3*time.Second) {
		t.Fatal("scheduler not started")
	}
	return &testEnv{p: p, store: store, bot: fb, chat: chat, emb: emb, reg: reg, cfg: p.cfg, logs: logs, timers: timers}
}

func waitSchedulerStarted(p *Plugin, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		p.jobs.mu.Lock()
		started := p.jobs.started
		p.jobs.mu.Unlock()
		if started {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

func putChunkForTest(idx *vectorIndex, c *chunkEntry) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	cp := *c
	cp.Vector = append([]float32(nil), c.Vector...)
	idx.chunks[c.ID] = &cp
}

func waitIndexReady(p *Plugin, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if p.idx != nil && p.idx.statusState() == vectorStateReady {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return p.idx != nil && p.idx.statusState() == vectorStateReady
}

func groupEvent(id, user, text, channel string) *bot.Event {
	return &bot.Event{
		ID:       id,
		Type:     bot.EventMessage,
		Platform: "mock",
		BotID:    "bot",
		Time:     time.Now().UTC(),
		Sender:   &bot.User{ID: user, Name: user},
		Channel:  &bot.Channel{ID: channel, Name: channel},
		Message: &bot.Message{
			ID:   id,
			Kind: bot.MessageGroup,
			Segments: []bot.Segment{
				{Type: bot.SegText, Data: map[string]any{bot.KeyText: text}},
			},
		},
	}
}

func privateEvent(id, user, text string) *bot.Event {
	ev := groupEvent(id, user, text, "")
	ev.Channel = nil
	ev.Message.Kind = bot.MessagePrivate
	return ev
}

func atEvent(id, user, text, channel string, at ...string) *bot.Event {
	ev := groupEvent(id, user, text, channel)
	for _, uid := range at {
		ev.Message.Segments = append(ev.Message.Segments, bot.Segment{
			Type: bot.SegAt, Data: map[string]any{bot.KeyUserID: uid},
		})
	}
	return ev
}

func replyEvent(id, user, text, channel, replyID string) *bot.Event {
	ev := groupEvent(id, user, text, channel)
	ev.Message.Segments = append(ev.Message.Segments, bot.Segment{
		Type: bot.SegReply, Data: map[string]any{bot.KeyMessageID: replyID},
	})
	return ev
}

func imageEvent(id, user, text, channel string, urls ...string) *bot.Event {
	ev := groupEvent(id, user, text, channel)
	for _, u := range urls {
		ev.Message.Segments = append(ev.Message.Segments, bot.Segment{
			Type: bot.SegImage, Data: map[string]any{bot.KeyURL: u},
		})
	}
	return ev
}

func commandEvent(name, args string, user string, channel string) *bot.Event {
	ev := groupEvent("cmd-"+name+"-"+args, user, "/"+name+" "+args, channel)
	ev.Command = &bot.Command{Name: name, Args: strings.Fields(args), Raw: args}
	return ev
}

func runCommand(t *testing.T, p *Plugin, ev *bot.Event) string {
	t.Helper()
	r := bot.NewNoopReply()
	if err := p.handleCommand(context.Background(), ev, r); err != nil {
		t.Fatalf("handleCommand: %v", err)
	}
	return r.PlainText()
}

func messageText(msg *bot.Message) string {
	if msg == nil {
		return ""
	}
	var b bytes.Buffer
	for _, seg := range msg.Segments {
		if seg.Type == bot.SegText {
			b.WriteString(bot.SegmentData(seg, bot.KeyText))
		}
	}
	return b.String()
}
