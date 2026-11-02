package chatagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RandomLemon/kei/adapters/mock"
	"github.com/RandomLemon/kei/pkg/bot"
	kei "github.com/RandomLemon/kei/pkg/kei"
)

const e2eVectorTable = "plugin:chat-agent:vector"

var e2eAdapterCh = make(chan *mock.Adapter, 8)

func init() {
	bot.RegisterAdapter(bot.AdapterMetadata{
		Name:        "e2etest",
		Version:     "v0.0.1",
		Author:      "test",
		Description: "end-to-end test adapter backed by the mock adapter",
		Platforms:   []string{"e2e"},
	}, func(ac bot.AdapterContext) (bot.Adapter, error) {
		a := mock.New(mock.Options{Name: ac.BotID, Platform: "e2e", Logger: ac.Logger})
		e2eAdapterCh <- a
		return a, nil
	})
}

type e2eRecordedChat struct {
	System    string
	UserText  string
	UserParts []ContentPart
}

type e2eStub struct {
	mu         sync.Mutex
	chats      []e2eRecordedChat
	embInputs  []string
	reply      string
	extraction string
}

func newE2EStub(t *testing.T) (*e2eStub, *httptest.Server) {
	t.Helper()
	stub := &e2eStub{
		reply:      "stub reply",
		extraction: `{"memories":[]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat/completions":
			var body struct {
				Messages []struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var rec e2eRecordedChat
			for _, m := range body.Messages {
				switch m.Role {
				case "system":
					_ = json.Unmarshal(m.Content, &rec.System)
				case "user":
					if err := json.Unmarshal(m.Content, &rec.UserText); err != nil {
						_ = json.Unmarshal(m.Content, &rec.UserParts)
					}
				}
			}
			stub.mu.Lock()
			stub.chats = append(stub.chats, rec)
			reply := stub.reply
			if rec.System == memoryExtractPrompt {
				reply = stub.extraction
			}
			stub.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{"message": map[string]any{"content": reply}, "finish_reason": "stop"}},
			})
		case "/embeddings":
			var req embedWireRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			stub.mu.Lock()
			stub.embInputs = append(stub.embInputs, req.Input...)
			stub.mu.Unlock()
			data := make([]any, len(req.Input))
			for i := range req.Input {
				data[i] = map[string]any{"index": i, "embedding": []float64{1, 0, 0, 0}}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return stub, srv
}

func (s *e2eStub) setReply(reply, extraction string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reply = reply
	if extraction != "" {
		s.extraction = extraction
	}
}

func (s *e2eStub) chatsSnapshot() []e2eRecordedChat {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]e2eRecordedChat(nil), s.chats...)
}

type e2eHost struct {
	adapter  *mock.Adapter
	p        *Plugin
	cancel   context.CancelFunc
	done     chan error
	stopOnce sync.Once
	runErr   error
}

func e2eHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Transport: &http.Transport{DialContext: dialer.DialContext},
		Timeout:   5 * time.Second,
	}
}

func e2eConfig(stubURL string, overrides map[string]any) []byte {
	settings := map[string]any{
		"group_policy":                "whitelist",
		"group_list":                  []string{"e2e:g9"},
		"private_policy":              "open",
		"self_ids":                    []string{"bot"},
		"super_admins":                []string{"e2e:admin"},
		"llm_base_url":                stubURL,
		"llm_model":                   "stub-chat",
		"llm_timeout":                 "3s",
		"llm_max_retries":             0,
		"embedding_base_url":          stubURL,
		"embedding_model":             "stub-embed",
		"embedding_dimensions":        4,
		"embedding_timeout":           "3s",
		"embedding_max_retries":       0,
		"memory_enabled":              true,
		"rag_enabled":                 true,
		"vision_enabled":              true,
		"memory_extract_min_messages": 1,
		"memory_dedupe_threshold":     0.99,
		"memory_min_score":            0.1,
		"knowledge_min_score":         0.1,
		"knowledge_chunk_chars":       200,
		"knowledge_chunk_overlap":     10,
		"knowledge_url_allow_private": true,
		"batch_window":                "20ms",
		"batch_max_window":            "80ms",
		"mention_min_interval":        "0s",
		"mention_reply_probability":   1.0,
		"random_enabled":              false,
		"reply_quote":                 false,
		"reply_dedupe":                false,
		"memory_consolidate_interval": "0s",
		"history_cleanup_interval":    "0s",
		"jobs_run_on_start":           false,
	}
	for k, v := range overrides {
		settings[k] = v
	}
	var b strings.Builder
	b.WriteString("log:\n  level: error\n  format: text\nauth:\n  admin_users: [\"e2e:coreadmin\"]\n")
	b.WriteString("bots:\n  - name: e2e-main\n    adapter: e2etest\n")
	b.WriteString("plugins:\n  chat-agent:\n    enabled: true\n")
	for k, v := range settings {
		switch t := v.(type) {
		case string:
			fmt.Fprintf(&b, "    %s: %q\n", k, t)
		case bool:
			fmt.Fprintf(&b, "    %s: %t\n", k, t)
		case []string:
			quoted := make([]string, len(t))
			for i, s := range t {
				quoted[i] = fmt.Sprintf("%q", s)
			}
			fmt.Fprintf(&b, "    %s: [%s]\n", k, strings.Join(quoted, ", "))
		default:
			fmt.Fprintf(&b, "    %s: %v\n", k, t)
		}
	}
	return []byte(b.String())
}

func startE2E(t *testing.T, store *memStore, stubURL string, overrides map[string]any) *e2eHost {
	t.Helper()
	p := &Plugin{}
	ctx, cancel := context.WithCancel(context.Background())
	h := &e2eHost{p: p, cancel: cancel, done: make(chan error, 1)}
	go func() {
		h.done <- kei.Run(ctx, kei.Options{
			Config:     e2eConfig(stubURL, overrides),
			Plugins:    []bot.Plugin{p},
			Storage:    store,
			HTTPClient: e2eHTTPClient(),
			Logger:     slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		})
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case a := <-e2eAdapterCh:
		h.adapter = a
	case <-timer.C:
		t.Fatal("adapter was not assembled")
	}
	for !h.adapter.Started() {
		select {
		case <-timer.C:
			t.Fatal("adapter did not start")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if !waitIndexReady(p, 3*time.Second) {
		t.Fatalf("index not ready: state=%s", p.idx.statusState())
	}
	t.Cleanup(func() { h.stop(t) })
	return h
}

func (h *e2eHost) stop(t *testing.T) {
	t.Helper()
	h.stopOnce.Do(func() {
		h.cancel()
		select {
		case err := <-h.done:
			h.runErr = err
		case <-time.After(10 * time.Second):
			t.Error("host did not stop")
		}
	})
	if h.runErr != nil {
		t.Errorf("Run: %v", h.runErr)
	}
}

func e2eGroup(id, user, text, channel string, at ...string) *bot.Event {
	segs := []bot.Segment{{Type: bot.SegText, Data: map[string]any{bot.KeyText: text}}}
	for _, uid := range at {
		segs = append(segs, bot.Segment{Type: bot.SegAt, Data: map[string]any{bot.KeyUserID: uid}})
	}
	return &bot.Event{
		ID:      id,
		Type:    bot.EventMessage,
		Sender:  &bot.User{ID: user, Name: user},
		Channel: &bot.Channel{ID: channel, Name: channel, Kind: bot.MessageGroup},
		Message: &bot.Message{ID: id, Kind: bot.MessageGroup, Segments: segs},
	}
}

func e2ePrivate(id, user, text string) *bot.Event {
	return &bot.Event{
		ID:     id,
		Type:   bot.EventMessage,
		Sender: &bot.User{ID: user, Name: user},
		Message: &bot.Message{ID: id, Kind: bot.MessagePrivate, Segments: []bot.Segment{
			{Type: bot.SegText, Data: map[string]any{bot.KeyText: text}},
		}},
	}
}

func (h *e2eHost) inject(t *testing.T, ev *bot.Event) {
	t.Helper()
	if err := h.adapter.Inject(context.Background(), ev); err != nil {
		t.Fatalf("inject: %v", err)
	}
}

func (h *e2eHost) sentCount() int { return len(h.adapter.Sent()) }

func (h *e2eHost) waitSent(t *testing.T, n int) []*bot.SendRequest {
	t.Helper()
	if !h.adapter.WaitSent(n, 3*time.Second) {
		t.Fatalf("timed out waiting for %d sends (have %d)", n, h.sentCount())
	}
	return h.adapter.Sent()
}

func (h *e2eHost) command(t *testing.T, name, args, user string) (string, []*bot.SendRequest) {
	t.Helper()
	before := h.sentCount()
	text := "/" + name
	if args != "" {
		text += " " + args
	}
	ev := e2eGroup("cmd-"+itoa(before)+"-"+args, user, text, "g9")
	ev.Message.Segments = []bot.Segment{{Type: bot.SegText, Data: map[string]any{bot.KeyText: text}}}
	h.inject(t, ev)
	sent := h.waitSent(t, before+1)
	if len(sent) <= before {
		t.Fatalf("command produced no send")
	}
	return messageText(sent[before].Message), sent
}

func TestE2EGroupPolicyAndPrivateTarget(t *testing.T) {
	stub, srv := newE2EStub(t)
	defer srv.Close()
	h := startE2E(t, newMemStore(), srv.URL, nil)

	// g1 不在白名单内：无发送。
	h.inject(t, e2eGroup("g1-msg", "u1", "hello", "g1", "bot"))
	time.Sleep(100 * time.Millisecond)
	if h.sentCount() != 0 {
		t.Fatalf("g1 produced %d sends", h.sentCount())
	}
	// g9 被 @：回复目标为群聊。
	h.inject(t, e2eGroup("g9-msg", "u1", "hello", "g9", "bot"))
	sent := h.waitSent(t, 1)
	req := sent[0]
	if req.Target.Kind != bot.MessageGroup || req.Target.ChannelID != "g9" || req.Target.UserID != "" {
		t.Fatalf("group target=%+v", req.Target)
	}
	if messageText(req.Message) != "stub reply" {
		t.Fatalf("reply=%q", messageText(req.Message))
	}
	// 私聊恒寻址，目标 UserID 为发送者，ChannelID 为空。
	h.inject(t, e2ePrivate("p-msg", "peer", "hi"))
	sent = h.waitSent(t, 2)
	priv := sent[1]
	if priv.Target.Kind != bot.MessagePrivate || priv.Target.UserID != "peer" || priv.Target.ChannelID != "" {
		t.Fatalf("private target=%+v", priv.Target)
	}
	_ = stub
}

func TestE2ERAGInjection(t *testing.T) {
	stub, srv := newE2EStub(t)
	defer srv.Close()
	h := startE2E(t, newMemStore(), srv.URL, nil)

	if got, _ := h.command(t, "chat", "memory add 用户偏好简短回答", "admin"); got != "memory: 已接受" {
		t.Fatalf("memory add=%q", got)
	}
	sent := h.waitSent(t, 2)
	if !strings.HasPrefix(messageText(sent[1].Message), "memory: 完成 ") {
		t.Fatalf("memory completion=%q", messageText(sent[1].Message))
	}
	if got, _ := h.command(t, "chat", "knowledge add-text 持久化需要 sqlite 或 mysql 后端", "admin"); got != "knowledge: 已开始 text" {
		t.Fatalf("knowledge add=%q", got)
	}
	sent = h.waitSent(t, 4)
	if !strings.Contains(messageText(sent[3].Message), "knowledge: 完成 doc=") {
		t.Fatalf("knowledge completion=%q", messageText(sent[3].Message))
	}
	h.inject(t, e2eGroup("rag-msg", "u1", "部署方式", "g9", "bot"))
	sent = h.waitSent(t, 5)
	_ = sent
	chats := stub.chatsSnapshot()
	var last e2eRecordedChat
	found := false
	for _, c := range chats {
		if c.System != memoryExtractPrompt {
			last = c
			found = true
		}
	}
	if !found {
		t.Fatal("no chat request captured")
	}
	if !strings.Contains(last.System, "<long_term_memory>") || !strings.Contains(last.System, "用户偏好简短回答") {
		t.Fatalf("system missing memory block:\n%s", last.System)
	}
	if !strings.Contains(last.System, "<knowledge_base>") || !strings.Contains(last.System, "持久化需要 sqlite 或 mysql 后端") {
		t.Fatalf("system missing knowledge block:\n%s", last.System)
	}
}

func TestE2EConsolidatePersistenceRestart(t *testing.T) {
	stub, srv := newE2EStub(t)
	defer srv.Close()
	stub.setReply("ok", `{"memories":[{"text":"用户喜欢 Go","kind":"preference","importance":0.8}]}`)
	store := newMemStore()
	h := startE2E(t, store, srv.URL, nil)

	h.inject(t, e2eGroup("c1", "u1", "我住在杭州", "g9"))
	h.inject(t, e2eGroup("c2", "u1", "我喜欢 Go", "g9"))
	waitSessionSeq(t, h, sessionKey("e2e", "e2e-main", "group", "g9", ""), 2)
	if got, _ := h.command(t, "chat", "jobs run consolidate", "admin"); got != "jobs: 已触发 consolidate" {
		t.Fatalf("consolidate=%q", got)
	}
	sent := h.waitSent(t, 2)
	if !strings.Contains(messageText(sent[1].Message), "jobs: 完成 consolidate:") {
		t.Fatalf("completion=%q", messageText(sent[1].Message))
	}
	tables, err := store.ListTables(context.Background())
	if err != nil || !containsString(tables, e2eVectorTable) {
		t.Fatalf("tables=%v err=%v", tables, err)
	}
	rows := store.rows(e2eVectorTable)
	memories := 0
	for key := range rows {
		if strings.HasPrefix(key, "m:") {
			memories++
		}
	}
	if memories == 0 {
		t.Fatal("no memory row written")
	}
	if _, ok := rows[tableMetaKey]; !ok {
		t.Fatal("meta row missing")
	}
	h.stop(t)

	h2 := startE2E(t, store, srv.URL, nil)
	if got, _ := h2.command(t, "chat", "memory status", "admin"); !strings.Contains(got, "records="+itoa(memories)) {
		t.Fatalf("status after restart=%q want records=%d", got, memories)
	}
}

func TestE2EKnowledgeRowsAndList(t *testing.T) {
	_, srv := newE2EStub(t)
	defer srv.Close()
	store := newMemStore()
	h := startE2E(t, store, srv.URL, nil)
	body := strings.Repeat("知识库正文内容，用于分块测试。", 20)
	if got, _ := h.command(t, "chat", "knowledge add-text "+body, "admin"); got != "knowledge: 已开始 text" {
		t.Fatalf("add=%q", got)
	}
	sent := h.waitSent(t, 2)
	if !strings.Contains(messageText(sent[1].Message), "result=added") {
		t.Fatalf("completion=%q", messageText(sent[1].Message))
	}
	rows := store.rows(e2eVectorTable)
	var docID string
	var chunkIDs []string
	for key, raw := range rows {
		if strings.HasPrefix(key, "d:") {
			docID = strings.TrimPrefix(key, "d:")
			var rec docRecordJSON
			if err := json.Unmarshal(raw, &rec); err != nil {
				t.Fatal(err)
			}
			chunkIDs = rec.ChunkIDs
		}
	}
	if docID == "" || len(chunkIDs) == 0 {
		t.Fatalf("doc/chunks missing: doc=%s chunks=%v", docID, chunkIDs)
	}
	for _, cid := range chunkIDs {
		if _, ok := rows[chunkRowKey(cid)]; !ok {
			t.Fatalf("chunk %s missing", cid)
		}
	}
	if got, _ := h.command(t, "chat", "knowledge list", "admin"); !strings.Contains(got, "docs=1 chunks="+itoa(len(chunkIDs))) {
		t.Fatalf("list=%q", got)
	}
	if got, _ := h.command(t, "chat", "knowledge del "+docID, "admin"); got != "knowledge: 已开始 del" {
		t.Fatalf("del=%q", got)
	}
	sent = h.waitSent(t, 5)
	if !strings.Contains(messageText(sent[4].Message), "result=deleted") {
		t.Fatalf("del completion=%q", messageText(sent[4].Message))
	}
}

func TestE2ECleanupProtectsUnconsolidated(t *testing.T) {
	_, srv := newE2EStub(t)
	defer srv.Close()
	h := startE2E(t, newMemStore(), srv.URL, map[string]any{
		"history_ttl":               "80ms",
		"mention_reply_probability": 0.0,
	})
	h.inject(t, e2eGroup("k1", "u1", "第一组消息", "g9"))
	h.inject(t, e2ePrivate("k2", "peer", "私聊消息"))
	waitSessionSeq(t, h, sessionKey("e2e", "e2e-main", "group", "g9", ""), 1)
	waitSessionSeq(t, h, sessionKey("e2e", "e2e-main", "private", "", "peer"), 1)
	if got, _ := h.command(t, "chat", "jobs run consolidate", "admin"); got != "jobs: 已触发 consolidate" {
		t.Fatalf("consolidate=%q", got)
	}
	h.waitSent(t, 2)
	h.inject(t, e2eGroup("k3", "u1", "新消息保持未整理", "g9"))
	waitSessionSeq(t, h, sessionKey("e2e", "e2e-main", "group", "g9", ""), 2)
	time.Sleep(200 * time.Millisecond)
	if got, _ := h.command(t, "chat", "jobs run cleanup", "admin"); got != "jobs: 已触发 cleanup" {
		t.Fatalf("cleanup=%q", got)
	}
	h.waitSent(t, 4)
	h.p.sessionsMu.Lock()
	group := h.p.sessions[sessionKey("e2e", "e2e-main", "group", "g9", "")]
	_, privateExists := h.p.sessions[sessionKey("e2e", "e2e-main", "private", "", "peer")]
	h.p.sessionsMu.Unlock()
	if group == nil {
		t.Fatal("unconsolidated group session removed")
	}
	if privateExists {
		t.Fatal("consolidated private session not removed")
	}
}

func TestE2EAdminAuthorizationAndPersistence(t *testing.T) {
	_, srv := newE2EStub(t)
	defer srv.Close()
	store := newMemStore()
	h := startE2E(t, store, srv.URL, nil)
	if got, _ := h.command(t, "chat", "policy group open", "admin"); got != "policy: group=open" {
		t.Fatalf("policy=%q", got)
	}
	if got, _ := h.command(t, "chat", "status", "nobody"); got != notAdminLine {
		t.Fatalf("non-admin=%q", got)
	}
	if got, _ := h.command(t, "chat-admin", "status", "coreadmin"); !strings.HasPrefix(got, "chat-agent: enabled=") {
		t.Fatalf("core admin=%q", got)
	}
	waitUntil(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		raw, ok := store.kv["plugin:chat-agent:"+keyPolicy]
		return ok && strings.Contains(string(raw), `"group_mode":"open"`)
	})
	h.stop(t)

	h2 := startE2E(t, store, srv.URL, nil)
	if got, _ := h2.command(t, "chat", "policy", "admin"); got != "policy: group=open private=open" {
		t.Fatalf("policy after restart=%q", got)
	}
}

func TestE2EImageRecognitionAndMemory(t *testing.T) {
	stub, srv := newE2EStub(t)
	defer srv.Close()
	stub.setReply("看到图片了", `{"memories":[{"text":"图片里是部署拓扑图","kind":"fact","importance":0.7}]}`)
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("fake-png-body"))
	}))
	defer imgSrv.Close()
	store := newMemStore()
	h := startE2E(t, store, srv.URL, nil)
	// 端到端图片服务器在回环地址；生产受限 client 会拒绝它，测试显式注入。
	h.p.visionClient = imgSrv.Client()
	ev := e2eGroup("img-msg", "u1", "看看这个", "g9", "bot")
	ev.Message.Segments = append(ev.Message.Segments, bot.Segment{
		Type: bot.SegImage, Data: map[string]any{bot.KeyURL: imgSrv.URL + "/a.png"},
	})
	h.inject(t, ev)
	h.waitSent(t, 1)
	chats := stub.chatsSnapshot()
	foundImage := false
	for _, c := range chats {
		for _, p := range c.UserParts {
			if p.Type == "image_url" && strings.HasPrefix(p.ImageURL.URL, "data:image/png;base64,") {
				foundImage = true
			}
		}
	}
	if !foundImage {
		t.Fatal("chat request did not include data URL image part")
	}
	if got, _ := h.command(t, "chat", "jobs run consolidate", "admin"); got != "jobs: 已触发 consolidate" {
		t.Fatalf("consolidate=%q", got)
	}
	h.waitSent(t, 3)
	var description string
	for key, raw := range store.rows(e2eVectorTable) {
		if !strings.HasPrefix(key, "m:") {
			continue
		}
		var rec memoryRecordJSON
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Text == "图片里是部署拓扑图" {
			description = rec.Text
		}
	}
	if description == "" {
		t.Fatal("image memory not written")
	}
	h.stop(t)

	h2 := startE2E(t, store, srv.URL, nil)
	if got, _ := h2.command(t, "chat", "memory search 部署拓扑", "admin"); got != "memory search: 已开始" {
		t.Fatalf("memory search after restart=%q", got)
	}
	sent := h2.waitSent(t, 2)
	if len(sent) < 2 || !strings.Contains(messageText(sent[1].Message), "memory search: 1 条结果") {
		t.Fatalf("memory search result=%v", sent)
	}
}

func waitSessionSeq(t *testing.T, h *e2eHost, key string, seq uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.p.sessionsMu.Lock()
		s := h.p.sessions[key]
		h.p.sessionsMu.Unlock()
		if s != nil {
			s.mu.Lock()
			got := s.seq
			s.mu.Unlock()
			if got >= seq {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("session %s did not reach seq %d", key, seq)
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func waitUntil(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met")
}
