package chatagent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCommandAuthorization(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	if got := runCommand(t, env.p, commandEvent("chat", "status", "nobody", "g")); got != notAdminLine {
		t.Fatalf("non-admin=%q", got)
	}
	waitLog(t, env.logs, "result=denied")
	got := runCommand(t, env.p, commandEvent("chat-admin", "status", "nobody", "g"))
	if strings.HasPrefix(got, notAdminLine) {
		t.Fatalf("chat-admin should bypass plugin admin check: %q", got)
	}
	waitLog(t, env.logs, "command=chat-admin")
}

func TestCommandsBypassOffPolicyAndDisabledSession(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled": false,
		"group_policy":   "off",
	})
	if got := runCommand(t, env.p, commandEvent("chat", "status", "admin", "g")); !strings.HasPrefix(got, "chat-agent: enabled=") {
		t.Fatalf("status blocked by policy: %q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "off", "admin", "g")); got != "chat-agent: 本会话已停用" {
		t.Fatalf("off=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "status", "admin", "g")); !strings.Contains(got, "enabled=false") {
		t.Fatalf("status in disabled session=%q", got)
	}
}

func TestCommandHelpAndUsage(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	if got := runCommand(t, env.p, commandEvent("chat", "help", "admin", "g")); got != usageLine {
		t.Fatalf("help=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "", "admin", "g")); got != usageLine {
		t.Fatalf("bare=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "wat", "admin", "g")); got != usageLine {
		t.Fatalf("unknown=%q", got)
	}
}

func TestCommandStatusFormat(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	s := env.p.getOrCreateSession(groupEvent("x", "u", "x", "g"))
	env.p.appendUserTurn(s, "hello", nil, "u", "U", "e1", env.p.now())
	got := runCommand(t, env.p, commandEvent("chat", "status", "admin", "g"))
	want := "chat-agent: enabled=true kind=group policy=open history=1 vector=ready memory=0 knowledge=0/0"
	if got != want {
		t.Fatalf("status=%q want=%q", got, want)
	}
}

func TestSessionToggleCommands(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	if got := runCommand(t, env.p, commandEvent("chat", "off", "admin", "g")); got != "chat-agent: 本会话已停用" {
		t.Fatalf("off=%q", got)
	}
	s := env.p.getSession(sessionKey("mock", "bot", "group", "g", "admin"))
	if s == nil {
		t.Fatal("session not created")
	}
	s.mu.Lock()
	disabled := s.disabled
	s.mu.Unlock()
	if !disabled {
		t.Fatal("disabled flag not set")
	}
	raw := waitStored(t, env.store, s.storageKey)
	var v sessionJSON
	if err := json.Unmarshal(raw, &v); err != nil || !v.Disabled {
		t.Fatalf("session json=%+v err=%v", v, err)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "status", "admin", "g")); !strings.Contains(got, "enabled=false") {
		t.Fatalf("status=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "on", "admin", "g")); got != "chat-agent: 本会话已启用" {
		t.Fatalf("on=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "reset", "admin", "g")); got != "chat-agent: 本会话已重置" {
		t.Fatalf("reset=%q", got)
	}
	if !waitMissing(env.store, s.storageKey) {
		t.Fatal("reset must delete session override")
	}
}

func TestPolicyAndListCommands(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled": false,
		"group_policy":   "open",
		"group_list":     []string{"a", "b"},
		"private_list":   []string{},
	})
	if got := runCommand(t, env.p, commandEvent("chat", "policy", "admin", "g")); got != "policy: group=open private=open" {
		t.Fatalf("policy=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "list", "admin", "g")); got != "list group: a,b\nlist private: (empty)" {
		t.Fatalf("list=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "list group", "admin", "g")); got != "list group: a,b" {
		t.Fatalf("list group=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "list group add c", "admin", "g")); got != "list group: 已添加 c" {
		t.Fatalf("add=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "list group add c", "admin", "g")); got != "list group: 已添加 c" {
		t.Fatalf("duplicate add=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "list group del zzz", "admin", "g")); got != "list group: 已删除 zzz" {
		t.Fatalf("del missing=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "list group add :bad", "admin", "g")); !strings.HasPrefix(got, "chat-agent: 参数错误:") {
		t.Fatalf("bad id=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "policy group blacklist", "admin", "g")); got != "policy: group=blacklist" {
		t.Fatalf("set=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "policy group", "admin", "g")); !strings.HasPrefix(got, "chat-agent: 参数错误:") {
		t.Fatalf("missing mode=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "policy off", "admin", "g")); !strings.HasPrefix(got, "chat-agent: 参数错误:") {
		t.Fatalf("bad form=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "policy reset", "admin", "g")); got != "policy: 已重置" {
		t.Fatalf("reset=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "policy", "admin", "g")); got != "policy: group=open private=open" {
		t.Fatalf("after reset=%q", got)
	}
}

func TestAdminCommands(t *testing.T) {
	env := newTestEnv(t, map[string]any{
		"random_enabled": false,
		"super_admins":   []string{"mock:admin"},
	})
	if got := runCommand(t, env.p, commandEvent("chat", "admins", "admin", "g")); got != "admins: mock:admin" {
		t.Fatalf("list=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "admins add telegram:42", "admin", "g")); got != "admins: 已添加 telegram:42" {
		t.Fatalf("add=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "admins add telegram:42", "admin", "g")); got != "admins: 已添加 telegram:42" {
		t.Fatalf("duplicate add=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "admins", "admin", "g")); got != "admins: mock:admin,telegram:42" {
		t.Fatalf("list=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "admins del telegram:nobody", "admin", "g")); got != "admins: 已删除 telegram:nobody" {
		t.Fatalf("del missing=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "admins del telegram:42", "admin", "g")); got != "admins: 已删除 telegram:42" {
		t.Fatalf("del=%q", got)
	}
	waitAdmins(t, env.store, "mock:admin")
	if got := runCommand(t, env.p, commandEvent("chat", "admins add telegram:99", "admin", "g")); got != "admins: 已添加 telegram:99" {
		t.Fatalf("add=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "admins reset", "admin", "g")); got != "admins: 已重置" {
		t.Fatalf("reset=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "admins", "admin", "g")); got != "admins: mock:admin" {
		t.Fatalf("after reset=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "admins del mock:admin", "admin", "g")); got != lastAdminLine {
		t.Fatalf("last admin=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "admins", "admin", "g")); got != "admins: mock:admin" {
		t.Fatalf("last admin modified: %q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "admins add bad", "admin", "g")); !strings.HasPrefix(got, "chat-agent: 参数错误:") {
		t.Fatalf("bad=%q", got)
	}
}

func TestPolicyAndAdminPersistenceAcrossRestart(t *testing.T) {
	store := newMemStore()
	env := newTestEnvWithStore(t, store, map[string]any{
		"random_enabled": false,
		"super_admins":   []string{"mock:admin"},
	})
	runCommand(t, env.p, commandEvent("chat", "policy group whitelist", "admin", "g"))
	runCommand(t, env.p, commandEvent("chat", "list group add telegram:-1", "admin", "g"))
	runCommand(t, env.p, commandEvent("chat", "admins add telegram:99", "admin", "g"))
	waitPolicyContains(t, store, "telegram:-1")
	waitAdmins(t, store, "mock:admin,telegram:99")

	env2 := newTestEnvWithStore(t, store, map[string]any{
		"random_enabled": false,
		"super_admins":   []string{"mock:admin"},
	})
	if got := runCommand(t, env2.p, commandEvent("chat", "policy", "admin", "g")); got != "policy: group=whitelist private=open" {
		t.Fatalf("policy after restart=%q", got)
	}
	if got := runCommand(t, env2.p, commandEvent("chat", "list group", "admin", "g")); got != "list group: telegram:-1" {
		t.Fatalf("list after restart=%q", got)
	}
	if got := runCommand(t, env2.p, commandEvent("chat", "admins", "admin", "g")); got != "admins: mock:admin,telegram:99" {
		t.Fatalf("admins after restart=%q", got)
	}
}

func TestSessionOverridePersistsAcrossRestart(t *testing.T) {
	store := newMemStore()
	env := newTestEnvWithStore(t, store, map[string]any{"random_enabled": false})
	runCommand(t, env.p, commandEvent("chat", "off", "admin", "g"))
	s := env.p.getSession(sessionKey("mock", "bot", "group", "g", "admin"))
	waitStored(t, store, s.storageKey)

	env2 := newTestEnvWithStore(t, store, map[string]any{"random_enabled": false})
	s2 := env2.p.getOrCreateSession(groupEvent("x", "u", "x", "g"))
	env2.p.waitOverride(s2)
	s2.mu.Lock()
	disabled := s2.disabled
	s2.mu.Unlock()
	if !disabled {
		t.Fatal("session disabled override not loaded after restart")
	}
}

func TestJobsAndVectorCommands(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	if got := runCommand(t, env.p, commandEvent("chat", "jobs", "admin", "g")); !strings.HasPrefix(got, "jobs: consolidate=idle last=never next=never cleanup=idle") {
		t.Fatalf("jobs status=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "jobs run cleanup", "admin", "g")); got != "jobs: 已触发 cleanup" {
		t.Fatalf("run cleanup=%q", got)
	}
	sn := env.bot.waitSends(1, time.Second)
	if len(sn) != 1 || !strings.HasPrefix(messageText(sn[0].msg), "jobs: 完成 cleanup: sessions_removed=") {
		t.Fatalf("cleanup completion=%v", sn)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "jobs run consolidate", "admin", "g")); got != "jobs: 已触发 consolidate" {
		t.Fatalf("run consolidate=%q", got)
	}
	sn = env.bot.waitSends(2, time.Second)
	if len(sn) < 2 || !strings.Contains(messageText(sn[1].msg), "jobs: 完成 consolidate: sessions_done=") {
		t.Fatalf("consolidate completion=%v", sn)
	}
	env.p.jobs.mu.Lock()
	env.p.jobs.consolidateRunning = true
	env.p.jobs.mu.Unlock()
	if got := runCommand(t, env.p, commandEvent("chat", "jobs run consolidate", "admin", "g")); got != "jobs: 失败 consolidate: job_busy" {
		t.Fatalf("busy=%q", got)
	}
	env.p.jobs.mu.Lock()
	env.p.jobs.consolidateRunning = false
	env.p.jobs.mu.Unlock()

	if got := runCommand(t, env.p, commandEvent("chat", "vector", "admin", "g")); !strings.HasPrefix(got, "vector: state=ready model=test-embed dims=0 memory=0 knowledge=0 stale=0") {
		t.Fatalf("vector status=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "vector reindex", "admin", "g")); got != "vector: 已开始重建" {
		t.Fatalf("reindex=%q", got)
	}
	sn = env.bot.waitSends(3, time.Second)
	if len(sn) < 3 || messageText(sn[2].msg) != "vector: 完成 reindexed=0" {
		t.Fatalf("reindex completion=%v", sn)
	}
	env.p.jobs.mu.Lock()
	env.p.jobs.reindexRunning = true
	env.p.jobs.mu.Unlock()
	if got := runCommand(t, env.p, commandEvent("chat", "vector reindex", "admin", "g")); got != "chat-agent: 忙碌: vector" {
		t.Fatalf("busy=%q", got)
	}
}

func TestVectorCommandDisabledAndLoading(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	env.p.idx.mu.Lock()
	env.p.idx.state = vectorStateLoading
	env.p.idx.mu.Unlock()
	if got := runCommand(t, env.p, commandEvent("chat", "vector reindex", "admin", "g")); got != notReadyLine {
		t.Fatalf("loading reindex=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "jobs run consolidate", "admin", "g")); got != notReadyLine {
		t.Fatalf("loading consolidate=%q", got)
	}
	env.p.idx.mu.Lock()
	env.p.idx.state = vectorStateDisabled
	env.p.idx.mu.Unlock()
	if got := runCommand(t, env.p, commandEvent("chat", "vector", "admin", "g")); !strings.HasPrefix(got, "vector: state=disabled") {
		t.Fatalf("disabled status=%q", got)
	}
}

func TestCommandParameterErrors(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	cases := []string{
		"on extra",
		"status extra",
		"list wat",
		"list group wat x",
		"admins wat",
		"jobs run wat",
		"jobs status extra",
		"vector wat",
		"vector status extra",
		"memory status extra",
		"knowledge list extra",
	}
	for _, args := range cases {
		got := runCommand(t, env.p, commandEvent("chat", args, "admin", "g"))
		if !strings.HasPrefix(got, usageLine) && !strings.HasPrefix(got, "chat-agent: 参数错误:") {
			t.Fatalf("%q -> %q", args, got)
		}
	}
	if got := runCommand(t, env.p, commandEvent("chat", "knowledge add", "admin", "g")); !strings.HasPrefix(got, "chat-agent: 参数错误:") {
		t.Fatalf("knowledge add=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "memory reset", "admin", "g")); !strings.HasPrefix(got, "chat-agent: 参数错误:") {
		t.Fatalf("memory reset=%q", got)
	}
}

func TestCommandDefaults(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	if got := runCommand(t, env.p, commandEvent("chat", "memory", "admin", "g")); !strings.HasPrefix(got, "memory: records=") {
		t.Fatalf("memory default=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "knowledge", "admin", "g")); !strings.HasPrefix(got, "knowledge: ") {
		t.Fatalf("knowledge default=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "jobs", "admin", "g")); !strings.HasPrefix(got, "jobs: ") {
		t.Fatalf("jobs default=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "vector", "admin", "g")); !strings.HasPrefix(got, "vector: ") {
		t.Fatalf("vector default=%q", got)
	}
	if got := runCommand(t, env.p, commandEvent("chat", "admins list", "admin", "g")); got != "admins: mock:admin" {
		t.Fatalf("admins list=%q", got)
	}
}

func TestCommandAuditFields(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	runCommand(t, env.p, commandEvent("chat", "status", "admin", "g"))
	waitLog(t, env.logs, "operation=admin_command")
	waitLog(t, env.logs, "command=chat")
	waitLog(t, env.logs, "subcommand=status")
	waitLog(t, env.logs, "result=ok")
}

func waitAdmins(t *testing.T, store *memStore, want string) {
	t.Helper()
	for range 200 {
		store.mu.Lock()
		raw, ok := store.kv[keyAdmins]
		store.mu.Unlock()
		if ok {
			var v adminsJSON
			if json.Unmarshal(raw, &v) == nil && strings.Join(v.SuperAdmins, ",") == want {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("admins never became %q", want)
}

func waitPolicyContains(t *testing.T, store *memStore, substr string) {
	t.Helper()
	for range 200 {
		store.mu.Lock()
		raw, ok := store.kv[keyPolicy]
		store.mu.Unlock()
		if ok && strings.Contains(string(raw), substr) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("policy never contained %q", substr)
}

func waitMissing(store *memStore, key string) bool {
	for range 200 {
		store.mu.Lock()
		_, ok := store.kv[key]
		store.mu.Unlock()
		if !ok {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestSessionLazyOverrideWait(t *testing.T) {
	env := newTestEnv(t, map[string]any{"random_enabled": false})
	s := &sessionState{key: "test:override", overrideDone: make(chan struct{})}
	done := s.overrideDone
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(done)
	}()
	start := time.Now()
	env.p.waitOverride(s)
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("waitOverride blocked too long")
	}
}
