package chatagent

import (
	"context"
	"testing"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

func TestPolicyModes(t *testing.T) {
	cfg := mustConfig(t, map[string]any{
		"group_policy":   "whitelist",
		"group_list":     []string{"telegram:-100", "-200"},
		"private_policy": "blacklist",
		"private_list":   []string{"telegram:42"},
	})
	s := newPolicyStore(cfg)
	if !s.allows("group", "telegram", "-100") {
		t.Fatal("qualified whitelist entry should match")
	}
	if !s.allows("group", "mock", "-200") {
		t.Fatal("bare whitelist entry should match any platform")
	}
	if !s.allows("group", "telegram", "-200") {
		t.Fatal("bare entry must match on other platforms")
	}
	if s.allows("group", "telegram", "-999") {
		t.Fatal("unlisted group should be denied")
	}
	if s.allows("private", "telegram", "42") {
		t.Fatal("blacklisted private user should be denied")
	}
	if !s.allows("private", "telegram", "43") {
		t.Fatal("other private user should be allowed")
	}

	off := newPolicyStore(mustConfig(t, map[string]any{"group_policy": "off"}))
	if off.allows("group", "mock", "x") {
		t.Fatal("off must deny")
	}
	open := newPolicyStore(mustConfig(t, map[string]any{"group_policy": "open"}))
	if !open.allows("group", "mock", "x") {
		t.Fatal("open must allow")
	}
	emptyWhite := newPolicyStore(mustConfig(t, map[string]any{"group_policy": "whitelist"}))
	if emptyWhite.allows("group", "mock", "x") {
		t.Fatal("empty whitelist must deny")
	}
	emptyBlack := newPolicyStore(mustConfig(t, map[string]any{"group_policy": "blacklist"}))
	if !emptyBlack.allows("group", "mock", "x") {
		t.Fatal("empty blacklist must allow")
	}
}

func TestPolicyRejectionHasNoSideEffects(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"group_policy": "blacklist",
		"group_list":   []string{"mock:g1"},
	})
	ev := atEvent("e1", "u1", "hello", "g1", "bot")
	if err := env.p.handleGroupMessage(context.Background(), ev, bot.NewNoopReply()); err != nil {
		t.Fatal(err)
	}
	if len(env.bot.sendsSnapshot()) != 0 {
		t.Fatal("rejected message must not send")
	}
	if len(env.p.sessions) != 0 {
		t.Fatalf("rejected message created sessions: %d", len(env.p.sessions))
	}
}

func TestPolicyOverrideLoadAndFallback(t *testing.T) {
	cfg := mustConfig(t, map[string]any{
		"group_policy":   "open",
		"group_list":     []string{"a"},
		"private_policy": "off",
	})
	store := newMemStore()
	p := &Plugin{log: testLogger(), kv: store, cfg: cfg}
	p.policy = newPolicyStore(cfg)
	store.kv[keyPolicy] = []byte(`{"group_mode":"whitelist","group_list":["b"],"private_mode":"open","private_list":[]}`)
	p.policy.load(context.Background(), p)
	if p.policy.modeFor("group") != "whitelist" || !p.policy.allows("group", "x", "b") {
		t.Fatal("override not applied")
	}
	if p.policy.modeFor("private") != "open" {
		t.Fatal("private override not applied")
	}

	bad := newPolicyStore(cfg)
	store.kv[keyPolicy] = []byte(`{"group_mode":"nope","group_list":["b"]}`)
	bad.load(context.Background(), p)
	_ = bad
	if bad.modeFor("group") != "open" || bad.modeFor("private") != "off" {
		t.Fatal("invalid mode must fall back per field")
	}

	corrupt := newPolicyStore(cfg)
	store.kv[keyPolicy] = []byte(`{`)
	corrupt.load(context.Background(), p)
	if corrupt.modeFor("group") != "open" || !corrupt.allows("group", "x", "b") {
		t.Fatal("corrupt override must fall back")
	}
}

func TestPolicyListsPersist(t *testing.T) {
	env := newTestEnv(t, nil)
	out := runCommand(t, env.p, commandEvent("chat", "policy group whitelist", "admin", "g1"))
	if out != "policy: group=whitelist" {
		t.Fatalf("out=%q", out)
	}
	out = runCommand(t, env.p, commandEvent("chat", "list group add telegram:-1", "admin", "g1"))
	if out != "list group: 已添加 telegram:-1" {
		t.Fatalf("out=%q", out)
	}
	waitStored(t, env.store, keyPolicy)
	out = runCommand(t, env.p, commandEvent("chat", "policy", "admin", "g1"))
	if out != "policy: group=whitelist private=open" {
		t.Fatalf("out=%q", out)
	}
	out = runCommand(t, env.p, commandEvent("chat", "policy reset", "admin", "g1"))
	if out != "policy: 已重置" {
		t.Fatalf("out=%q", out)
	}
}

func waitStored(t *testing.T, store *memStore, key string) []byte {
	t.Helper()
	for range 200 {
		store.mu.Lock()
		v, ok := store.kv[key]
		store.mu.Unlock()
		if ok {
			return append([]byte(nil), v...)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("key %s not stored", key)
	return nil
}
