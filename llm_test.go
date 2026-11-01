package chatagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func chatCfg(t *testing.T, base string, overrides map[string]any) *config {
	raw := map[string]any{
		"llm_base_url":          base,
		"llm_model":             "m",
		"llm_timeout":           "2s",
		"llm_max_retries":       0,
		"embedding_model":       "e",
		"embedding_base_url":    base,
		"embedding_timeout":     "2s",
		"embedding_max_retries": 0,
	}
	for k, v := range overrides {
		raw[k] = v
	}
	return mustConfig(t, raw)
}

func TestChatCompleteParsesResponse(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("unexpected auth %q", auth)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hello"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	cfg := chatCfg(t, srv.URL, map[string]any{"llm_api_key": ""})
	c := newChatCompleter(cfg, srv.Client(), testLogger())
	res, err := c.Complete(context.Background(), ChatRequest{
		Model:       "m",
		Messages:    []ChatMessage{{Role: "system", Content: "sys"}, {Role: "user", Content: "hi"}},
		Temperature: 0.5,
		MaxTokens:   10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "hello" || res.FinishReason != "stop" {
		t.Fatalf("res=%+v", res)
	}
	if gotBody["model"] != "m" || gotBody["stream"] != false {
		t.Fatalf("body=%v", gotBody)
	}
}

func TestChatCompleteValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		cat  errorCategory
	}{
		{"invalid_json", `{`, errInvalidJSON},
		{"empty_choices", `{"choices":[]}`, errInvalidShape},
		{"missing_choices", `{}`, errInvalidShape},
		{"content_object", `{"choices":[{"message":{"content":{}}}]}`, errInvalidShape},
		{"content_array", `{"choices":[{"message":{"content":[]}}]}`, errInvalidShape},
		{"content_number", `{"choices":[{"message":{"content":1}}]}`, errInvalidShape},
		{"content_null", `{"choices":[{"message":{"content":null}}]}`, errInvalidShape},
		{"content_empty", `{"choices":[{"message":{"content":""}}]}`, errInvalidShape},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := newChatCompleter(chatCfg(t, srv.URL, nil), srv.Client(), testLogger())
			_, err := c.Complete(context.Background(), ChatRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: "x"}}})
			if err == nil || categoryOf(err) != tc.cat {
				t.Fatalf("error=%v cat=%s want %s", err, categoryOf(err), tc.cat)
			}
		})
	}
}

func TestChatRetryAndStatusMapping(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	cfg := chatCfg(t, srv.URL, map[string]any{"llm_max_retries": 1})
	c := newChatCompleter(cfg, srv.Client(), testLogger())
	start := time.Now()
	res, err := c.Complete(context.Background(), ChatRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: "x"}}})
	if err != nil || res.Content != "ok" || calls != 2 {
		t.Fatalf("res=%+v err=%v calls=%d", res, err, calls)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("retry backoff too short: %v", elapsed)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad"))
	}))
	defer bad.Close()
	c = newChatCompleter(chatCfg(t, bad.URL, map[string]any{"llm_max_retries": 3}), bad.Client(), testLogger())
	_, err = c.Complete(context.Background(), ChatRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: "x"}}})
	if categoryOf(err) != errHTTP4xx {
		t.Fatalf("category=%s err=%v", categoryOf(err), err)
	}
}

func TestChatTimeoutAndCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(200 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"late"}}]}`))
	}))
	defer srv.Close()
	c := newChatCompleter(chatCfg(t, srv.URL, map[string]any{"llm_timeout": "20ms"}), srv.Client(), testLogger())
	_, err := c.Complete(context.Background(), ChatRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: "x"}}})
	if categoryOf(err) != errTimeout {
		t.Fatalf("timeout category=%s err=%v", categoryOf(err), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Complete(ctx, ChatRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: "x"}}})
	if categoryOf(err) != errCanceled {
		t.Fatalf("cancel category=%s err=%v", categoryOf(err), err)
	}
}

func TestChatVisionRequestUsesDataURL(t *testing.T) {
	var body chatWireRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	c := newChatCompleter(chatCfg(t, srv.URL, nil), srv.Client(), testLogger())
	parts := []ContentPart{
		{Type: "text", Text: "see [图片]"},
		{Type: "image_url", ImageURL: &ImageURL{URL: "data:image/png;base64,AAAA"}},
	}
	_, err := c.Complete(context.Background(), ChatRequest{
		Model: "m",
		Messages: []ChatMessage{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: parts},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(body)
	if !strings.Contains(string(raw), "data:image/png;base64,AAAA") {
		t.Fatalf("body missing data URL: %s", raw)
	}
	if strings.Contains(string(raw), "http://original") {
		t.Fatal("original image URL leaked")
	}
}

func TestEmbeddingValidationAndOrdering(t *testing.T) {
	var lastReq embedWireRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&lastReq)
		_, _ = w.Write([]byte(`{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`))
	}))
	defer srv.Close()
	e := newHTTPEmbedder(chatCfg(t, srv.URL, nil), srv.Client(), testLogger())
	vecs, err := e.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 || vecs[0][0] != 1 || vecs[1][1] != 1 {
		t.Fatalf("vecs=%v", vecs)
	}
	if lastReq.Dimensions != 0 {
		t.Fatalf("dimensions should be omitted: %+v", lastReq)
	}

	// dimensions > 0 发送并校验
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&lastReq)
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0,0]}]}`))
	}))
	defer srv2.Close()
	e = newHTTPEmbedder(chatCfg(t, srv2.URL, map[string]any{"embedding_dimensions": 3}), srv2.Client(), testLogger())
	if _, err := e.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if lastReq.Dimensions != 3 {
		t.Fatalf("dimensions=%d", lastReq.Dimensions)
	}
}

func TestEmbeddingValidationFailures(t *testing.T) {
	cases := []struct {
		name string
		body string
		cat  errorCategory
	}{
		{"count", `{"data":[{"index":0,"embedding":[1,0]}]}`, errInvalidShape},
		{"duplicate_index", `{"data":[{"index":0,"embedding":[1,0]},{"index":0,"embedding":[1,0]}]}`, errInvalidShape},
		{"missing_index", `{"data":[{"embedding":[1,0]},{"index":1,"embedding":[1,0]}]}`, errInvalidShape},
		{"dim_mismatch", `{"data":[{"index":0,"embedding":[1,0]},{"index":1,"embedding":[1,0,0]}]}`, errInvalidShape},
		{"zero", `{"data":[{"index":0,"embedding":[0,0]},{"index":1,"embedding":[1,0]}]}`, errInvalidVec},
		{"overflow", `{"data":[{"index":0,"embedding":[1e39,0]},{"index":1,"embedding":[1,0]}]}`, errInvalidVec},
		{"bad_json", `{`, errInvalidJSON},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			e := newHTTPEmbedder(chatCfg(t, srv.URL, nil), srv.Client(), testLogger())
			_, err := e.Embed(context.Background(), []string{"a", "b"})
			if err == nil || categoryOf(err) != tc.cat {
				t.Fatalf("err=%v cat=%s want %s", err, categoryOf(err), tc.cat)
			}
		})
	}
}

func TestEmbeddingEmptyAndTruncation(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req embedWireRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Input) != 1 || runeLen(req.Input[0]) != embeddingMaxInputChars {
			t.Errorf("input len=%d", runeLen(req.Input[0]))
		}
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
	}))
	defer srv.Close()
	e := newHTTPEmbedder(chatCfg(t, srv.URL, nil), srv.Client(), testLogger())
	if vecs, err := e.Embed(context.Background(), nil); err != nil || len(vecs) != 0 || calls != 0 {
		t.Fatalf("empty embed vecs=%v err=%v calls=%d", vecs, err, calls)
	}
	if _, err := e.Embed(context.Background(), []string{strings.Repeat("x", embeddingMaxInputChars+5)}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}

type countingEmbedder struct {
	sizes [][]int
}

func (c *countingEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	c.sizes = append(c.sizes, []int{len(inputs)})
	out := make([][]float32, len(inputs))
	for i := range inputs {
		out[i] = []float32{1, float32(i)}
	}
	return out, nil
}

func TestEmbedTextsBatches(t *testing.T) {
	ce := &countingEmbedder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := newEmbedQueue(ctx, ce, testLogger())
	go q.loop()
	p := &Plugin{embedWorker: q, ctx: ctx}
	texts := make([]string, 17)
	for i := range texts {
		texts[i] = "x"
	}
	vecs, err := p.embedQueuedBatches(context.Background(), texts)
	if err != nil || len(vecs) != 17 {
		t.Fatalf("vecs=%d err=%v", len(vecs), err)
	}
	if len(ce.sizes) != 2 || ce.sizes[0][0] != 16 || ce.sizes[1][0] != 1 {
		t.Fatalf("batches=%v", ce.sizes)
	}
}

func TestImageDebugSummary(t *testing.T) {
	got := imageDebugSummary("data:image/png;base64,AAAA")
	if !strings.HasPrefix(got, "[image image/png ") || strings.HasSuffix(got, "AAAA]") {
		t.Fatalf("summary=%q", got)
	}
	if got := imageDebugSummary("http://example.com/a.png"); got != "[image]" {
		t.Fatalf("summary=%q", got)
	}
}

func TestErrorCategories(t *testing.T) {
	if categoryOf(errors.New("x")) != "internal" {
		t.Fatal("unknown category")
	}
	if categoryOf(context.Canceled) != errCanceled {
		t.Fatal("canceled category")
	}
	if categoryOf(context.DeadlineExceeded) != errTimeout {
		t.Fatal("deadline category")
	}
	if !retryableCategory(errHTTP429) || !retryableCategory(errTimeout) || retryableCategory(errHTTP4xx) {
		t.Fatal("retryable classification")
	}
}
