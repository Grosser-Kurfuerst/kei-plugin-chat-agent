package chatagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBlockedIPRanges(t *testing.T) {
	blocked := []string{
		"0.0.0.0", "10.1.2.3", "100.64.0.1", "127.0.0.1", "169.254.1.1",
		"172.16.0.1", "192.0.0.1", "192.0.2.1", "192.168.1.1", "198.18.0.1",
		"198.51.100.1", "203.0.113.1", "224.0.0.1", "240.0.0.1",
		"::", "::1", "fc00::1", "fe80::1", "ff00::1", "2001:db8::1",
		"2001::1", "2002::1", "64:ff9b::1", "::ffff:127.0.0.1",
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %s", s)
		}
		if !isBlockedIP(ip) {
			t.Fatalf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700::1"} {
		if isBlockedIP(net.ParseIP(s)) {
			t.Fatalf("%s should be allowed", s)
		}
	}
}

func TestValidateKnowledgeURL(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		url    string
		allow  bool
		reason string
	}{
		{"ftp://example.com/x", false, "url_rejected"},
		{"http://", false, "url_rejected"},
		{"http://u:p@example.com/x", false, "url_rejected"},
		{"http://127.0.0.1/x", false, "url_rejected"},
		{"http://10.0.0.1/x", false, "url_rejected"},
		{"http://[::1]/x", false, "url_rejected"},
		{"http://127.0.0.1/x", true, ""},
		{"https://8.8.8.8/x", false, ""},
	}
	for _, tc := range cases {
		_, err := validateKnowledgeURL(ctx, tc.url, tc.allow)
		if tc.reason == "" {
			if err != nil {
				t.Fatalf("%s: %v", tc.url, err)
			}
			continue
		}
		if err == nil || knowledgeReason(err) != tc.reason {
			t.Fatalf("%s: err=%v reason=%s", tc.url, err, knowledgeReason(err))
		}
	}
}

func TestKnowledgeFetcher(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("hello"))
		case "/html":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><title>T</title><body>body</body></html>`))
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/badtype":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		case "/emptytype":
			w.Header()["Content-Type"] = []string{""}
			_, _ = w.Write([]byte("x"))
		case "/large":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("0123456789abcdef"))
		case "/status":
			w.WriteHeader(http.StatusNotFound)
		case "/r1":
			http.Redirect(w, r, "/r2", http.StatusFound)
		case "/r2":
			http.Redirect(w, r, "/r3", http.StatusFound)
		case "/r3":
			http.Redirect(w, r, "/r4", http.StatusFound)
		case "/r4":
			http.Redirect(w, r, "/ok", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	cfg := mustConfig(t, map[string]any{
		"knowledge_max_bytes":         10,
		"knowledge_url_allow_private": true,
		"llm_model":                   "m",
		"embedding_model":             "e",
	})
	f := newKnowledgeFetcher(cfg, srv.Client())
	if page, err := f.Fetch(context.Background(), srv.URL+"/ok"); err != nil || string(page.Body) != "hello" || page.ContentType != "text/plain" {
		t.Fatalf("ok page=%+v err=%v", page, err)
	}
	if page, err := f.Fetch(context.Background(), srv.URL+"/redirect"); err != nil || string(page.Body) != "hello" {
		t.Fatalf("redirect page=%+v err=%v", page, err)
	}
	if page, err := f.Fetch(context.Background(), srv.URL+"/r3"); err != nil || string(page.Body) != "hello" {
		t.Fatalf("3 redirects page=%+v err=%v", page, err)
	}
	cases := []struct {
		path   string
		reason string
	}{
		{"/loop", "url_rejected"},
		{"/r1", "url_rejected"},
		{"/badtype", "unsupported_type"},
		{"/emptytype", "unsupported_type"},
		{"/large", "too_large"},
		{"/status", "fetch_error"},
	}
	for _, tc := range cases {
		if _, err := f.Fetch(context.Background(), srv.URL+tc.path); err == nil || knowledgeReason(err) != tc.reason {
			t.Fatalf("%s: err=%v reason=%s", tc.path, err, knowledgeReason(err))
		}
	}
	if _, err := f.Fetch(context.Background(), "file:///tmp/x"); err == nil || knowledgeReason(err) != "url_rejected" {
		t.Fatalf("file url err=%v", err)
	}
}

func TestHTMLExtractionAndTitle(t *testing.T) {
	raw := `<html><head><title> 部署 &amp; 说明 </title><style>.x{}</style></head>
<body><!-- c --><script>bad()</script><noscript>no</noscript>
<p>第一段 &amp; 实体</p><ul><li>项目一</li><li>项目二</li></ul></body></html>`
	got := extractHTMLText(raw)
	if strings.Contains(got, "bad()") || strings.Contains(got, "no") || strings.Contains(got, ".x{}") {
		t.Fatalf("script/style/noscript remained: %q", got)
	}
	if !strings.Contains(got, "第一段 & 实体") || !strings.Contains(got, "- 项目一") {
		t.Fatalf("extract=%q", got)
	}
	if title := htmlTitle(raw); title != "部署 & 说明" {
		t.Fatalf("title=%q", title)
	}
	if title := deriveKnowledgeTitle("命令标题", "页面标题", "https://example.com/a/b", "正文"); title != "命令标题" {
		t.Fatalf("title=%q", title)
	}
	if title := deriveKnowledgeTitle("", "页面标题", "https://example.com/a/b", "正文"); title != "页面标题" {
		t.Fatalf("title=%q", title)
	}
	if title := deriveKnowledgeTitle("", "", "https://example.com/a/b/", "正文"); title != "b" {
		t.Fatalf("title=%q", title)
	}
	if title := deriveKnowledgeTitle("", "", "https://example.com/", "正文"); title != "example.com" {
		t.Fatalf("title=%q", title)
	}
	if title := deriveKnowledgeTitle("", "", "text:正文内容", "正文内容"); title != "正文内容" {
		t.Fatalf("text title=%q", title)
	}
	long := strings.Repeat("x", 300)
	if title := deriveKnowledgeTitle(long, "", "https://example.com/", "正文"); runeLen(title) != 200 {
		t.Fatalf("long title len=%d", runeLen(title))
	}
}

func TestChunkText(t *testing.T) {
	text := strings.Repeat("a", 120) + "。" + strings.Repeat("b", 120) + "\n\n" + strings.Repeat("c", 120)
	chunks := chunkText(text, 100, 10)
	if len(chunks) < 3 {
		t.Fatalf("chunks=%v", chunks)
	}
	for _, c := range chunks {
		if runeLen(c) > 100 {
			t.Fatalf("chunk too long: %d", runeLen(c))
		}
		if runeLen(c) < knowledgeMinChunkRunes {
			t.Fatalf("chunk too short: %q", c)
		}
	}
	dedup := chunkText(strings.Repeat("word ", 100), 100, 20)
	seen := map[string]bool{}
	for _, c := range dedup {
		if seen[c] {
			t.Fatalf("duplicate chunk %q", c)
		}
		seen[c] = true
	}
	progress := chunkText(strings.Repeat("x", 100), 30, 30)
	if len(progress) == 0 {
		t.Fatal("chunking made no progress")
	}
	if id := docIDFor("https://x/y"); len(id) != 32 {
		t.Fatalf("doc id=%q", id)
	}
	if id1, id2 := chunkIDFor("d", 0, "text"), chunkIDFor("d", 0, "text"); id1 != id2 || len(id1) != 32 {
		t.Fatalf("chunk ids %q %q", id1, id2)
	}
}

func TestKnowledgeImportUpdateDelete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		switch r.URL.Path {
		case "/doc":
			_, _ = w.Write([]byte(strings.Repeat("部署说明正文。", 40)))
		case "/doc2":
			_, _ = w.Write([]byte(strings.Repeat("更新后的正文。", 40)))
		default:
			_, _ = w.Write([]byte("hello world, this is a long enough body"))
		}
	}))
	defer srv.Close()
	env := newTestEnv(t, map[string]any{
		"random_enabled":          false,
		"knowledge_chunk_chars":   100,
		"knowledge_chunk_overlap": 10,
		"knowledge_max_bytes":     100000,
	})
	ctx := context.Background()
	res, err := env.p.addKnowledgeURL(ctx, srv.URL+"/doc", "部署说明")
	if err != nil || res.Result != "added" || res.Chunks < 2 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	doc, ok := env.p.idx.docByID(res.DocID)
	if !ok || doc.Title != "部署说明" || len(doc.ChunkIDs) != res.Chunks {
		t.Fatalf("doc=%+v", doc)
	}
	rows := env.store.rows(tableName)
	if _, ok := rows[docRowKey(res.DocID)]; !ok {
		t.Fatal("doc row missing")
	}
	for _, cid := range doc.ChunkIDs {
		if _, ok := rows[chunkRowKey(cid)]; !ok {
			t.Fatalf("chunk row missing %s", cid)
		}
	}
	embCalls := env.emb.callCount()
	res2, err := env.p.addKnowledgeURL(ctx, srv.URL+"/doc", "部署说明")
	if err != nil || res2.Result != "unchanged" || env.emb.callCount() != embCalls {
		t.Fatalf("unchanged res=%+v err=%v emb=%d", res2, err, env.emb.callCount())
	}
	res3, err := env.p.addKnowledgeURL(ctx, srv.URL+"/doc", "新标题")
	if err != nil || res3.Result != "unchanged" {
		t.Fatalf("title res=%+v err=%v", res3, err)
	}
	if doc, _ := env.p.idx.docByID(res.DocID); doc.Title != "新标题" {
		t.Fatalf("title not updated: %+v", doc)
	}
	oldChunks := append([]string(nil), doc.ChunkIDs...)
	res4, err := env.p.addKnowledgeURL(ctx, srv.URL+"/doc2", "部署说明")
	_ = res4
	_ = err
	// 同一 source 才会更新；/doc2 是不同文档。
	if res4.Result != "added" {
		t.Fatalf("different source=%+v", res4)
	}
	// 通过相同 source 重新 import 更新后的正文。
	page, err := env.p.fetcher.Fetch(ctx, srv.URL+"/doc2")
	if err != nil {
		t.Fatal(err)
	}
	res5, err := env.p.importKnowledgeBody(ctx, srv.URL+"/doc", "部署说明", page.Body, page.ContentType)
	if err != nil || res5.Result != "updated" || res5.DocID != res.DocID {
		t.Fatalf("updated res=%+v err=%v", res5, err)
	}
	for _, cid := range oldChunks {
		if _, ok := env.store.row(tableName, chunkRowKey(cid)); ok {
			// 重叠块可能保留；只要求不在新集合中的旧块被清理。
			found := false
			for _, newID := range res5ChunkIDs(env, res.DocID) {
				if newID == cid {
					found = true
				}
			}
			if !found {
				t.Fatalf("old chunk %s not deleted", cid)
			}
		}
	}
	res6, err := env.p.deleteKnowledgeDoc(ctx, res.DocID)
	if err != nil || res6.Result != "deleted" {
		t.Fatalf("delete res=%+v err=%v", res6, err)
	}
	if _, ok := env.p.idx.docByID(res.DocID); ok {
		t.Fatal("doc still in index")
	}
	if _, ok := env.store.row(tableName, docRowKey(res.DocID)); ok {
		t.Fatal("doc row not deleted")
	}
}

func res5ChunkIDs(env *testEnv, id string) []string {
	if d, ok := env.p.idx.docByID(id); ok {
		return d.ChunkIDs
	}
	return nil
}

func TestKnowledgeEmptyAndLimits(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled":          false,
		"knowledge_chunk_chars":   100,
		"knowledge_chunk_overlap": 0,
		"knowledge_max_bytes":     1000000,
	})
	ctx := context.Background()
	if _, err := env.p.addKnowledgeText(ctx, "   "); err == nil || knowledgeReason(err) != "invalid_text" {
		t.Fatalf("empty err=%v", err)
	}
	tooLarge := strings.Repeat("x", env.cfg.KnowledgeMaxBytes+1)
	if _, err := env.p.addKnowledgeText(ctx, tooLarge); err == nil || knowledgeReason(err) != "too_large" {
		t.Fatalf("large err=%v", err)
	}
	var body strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&body, "段落%04d-", i)
		body.WriteString(strings.Repeat("段", 40))
		body.WriteString("\n\n")
	}
	if _, err := env.p.addKnowledgeText(ctx, body.String()); err == nil || knowledgeReason(err) != "chunk_limit" {
		t.Fatalf("chunk limit err=%v", err)
	}
	if _, err := env.p.addKnowledgeText(ctx, "hello world 这是一个足够长的正文。"); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeCapacity(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled":          false,
		"vector_max_records":      1,
		"knowledge_chunk_chars":   100,
		"knowledge_chunk_overlap": 0,
		"knowledge_max_bytes":     100000,
	})
	body := fmt.Sprintf("%0100d%0100d%0100d", 1, 2, 3)
	if _, err := env.p.addKnowledgeText(context.Background(), body); err == nil || knowledgeReason(err) != "vector_full" {
		t.Fatalf("err=%v", err)
	}
	for key := range env.store.rows(tableName) {
		if strings.HasPrefix(key, "d:") || strings.HasPrefix(key, "c:") {
			t.Fatalf("document rows written on vector_full: %s", key)
		}
	}
}

func TestKnowledgeCommands(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	if got := runCommand(t, env.p, commandEvent("chat", "knowledge list", "admin", "g")); got != "knowledge: (empty)" {
		t.Fatalf("empty list=%q", got)
	}
	body := strings.Repeat("知识库正文。", 20)
	out := runCommand(t, env.p, commandEvent("chat", "knowledge add-text "+body, "admin", "g"))
	if out != "knowledge: 已开始 text" {
		t.Fatalf("add out=%q", out)
	}
	sn := env.bot.waitSends(1, time.Second)
	if len(sn) != 1 || !strings.HasPrefix(messageText(sn[0].msg), "knowledge: 完成 doc=") {
		t.Fatalf("completion=%v", sn)
	}
	list := runCommand(t, env.p, commandEvent("chat", "knowledge list", "admin", "g"))
	if !strings.Contains(list, "knowledge: docs=1 chunks=") {
		t.Fatalf("list=%q", list)
	}
	var docID string
	for key := range env.store.rows(tableName) {
		if strings.HasPrefix(key, "d:") {
			docID = strings.TrimPrefix(key, "d:")
		}
	}
	if docID == "" {
		t.Fatal("doc id missing")
	}
	out = runCommand(t, env.p, commandEvent("chat", "knowledge refresh "+docID, "admin", "g"))
	if out != "knowledge: 已开始 refresh" {
		t.Fatalf("refresh out=%q", out)
	}
	sn = env.bot.waitSends(2, time.Second)
	if len(sn) < 2 || !strings.Contains(messageText(sn[1].msg), "result=unchanged") {
		t.Fatalf("refresh completion=%v", sn)
	}
	out = runCommand(t, env.p, commandEvent("chat", "knowledge del "+docID, "admin", "g"))
	if out != "knowledge: 已开始 del" {
		t.Fatalf("del out=%q", out)
	}
	sn = env.bot.waitSends(3, time.Second)
	if len(sn) < 3 || !strings.Contains(messageText(sn[2].msg), "result=deleted") {
		t.Fatalf("del completion=%v", sn)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "knowledge del nope", "admin", "g")); got != "chat-agent: 未找到: nope" {
		t.Fatalf("not found=%q", got)
	}
}

func TestKnowledgeCommandFailures(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	out := runCommand(t, env.p, commandEvent("chat", "knowledge add /tmp/x", "admin", "g"))
	if out != "knowledge: 已开始 /tmp/x" {
		t.Fatalf("out=%q", out)
	}
	sn := env.bot.waitSends(1, time.Second)
	if len(sn) != 1 || messageText(sn[0].msg) != "knowledge: 失败 url_rejected" {
		t.Fatalf("completion=%v", sn)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "knowledge add-text   ", "admin", "g")); got != "knowledge: 失败 invalid_text" {
		t.Fatalf("invalid text=%q", got)
	}
	env.p.jobs.mu.Lock()
	env.p.jobs.knowledgeRunning = true
	env.p.jobs.mu.Unlock()
	if got := runCommand(t, env.p, commandEvent("chat", "knowledge add-text hello world", "admin", "g")); got != "knowledge: 失败 busy" {
		t.Fatalf("busy=%q", got)
	}
	env.p.jobs.mu.Lock()
	env.p.jobs.knowledgeRunning = false
	env.p.jobs.mu.Unlock()

	off := newTestEnv(t, map[string]any{
		"random_enabled": false,
		"memory_enabled": true,
		"rag_enabled":    false,
	})
	if got := runCommand(t, off.p, commandEvent("chat", "knowledge list", "admin", "g")); got != "chat-agent: 知识库功能未启用" {
		t.Fatalf("disabled=%q", got)
	}
	env.p.idx.mu.Lock()
	env.p.idx.state = vectorStateLoading
	env.p.idx.mu.Unlock()
	if got := runCommand(t, env.p, commandEvent("chat", "knowledge list", "admin", "g")); got != notReadyLine {
		t.Fatalf("loading=%q", got)
	}
}

func TestKnowledgeRefreshAllAndRows(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	ctx := context.Background()
	if _, err := env.p.addKnowledgeText(ctx, "内联文本正文足够长，这是一段可以被分块的正文内容。"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Repeat("URL 文档正文足够长 enough。", 5)))
	}))
	defer srv.Close()
	if _, err := env.p.addKnowledgeURL(ctx, srv.URL+"/doc", "URL 标题"); err != nil {
		t.Fatal(err)
	}
	out := runCommand(t, env.p, commandEvent("chat", "knowledge refresh", "admin", "g"))
	if out != "knowledge: 已开始 refresh" {
		t.Fatalf("out=%q", out)
	}
	sn := env.bot.waitSends(1, time.Second)
	if len(sn) != 1 || !strings.Contains(messageText(sn[0].msg), "result=unchanged") {
		t.Fatalf("refresh all completion=%v", sn)
	}
	list := runCommand(t, env.p, commandEvent("chat", "knowledge list", "admin", "g"))
	rows := strings.Split(list, "\n")
	if len(rows) != 3 {
		t.Fatalf("list=%q", list)
	}
	for _, line := range rows[1:] {
		if !strings.Contains(line, "chunks=") || !strings.Contains(line, "updated=") {
			t.Fatalf("bad doc line %q", line)
		}
	}
	// 知识库行 JSON 形状
	var docs int
	for key, raw := range env.store.rows(tableName) {
		switch {
		case strings.HasPrefix(key, "d:"):
			docs++
			var rec docRecordJSON
			if err := json.Unmarshal(raw, &rec); err != nil {
				t.Fatal(err)
			}
			if len(rec.ChunkIDs) == 0 {
				t.Fatalf("doc without chunks: %+v", rec)
			}
		case strings.HasPrefix(key, "c:"):
			var rec chunkRecordJSON
			if err := json.Unmarshal(raw, &rec); err != nil {
				t.Fatal(err)
			}
			if rec.DocID == "" || rec.Vector == "" {
				t.Fatalf("chunk=%+v", rec)
			}
		}
	}
	if docs != 2 {
		t.Fatalf("docs=%d", docs)
	}
}
