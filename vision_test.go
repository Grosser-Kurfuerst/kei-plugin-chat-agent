package chatagent

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestVisionURLValidation(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false, "vision_enabled": true})
	cases := []struct {
		url    string
		reason string
	}{
		{"ftp://example.com/a.png", "bad_url"},
		{"http://", "bad_url"},
		{"http://u:p@example.com/a.png", "bad_url"},
		{"not a url", "bad_url"},
	}
	for _, tc := range cases {
		if _, reason := env.p.fetchImage(context.Background(), tc.url); reason != tc.reason {
			t.Fatalf("%s -> %s want %s", tc.url, reason, tc.reason)
		}
	}
}

func TestVisionPrivateAddressRejected(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()
	env := newTestEnv(t, map[string]any{"random_enabled": false, "vision_enabled": true})
	env.p.visionClient = newVisionClient(&http.Client{})
	if _, reason := env.p.fetchImage(context.Background(), srv.URL+"/a.png"); reason != "fetch_error" {
		t.Fatalf("reason=%s", reason)
	}
	if hits.Load() != 0 {
		t.Fatal("private address request reached server")
	}
}

func TestVisionRedirectsStatusAndLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/img":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("body"))
		case "/r1":
			http.Redirect(w, r, "/img", http.StatusFound)
		case "/r2":
			http.Redirect(w, r, "/r1", http.StatusFound)
		case "/r3":
			http.Redirect(w, r, "/r2", http.StatusFound)
		case "/r4":
			http.Redirect(w, r, "/r3", http.StatusFound)
		case "/r5":
			http.Redirect(w, r, "/r4", http.StatusFound)
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/huge":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(make([]byte, visionMaxImageBytes+1))
		case "/svg":
			w.Header().Set("Content-Type", "image/svg+xml")
			_, _ = w.Write([]byte(`<svg/>`))
		case "/text":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("hello"))
		case "/sniff":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("\x89PNG\r\n\x1a\npayload"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	env := newTestEnv(t, map[string]any{"random_enabled": false, "vision_enabled": true})
	env.p.visionClient = srv.Client()
	for _, tc := range []struct {
		path   string
		reason string
	}{
		{"/r3", ""},
		{"/r4", "redirect"},
		{"/missing", "status"},
		{"/huge", "too_large"},
		{"/svg", "format"},
		{"/text", "not_image"},
	} {
		_, reason := env.p.fetchImage(context.Background(), srv.URL+tc.path)
		if reason != tc.reason {
			t.Fatalf("%s -> %q want %q", tc.path, reason, tc.reason)
		}
	}
	att, reason := env.p.fetchImage(context.Background(), srv.URL+"/sniff")
	if reason != "" || att == nil || att.MIME != "image/png" {
		t.Fatalf("sniff att=%+v reason=%s", att, reason)
	}
	att, reason = env.p.fetchImage(context.Background(), srv.URL+"/img")
	if reason != "" || att == nil {
		t.Fatalf("img reason=%s", reason)
	}
	if !strings.HasPrefix(att.DataURL, "data:image/png;base64,") {
		t.Fatalf("data url=%q", att.DataURL)
	}
	raw := strings.TrimPrefix(att.DataURL, "data:image/png;base64,")
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || string(decoded) != "body" {
		t.Fatalf("decoded=%q err=%v", decoded, err)
	}
}

func TestVisionFailureLoggingHidesURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	env := newTestEnv(t, map[string]any{"random_enabled": false, "vision_enabled": true})
	env.p.visionClient = srv.Client()
	_, reason := env.p.fetchImage(context.Background(), srv.URL+"/secret/path.png")
	if reason != "status" {
		t.Fatalf("reason=%s", reason)
	}
	env.p.logVisionFailure(reason, srv.URL+"/secret/path.png", 0)
	waitLog(t, env.logs, "operation=vision")
	logs := env.logs.String()
	if strings.Contains(logs, "/secret/path.png") {
		t.Fatal("full image URL logged")
	}
	if !strings.Contains(logs, "host=") {
		t.Fatal("host field missing")
	}
}

func TestFetchImagesForChatLimitAndOrder(t *testing.T) {
	var counter atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "fail") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		counter.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()
	env := newTestEnv(t, map[string]any{"random_enabled": false, "vision_enabled": true})
	env.p.visionClient = srv.Client()
	var turns []Turn
	for i := 0; i < 6; i++ {
		path := "/ok" + itoa(i)
		if i >= 4 {
			path = "/fail" + itoa(i)
		}
		turns = append(turns, Turn{Seq: uint64(i), Images: []string{srv.URL + path}})
	}
	att := env.p.fetchImagesForChat(context.Background(), turns)
	total := 0
	for _, list := range att {
		for _, a := range list {
			if a != nil {
				total++
			}
		}
	}
	if total != visionMaxImagesPerRequest {
		t.Fatalf("total=%d", total)
	}
	// 最新两张失败，仍应继续尝试更旧图片凑满 4 张。
	if att[3] == nil || att[3][0] == nil || att[2] == nil || att[2][0] == nil {
		t.Fatal("failed newest images consumed quota")
	}
}

func TestFetchImagesForExtractionOrderAndLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()
	env := newTestEnv(t, map[string]any{"random_enabled": false, "vision_enabled": true})
	env.p.visionClient = srv.Client()
	turns := []Turn{
		{Seq: 0, Images: []string{srv.URL + "/0"}},
		{Seq: 1, Images: []string{srv.URL + "/1"}},
		{Seq: 2, Images: []string{srv.URL + "/2"}},
		{Seq: 3, Images: []string{srv.URL + "/3"}},
		{Seq: 4, Images: []string{srv.URL + "/4"}},
	}
	att := env.p.fetchImagesForExtraction(context.Background(), turns, 4)
	total := 0
	for _, list := range att {
		for _, a := range list {
			if a != nil {
				total++
			}
		}
	}
	if total != 4 {
		t.Fatalf("total=%d", total)
	}
	if att[4] != nil && att[4][0] != nil {
		t.Fatal("extraction should stop at 4 images in message order")
	}
}

func TestVisionDisabledNoFetch(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false, "vision_enabled": false})
	turns := []Turn{{Seq: 0, Images: []string{"http://127.0.0.1:1/a"}}}
	if att := env.p.fetchImagesForChat(context.Background(), turns); len(att) != 0 {
		t.Fatalf("attachments=%v", att)
	}
	if att := env.p.fetchImagesForExtraction(context.Background(), turns, 4); len(att) != 0 {
		t.Fatalf("attachments=%v", att)
	}
	_ = time.Second
}
