package chatagent

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPutRowProtocol(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	ctx := context.Background()
	if err := env.p.putRowWait(ctx, "probe", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if raw, ok := env.store.row(tableName, "probe"); !ok || string(raw) != "one" {
		t.Fatalf("row=%q ok=%v", raw, ok)
	}
	if err := env.p.putRowWait(ctx, "probe", []byte("two")); err != nil {
		t.Fatal(err)
	}
	if raw, ok := env.store.row(tableName, "probe"); !ok || string(raw) != "two" {
		t.Fatalf("row=%q ok=%v", raw, ok)
	}
	if err := env.p.deleteRowWait(ctx, "missing"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
	if err := env.p.deleteRowWait(ctx, "probe"); err != nil {
		t.Fatal(err)
	}
	if _, ok := env.store.row(tableName, "probe"); ok {
		t.Fatal("row not deleted")
	}
	if raw, err := env.p.readKey(ctx, "missing-kv"); err != nil || raw != nil {
		t.Fatalf("read missing raw=%v err=%v", raw, err)
	}
}

func TestWriteQueueRejectsAfterStop(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	if err := env.p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := env.p.putRowWait(context.Background(), "late", []byte("x")); err == nil {
		t.Fatal("write after stop succeeded")
	}
}

func TestSessionStorageKeyShape(t *testing.T) {
	group := sessionStorageKey("telegram", "bot", "-100123")
	private := sessionStorageKey("telegram", "bot", "user:42")
	if len(group) != len("session:")+64 || group[:8] != "session:" {
		t.Fatalf("group key=%q", group)
	}
	if group == private {
		t.Fatal("group and private keys collide")
	}
	if again := sessionStorageKey("telegram", "bot", "-100123"); again != group {
		t.Fatal("session key not deterministic")
	}
	if id := hash16("scope", "key", "text"); len(id) != 32 {
		t.Fatalf("hash16=%q", id)
	}
}

func TestRecordArraysAreNotEmptyJSON(t *testing.T) {
	mem := string(marshalJSON(memoryRecordOf(&memoryEntry{
		ID: "a", Scope: "channel", ScopeKey: "g", Kind: "fact", Text: "t",
		CreatedAt: time.Unix(0, 0).UTC(), UpdatedAt: time.Unix(0, 0).UTC(), LastUsedAt: time.Unix(0, 0).UTC(),
	})))
	if strings.Contains(mem, `"sources":null`) || !strings.Contains(mem, `"sources":[]`) {
		t.Fatalf("memory sources=%s", mem)
	}
	doc := string(marshalJSON(docRecordOf(&docEntry{
		ID: "d", CreatedAt: time.Unix(0, 0).UTC(), UpdatedAt: time.Unix(0, 0).UTC(),
	})))
	if strings.Contains(doc, `"chunk_ids":null`) || !strings.Contains(doc, `"chunk_ids":[]`) {
		t.Fatalf("doc chunk ids=%s", doc)
	}
	pol := string(marshalJSON(policyJSON{GroupList: []string{}, PrivateList: []string{}}))
	if strings.Contains(pol, "null") {
		t.Fatalf("policy=%s", pol)
	}
}

func TestSafeTextAndTruncation(t *testing.T) {
	if got := safeText("a<b>c\n\td"); got != "a‹b›c  d" {
		t.Fatalf("safeText=%q", got)
	}
	if got := truncateRunes("abcdef", 3); got != "ab…" {
		t.Fatalf("truncate=%q", got)
	}
	if got := takePrefix("abcdef", 3); got != "abc" {
		t.Fatalf("prefix=%q", got)
	}
	if runeLen("你好") != 2 {
		t.Fatal("rune length")
	}
	if normalizeText("  a \t b\n c ") != "a b c" {
		t.Fatal("normalizeText")
	}
	if normalizeReplyText(" A  B ") != "a b" {
		t.Fatal("normalizeReplyText")
	}
}

func TestMemoryAddRejectsInvalidLength(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	if got := runCommand(t, env.p, commandEvent("chat", "memory add abc", "admin", "g")); got != "memory: 失败 invalid_text" {
		t.Fatalf("short=%q", got)
	}
	long := strings.Repeat("x", memoryTextMaxRunes+1)
	if got := runCommand(t, env.p, commandEvent("chat", "memory add "+long, "admin", "g")); got != "memory: 失败 invalid_text" {
		t.Fatalf("long=%q", got)
	}
}
