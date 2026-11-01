package chatagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestJobsRunOnStart(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled":    false,
		"jobs_run_on_start": true,
	})
	waitLog(t, env.logs, "operation=consolidate")
	waitLog(t, env.logs, "operation=cleanup")
}

func TestSchedulerNextWhenIntervalsSet(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled":              false,
		"memory_consolidate_interval": "1h",
		"history_cleanup_interval":    "2h",
	})
	got := runCommand(t, env.p, commandEvent("chat", "jobs status", "admin", "g"))
	if strings.Contains(got, "consolidate=idle last=never next=never") {
		t.Fatalf("expected next schedule: %q", got)
	}
	if !strings.Contains(got, "cleanup=idle last=never next=") {
		t.Fatalf("cleanup next missing: %q", got)
	}
}

func TestSchedulerCleanupTicker(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled":           false,
		"history_ttl":              "1ms",
		"history_cleanup_interval": "20ms",
		"memory_enabled":           false,
		"rag_enabled":              false,
	})
	s := env.p.getOrCreateSession(groupEvent("x", "u", "x", "g"))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if env.p.getSession(s.key) == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("cleanup ticker did not remove idle session")
}

func TestReindexEmbeddingFailureKeepsMeta(t *testing.T) {
	store := newSeedStore(t)
	seedMeta(t, store, "test-embed", 4)
	seedMemory(t, store, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "channel", "g", "stale", "old-model", []float32{1, 0, 0, 0}, 0.5)
	env := newTestEnvWithStore(t, store, map[string]any{"random_enabled": false})
	env.emb.err = errTest
	n, err := env.p.runReindex(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	raw, _ := store.row(tableName, tableMetaKey)
	var meta metaJSON
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Model != "test-embed" || meta.Dims != 4 {
		t.Fatalf("meta changed on failure: %+v", meta)
	}
	if _, _, _, _, _, stale := env.p.idx.statusSnapshot(); stale != 1 {
		t.Fatalf("stale=%d", stale)
	}
}

func TestSchedulerTriggerRefusedAfterStop(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := env.p.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if env.p.jobs.trigger(env.p.ctx, "cleanup", nil) {
		t.Fatal("trigger after stop succeeded")
	}
}
