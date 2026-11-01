package chatagent

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

type blockingChat struct {
	started chan struct{}
	once    sync.Once
}

func (b *blockingChat) Complete(ctx context.Context, _ ChatRequest) (ChatResult, error) {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	return ChatResult{}, ctx.Err()
}

type blockingEmbedder struct {
	started chan struct{}
	once    sync.Once
}

func (b *blockingEmbedder) Embed(ctx context.Context, _ []string) ([][]float32, error) {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

type blockingFetcher struct {
	started chan struct{}
	once    sync.Once
}

func (b *blockingFetcher) Fetch(ctx context.Context, _ string) (*fetchedPage, error) {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

type blockingTransport struct {
	started chan struct{}
	once    sync.Once
}

func (b *blockingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	b.once.Do(func() { close(b.started) })
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func TestStopDuringGeneration(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	bc := &blockingChat{started: make(chan struct{})}
	env.p.chat = bc
	_ = env.p.handleGroupMessage(context.Background(), atEvent("e", "u", "hello", "g", "bot"), bot.NewNoopReply())
	env.timers.fireLast()
	select {
	case <-bc.started:
	case <-time.After(time.Second):
		t.Fatal("generation did not start")
	}
	start := time.Now()
	if err := env.p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Stop took %v", elapsed)
	}
	if len(env.bot.sendsSnapshot()) != 0 {
		t.Fatal("message sent during stop")
	}
	if err := env.p.sendText(context.Background(), bot.Target{}, "after"); err == nil {
		t.Fatal("send after stop must fail")
	}
	if err := env.p.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestStopDrainsWriteQueue(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	for i := 0; i < 50; i++ {
		env.p.kvSet(fmt.Sprintf("drain:%d", i), []byte("v"))
	}
	if err := env.p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	env.store.mu.Lock()
	defer env.store.mu.Unlock()
	for i := 0; i < 50; i++ {
		if _, ok := env.store.kv[fmt.Sprintf("drain:%d", i)]; !ok {
			t.Fatalf("write %d lost", i)
		}
	}
}

func TestStopDuringEmbedding(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	be := &blockingEmbedder{started: make(chan struct{})}
	env.p.embedder = be
	env.p.embedWorker.embedder = be
	done := make(chan error, 1)
	go func() {
		_, err := env.p.embedQueued(env.p.ctx, []string{"x"})
		done <- err
	}()
	select {
	case <-be.started:
	case <-time.After(time.Second):
		t.Fatal("embedding did not start")
	}
	if err := env.p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("embedding should fail after stop")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("embedding did not return after stop")
	}
}

func TestStopDuringKnowledgeFetch(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	bf := &blockingFetcher{started: make(chan struct{})}
	env.p.fetcher = bf
	out := runCommand(t, env.p, commandEvent("chat", "knowledge add https://example.com/x", "admin", "g"))
	if out != "knowledge: 已开始 https://example.com/x" {
		t.Fatalf("out=%q", out)
	}
	select {
	case <-bf.started:
	case <-time.After(time.Second):
		t.Fatal("fetch did not start")
	}
	if err := env.p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	for key := range env.store.rows(tableName) {
		if strings.HasPrefix(key, "d:") || strings.HasPrefix(key, "c:") {
			t.Fatalf("half-written knowledge row %s", key)
		}
	}
}

func TestStopDuringImageDownload(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled":              false,
		"vision_enabled":              true,
		"memory_extract_min_messages": 1,
	})
	bt := &blockingTransport{started: make(chan struct{})}
	env.p.visionClient = &http.Client{Transport: bt}
	s := env.p.getOrCreateSession(groupEvent("seed", "u", "seed", "g"))
	env.p.appendUserTurn(s, "[图片]", []string{"http://example.com/a.png"}, "u", "u", "e1", env.p.now())
	if !env.p.jobs.trigger(env.p.ctx, "consolidate", nil) {
		t.Fatal("trigger failed")
	}
	select {
	case <-bt.started:
	case <-time.After(time.Second):
		t.Fatal("image download did not start")
	}
	if err := env.p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	for key := range env.store.rows(tableName) {
		if strings.HasPrefix(key, "m:") {
			t.Fatalf("memory written during stop: %s", key)
		}
	}
}

func TestStopErrorOnHungBackground(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	release := make(chan struct{})
	defer close(release)
	env.p.runBackground(func() {
		<-release
	})
	start := time.Now()
	err := env.p.Stop(context.Background())
	if err == nil {
		t.Fatal("expected shutdown incomplete error")
	}
	if !strings.Contains(err.Error(), "关闭不完整") {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 4*time.Second {
		t.Fatalf("Stop returned too early: %v", elapsed)
	}
}
