package chatagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/RandomLemon/kei/pkg/bot"
)

type commandDeferKey struct{}

// commandDeferrer 收集命令异步任务，确保触发输出先于任务启动。
type commandDeferrer struct {
	mu    sync.Mutex
	funcs []func()
}

func (d *commandDeferrer) add(fn func()) {
	d.mu.Lock()
	d.funcs = append(d.funcs, fn)
	d.mu.Unlock()
}

func (d *commandDeferrer) run() {
	d.mu.Lock()
	funcs := d.funcs
	d.funcs = nil
	d.mu.Unlock()
	for _, fn := range funcs {
		fn()
	}
}

// deferStart 在命令回复发送后执行 fn；没有 deferrer 时立即执行。
func (p *Plugin) deferStart(ctx context.Context, fn func()) {
	if d, ok := ctx.Value(commandDeferKey{}).(*commandDeferrer); ok {
		d.add(fn)
		return
	}
	fn()
}

// deferCommandTask 在命令回复发送后启动后台任务。
func (p *Plugin) deferCommandTask(ctx context.Context, fn func()) {
	p.deferStart(ctx, func() { p.runBackground(fn) })
}

const (
	usageLine     = "chat-agent: 用法: /chat <status|on|off|reset|policy|list|admins|memory|knowledge|jobs|vector|help>"
	notAdminLine  = "chat-agent: 无权执行该命令"
	lastAdminLine = "chat-agent: 不能删除最后一个超级管理员"
	notReadyLine  = "chat-agent: 向量索引未就绪"
)

// handleCommand 是 `/chat` 与 `/chat-admin` 的共同入口。
func (p *Plugin) handleCommand(ctx context.Context, ev *bot.Event, r bot.Reply) error {
	if p.stopping.Load() {
		return nil
	}
	start := time.Now()
	commandName := "chat"
	if ev != nil && ev.Command != nil && ev.Command.Name == "chat-admin" {
		commandName = "chat-admin"
	}
	subcommand := ""
	if ev != nil && ev.Command != nil && len(ev.Command.Args) > 0 {
		subcommand = ev.Command.Args[0]
	}
	result := "ok"
	defer func() {
		fields := []any{
			"operation", "admin_command",
			"command", commandName,
			"subcommand", subcommand,
			"result", result,
			"duration_ms", time.Since(start).Milliseconds(),
		}
		if ev != nil {
			fields = append(fields, "platform", ev.Platform)
			if ev.Sender != nil {
				fields = append(fields, "user", ev.Sender.ID)
			}
		}
		p.log.Info("admin command", fields...)
	}()

	if commandName == "chat" && !p.admins.isSuper(ev) {
		result = "denied"
		if err := r.Text(notAdminLine).Send(ctx); err != nil {
			result = "error"
		}
		return nil
	}
	if ev == nil || ev.Command == nil {
		if err := r.Text(usageLine).Send(ctx); err != nil {
			result = "error"
		}
		return nil
	}
	deferrer := &commandDeferrer{}
	cmdCtx := context.WithValue(ctx, commandDeferKey{}, deferrer)
	out := p.executeCommand(cmdCtx, ev, ev.Command)
	var sendErr error
	if out != "" {
		sendErr = r.Text(out).Send(ctx)
	}
	deferrer.run()
	if sendErr != nil {
		result = "error"
	}
	return nil
}

// executeCommand 执行一条已授权命令并返回同步输出。
func (p *Plugin) executeCommand(ctx context.Context, ev *bot.Event, cmd *bot.Command) string {
	args := cmd.Args
	if len(args) == 0 {
		return usageLine
	}
	switch args[0] {
	case "help":
		return usageLine
	case "status":
		if len(args) != 1 {
			return paramError("status 不接受参数")
		}
		return p.commandStatus(ev)
	case "on", "off", "reset":
		if len(args) != 1 {
			return paramError("on/off/reset 不接受参数")
		}
		return p.commandSessionToggle(ev, args[0])
	case "policy":
		return p.commandPolicy(args)
	case "list":
		return p.commandList(args)
	case "admins":
		return p.commandAdmins(args)
	case "memory":
		return p.commandMemory(ctx, ev, cmd, args[1:])
	case "knowledge":
		return p.commandKnowledge(ctx, ev, cmd, args[1:])
	case "jobs":
		return p.commandJobs(ctx, ev, args[1:])
	case "vector":
		return p.commandVector(ctx, ev, args[1:])
	default:
		return usageLine
	}
}

func paramError(detail string) string {
	return "chat-agent: 参数错误: " + detail
}

func (p *Plugin) commandStatus(ev *bot.Event) string {
	kind := commandKind(ev)
	enabled := true
	history := 0
	if s := p.commandSession(ev); s != nil {
		s.mu.Lock()
		enabled = !s.disabled
		history = len(s.history)
		s.mu.Unlock()
	}
	mode := ""
	if kind != "" {
		mode = p.policy.modeFor(kind)
	}
	vectorState := vectorStateDisabled
	memories := 0
	docs, chunks := 0, 0
	if p.idx != nil {
		vectorState = p.idx.statusState()
		memories = p.idx.memoryRows()
		docs, chunks = p.idx.knowledgeStatus()
	}
	return fmt.Sprintf("chat-agent: enabled=%t kind=%s policy=%s history=%d vector=%s memory=%d knowledge=%d/%d",
		enabled, kind, mode, history, vectorState, memories, docs, chunks)
}

func (p *Plugin) commandSessionToggle(ev *bot.Event, action string) string {
	s := p.getOrCreateSession(ev)
	if s == nil {
		return paramError("仅支持群聊与私聊")
	}
	switch action {
	case "on":
		p.setDisabled(s, false)
		return "chat-agent: 本会话已启用"
	case "off":
		p.setDisabled(s, true)
		return "chat-agent: 本会话已停用"
	case "reset":
		p.resetDisabled(s)
		return "chat-agent: 本会话已重置"
	default:
		return usageLine
	}
}

func (p *Plugin) commandPolicy(args []string) string {
	if len(args) == 1 {
		snap := p.policy.snapshot()
		return fmt.Sprintf("policy: group=%s private=%s", snap.GroupMode, snap.PrivateMode)
	}
	if len(args) == 2 && args[1] == "reset" {
		p.policy.reset()
		p.kvDelete(keyPolicy)
		return "policy: 已重置"
	}
	if len(args) != 3 {
		return paramError("policy 需要 <group|private> <mode> 或 reset")
	}
	kind := args[1]
	if kind != "group" && kind != "private" {
		return paramError("policy 类型必须是 group 或 private")
	}
	mode := args[2]
	if !validMode(mode) {
		return paramError("模式必须是 off|open|whitelist|blacklist 之一")
	}
	p.policy.setMode(kind, mode)
	p.persistPolicy()
	return fmt.Sprintf("policy: %s=%s", kind, mode)
}

func (p *Plugin) commandList(args []string) string {
	if len(args) == 1 {
		snap := p.policy.snapshot()
		return "list group: " + formatList(snap.GroupList) + "\nlist private: " + formatList(snap.PrivateList)
	}
	if len(args) == 2 {
		if args[1] != "group" && args[1] != "private" {
			return paramError("list 类型必须是 group 或 private")
		}
		snap := p.policy.snapshot()
		if args[1] == "group" {
			return "list group: " + formatList(snap.GroupList)
		}
		return "list private: " + formatList(snap.PrivateList)
	}
	if len(args) != 4 {
		return paramError("list 需要 <group|private> add|del <id>")
	}
	kind, op, id := args[1], args[2], normalizePlatformEntry(args[3])
	if kind != "group" && kind != "private" {
		return paramError("list 类型必须是 group 或 private")
	}
	if op != "add" && op != "del" {
		return paramError("list 操作必须是 add 或 del")
	}
	if !validListEntry(id) {
		return paramError("名单条目不能为空")
	}
	switch op {
	case "add":
		if p.policy.addEntry(kind, id) {
			p.persistPolicy()
		}
		return fmt.Sprintf("list %s: 已添加 %s", kind, id)
	default:
		if p.policy.delEntry(kind, id) {
			p.persistPolicy()
		}
		return fmt.Sprintf("list %s: 已删除 %s", kind, id)
	}
}

func validListEntry(id string) bool {
	if id == "" {
		return false
	}
	if platform, userID, ok := strings.Cut(id, ":"); ok {
		return strings.TrimSpace(platform) != "" && strings.TrimSpace(userID) != ""
	}
	return true
}

func formatList(list []string) string {
	if len(list) == 0 {
		return "(empty)"
	}
	return strings.Join(list, ",")
}

func (p *Plugin) commandAdmins(args []string) string {
	if len(args) == 1 || (len(args) == 2 && args[1] == "list") {
		if len(args) == 2 {
			return "admins: " + formatList(p.admins.list())
		}
		return "admins: " + formatList(p.admins.list())
	}
	if len(args) == 2 && args[1] == "reset" {
		p.admins.reset()
		p.kvDelete(keyAdmins)
		if len(p.admins.list()) == 0 {
			p.log.Warn("no super admin configured; use /chat-admin to recover")
		}
		return "admins: 已重置"
	}
	if len(args) != 3 {
		return paramError("admins 需要 list|add <id>|del <id>|reset")
	}
	id := normalizePlatformEntry(args[2])
	if !validPlatformUser(id) {
		return paramError("管理员条目必须是 platform:userID")
	}
	switch args[1] {
	case "add":
		if p.admins.add(id) {
			p.persistAdmins()
		}
		return "admins: 已添加 " + id
	case "del":
		removed, last := p.admins.del(id)
		if last {
			return lastAdminLine
		}
		if removed {
			p.persistAdmins()
		}
		return "admins: 已删除 " + id
	default:
		return paramError("admins 需要 list|add <id>|del <id>|reset")
	}
}

func (p *Plugin) commandMemory(ctx context.Context, ev *bot.Event, cmd *bot.Command, args []string) string {
	if !p.cfg.MemoryEnabled {
		return "chat-agent: 记忆功能未启用"
	}
	if p.idx == nil || p.idx.statusState() != vectorStateReady {
		return notReadyLine
	}
	if len(args) == 0 || args[0] == "status" {
		if len(args) > 1 {
			return paramError("memory status 不接受参数")
		}
		return p.memoryStatusText()
	}
	switch args[0] {
	case "search":
		query := strings.TrimSpace(subRaw(cmd.Raw, 2))
		if query == "" {
			return paramError("memory search 需要查询文本")
		}
		if p.cfg.MemoryTopK == 0 {
			return "memory search: 无结果"
		}
		if !p.jobs.tryExclusive("memory") {
			return "chat-agent: 忙碌: memory"
		}
		target := sessionTargetFromEvent(ev)
		p.deferCommandTask(ctx, func() {
			defer p.jobs.finish("memory")
			taskCtx, cancel := context.WithTimeout(p.ctx, p.cfg.EmbeddingTimeout)
			defer cancel()
			out, err := p.memorySearchText(taskCtx, ev, query)
			if err != nil {
				p.sendAsync(target, "memory search: 失败 "+memoryFailureReason(err))
				return
			}
			p.sendAsync(target, out)
		})
		return "memory search: 已开始"
	case "add":
		text := strings.TrimSpace(subRaw(cmd.Raw, 2))
		if text == "" {
			return paramError("memory add 需要文本")
		}
		if n := runeLen(text); n < memoryTextMinRunes || n > memoryTextMaxRunes {
			return "memory: 失败 invalid_text"
		}
		return p.memoryAdd(ctx, ev, text)
	case "del":
		if len(args) != 2 {
			return paramError("memory del 需要 <id>")
		}
		return p.memoryDel(ctx, ev, args[1])
	case "reset":
		if len(args) != 2 || args[1] != "confirm" {
			return paramError("memory reset 需要 confirm")
		}
		return p.memoryReset(ctx, ev)
	default:
		return usageLine
	}
}

func (p *Plugin) memoryStatusText() string {
	last := "never"
	_, _, consolidateLast, _, _, _ := p.jobs.status()
	if !consolidateLast.IsZero() {
		last = consolidateLast.UTC().Format(time.RFC3339)
	}
	pending := 0
	p.sessionsMu.Lock()
	sessions := make([]*sessionState, 0, len(p.sessions))
	for _, s := range p.sessions {
		sessions = append(sessions, s)
	}
	p.sessionsMu.Unlock()
	for _, s := range sessions {
		s.mu.Lock()
		if s.consolidatedSeq < s.seq {
			pending++
		}
		s.mu.Unlock()
	}
	return fmt.Sprintf("memory: records=%d stale=%d pending_sessions=%d last=%s",
		p.idx.memoryRows(), p.idx.staleMemoryRows(), pending, last)
}

func (p *Plugin) memoryAdd(ctx context.Context, ev *bot.Event, text string) string {
	s := p.getOrCreateSession(ev)
	if s == nil {
		return paramError("仅支持群聊与私聊")
	}
	if !p.jobs.tryExclusive("memory") {
		return "chat-agent: 忙碌: memory"
	}
	target := sessionTargetFromEvent(ev)
	eventID := ev.ID
	p.deferCommandTask(ctx, func() {
		defer p.jobs.finish("memory")
		taskCtx, cancel := context.WithTimeout(p.ctx, p.cfg.MemoryConsolidateTimeout)
		defer cancel()
		mem := extractedMemory{Text: text, Kind: "fact", Importance: 0.8}
		_, _, ids, err := p.storeMemories(taskCtx, s, []extractedMemory{mem}, []string{eventID})
		if err != nil {
			p.sendAsync(target, "memory: 失败 "+memoryFailureReason(err))
			return
		}
		id := ""
		if len(ids) > 0 {
			id = ids[0]
		}
		p.sendAsync(target, "memory: 完成 "+id)
	})
	return "memory: 已接受"
}

func (p *Plugin) memoryDel(ctx context.Context, ev *bot.Event, id string) string {
	if _, ok := p.idx.memoryByID(id); !ok {
		return "chat-agent: 未找到: " + id
	}
	if !p.jobs.tryExclusive("memory") {
		return "chat-agent: 忙碌: memory"
	}
	target := sessionTargetFromEvent(ev)
	p.deferCommandTask(ctx, func() {
		defer p.jobs.finish("memory")
		taskCtx, cancel := context.WithTimeout(p.ctx, p.cfg.MemoryConsolidateTimeout)
		defer cancel()
		entry, claimed := p.idx.claimMemory(id)
		if err := p.deleteRowWait(taskCtx, memoryRowKey(id)); err != nil {
			if claimed {
				p.idx.restoreMemory(entry)
			}
			p.sendAsync(target, "memory: 失败 "+storageFailureReason(err))
			return
		}
		p.sendAsync(target, "memory: 完成 "+id)
	})
	return "memory: 已接受"
}

func (p *Plugin) memoryReset(ctx context.Context, ev *bot.Event) string {
	if !p.jobs.tryExclusive("memory") {
		return "chat-agent: 忙碌: memory"
	}
	target := sessionTargetFromEvent(ev)
	p.deferCommandTask(ctx, func() {
		defer p.jobs.finish("memory")
		taskCtx, cancel := context.WithTimeout(p.ctx, p.cfg.MemoryConsolidateTimeout)
		defer cancel()
		var failure error
		for _, m := range p.idx.memorySnapshot() {
			if err := taskCtx.Err(); err != nil {
				failure = err
				break
			}
			entry, claimed := p.idx.claimMemory(m.ID)
			if !claimed {
				continue
			}
			if err := p.deleteRowWait(taskCtx, memoryRowKey(m.ID)); err != nil {
				p.idx.restoreMemory(entry)
				failure = err
				continue
			}
		}
		for _, key := range p.idx.badMemoryKeys() {
			if err := taskCtx.Err(); err != nil {
				failure = err
				break
			}
			if err := p.deleteRowWait(taskCtx, key); err != nil {
				failure = err
				continue
			}
			p.idx.removeBadMemoryKey(key)
		}
		if failure != nil {
			p.sendAsync(target, "memory: 失败 "+storageFailureReason(failure))
			return
		}
		p.sendAsync(target, "memory: 完成 reset")
	})
	return "memory: 已接受"
}

func storageFailureReason(err error) string {
	switch categoryOf(err) {
	case errTimeout:
		return "timeout"
	case errCanceled:
		return "internal_error"
	default:
		return "storage_error"
	}
}

func (p *Plugin) memorySearchText(ctx context.Context, ev *bot.Event, query string) (string, error) {
	if p.cfg.MemoryTopK == 0 {
		return "memory search: 无结果", nil
	}
	vecs, err := p.embedder.Embed(ctx, []string{takePrefix(query, contextMessageMaxChars)})
	if err != nil {
		switch categoryOf(err) {
		case errTimeout:
			return "", kerr("timeout", err)
		case errCanceled:
			return "", kerr("internal_error", err)
		default:
			return "", kerr("embedding_error", err)
		}
	}
	if len(vecs) != 1 {
		return "", kerr("embedding_error", errors.New("embedding count mismatch"))
	}
	vec, err := normalizeVector(vecs[0])
	if err != nil {
		return "", kerr("embedding_error", err)
	}
	kind := commandKind(ev)
	channelID := ""
	userIDs := make(map[string]struct{})
	if ev.Sender != nil {
		userIDs[ev.Sender.ID] = struct{}{}
	}
	if kind == "group" && ev.Channel != nil {
		channelID = ev.Channel.ID
	}
	hits, mismatch := p.idx.searchMemories(vec, channelID, userIDs, p.cfg.MemoryTopK)
	if mismatch > 0 {
		p.log.Warn("memory search dimension mismatch", "operation", "memory", "skipped", mismatch)
	}
	type row struct {
		id    string
		score float64
		kind  string
		text  string
	}
	rows := make([]row, 0, len(hits))
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		if h.Score < p.cfg.MemoryMinScore {
			continue
		}
		entry, ok := p.idx.memoryByID(h.ID)
		if !ok {
			continue
		}
		rows = append(rows, row{id: entry.ID, score: h.Score, kind: entry.Kind, text: entry.Text})
		ids = append(ids, entry.ID)
	}
	if len(ids) > 0 {
		p.idx.reinforce(ids, p.now().UTC())
	}
	if len(rows) == 0 {
		return "memory search: 无结果", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "memory search: %d 条结果", len(rows))
	for _, r := range rows {
		fmt.Fprintf(&b, "\n%s %.3f %s %s", r.id, r.score, r.kind, r.text)
	}
	return b.String(), nil
}

func (p *Plugin) commandKnowledge(ctx context.Context, ev *bot.Event, cmd *bot.Command, args []string) string {
	if !p.cfg.RAGEnabled {
		return "chat-agent: 知识库功能未启用"
	}
	if p.idx == nil || p.idx.statusState() != vectorStateReady {
		return notReadyLine
	}
	if len(args) == 0 || args[0] == "list" {
		if len(args) > 1 {
			return paramError("knowledge list 不接受参数")
		}
		return p.knowledgeListText()
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			return paramError("knowledge add 需要 <url> [title]")
		}
		rawURL := args[1]
		title := ""
		if len(args) > 2 {
			title = strings.Join(args[2:], " ")
		}
		if !p.jobs.tryExclusive("knowledge") {
			return "knowledge: 失败 busy"
		}
		target := sessionTargetFromEvent(ev)
		p.deferCommandTask(ctx, func() {
			defer p.jobs.finish("knowledge")
			res, err := p.addKnowledgeURL(p.ctx, rawURL, title)
			p.knowledgeNotify(target, res, err)
		})
		return "knowledge: 已开始 " + rawURL
	case "add-text":
		text := strings.TrimSpace(subRaw(cmd.Raw, 2))
		if text == "" {
			return "knowledge: 失败 invalid_text"
		}
		if len(text) > p.cfg.KnowledgeMaxBytes {
			return "knowledge: 失败 too_large"
		}
		if !p.jobs.tryExclusive("knowledge") {
			return "knowledge: 失败 busy"
		}
		target := sessionTargetFromEvent(ev)
		p.deferCommandTask(ctx, func() {
			defer p.jobs.finish("knowledge")
			res, err := p.addKnowledgeText(p.ctx, text)
			p.knowledgeNotify(target, res, err)
		})
		return "knowledge: 已开始 text"
	case "del":
		if len(args) != 2 {
			return paramError("knowledge del 需要 <id>")
		}
		id := args[1]
		if _, ok := p.idx.docByID(id); !ok {
			return "chat-agent: 未找到: " + id
		}
		if !p.jobs.tryExclusive("knowledge") {
			return "knowledge: 失败 busy"
		}
		target := sessionTargetFromEvent(ev)
		p.deferCommandTask(ctx, func() {
			defer p.jobs.finish("knowledge")
			res, err := p.deleteKnowledgeDoc(p.ctx, id)
			p.knowledgeNotify(target, res, err)
		})
		return "knowledge: 已开始 del"
	case "refresh":
		if len(args) > 2 {
			return paramError("knowledge refresh 只接受一个 <id>")
		}
		if len(args) == 2 {
			id := args[1]
			if _, ok := p.idx.docByID(id); !ok {
				return "chat-agent: 未找到: " + id
			}
			if !p.jobs.tryExclusive("knowledge") {
				return "knowledge: 失败 busy"
			}
			target := sessionTargetFromEvent(ev)
			p.deferCommandTask(ctx, func() {
				defer p.jobs.finish("knowledge")
				res, err := p.refreshKnowledgeDoc(p.ctx, id)
				p.knowledgeNotify(target, res, err)
			})
			return "knowledge: 已开始 refresh"
		}
		if !p.jobs.tryExclusive("knowledge") {
			return "knowledge: 失败 busy"
		}
		target := sessionTargetFromEvent(ev)
		docs := p.idx.docSnapshot()
		p.deferCommandTask(ctx, func() {
			defer p.jobs.finish("knowledge")
			for _, doc := range docs {
				if p.ctx.Err() != nil {
					break
				}
				if strings.HasPrefix(doc.Source, "text:") {
					continue
				}
				res, err := p.refreshKnowledgeDoc(p.ctx, doc.ID)
				p.knowledgeNotify(target, res, err)
			}
		})
		return "knowledge: 已开始 refresh"
	default:
		return usageLine
	}
}

func (p *Plugin) knowledgeNotify(target bot.Target, res knowledgeResult, err error) {
	if err != nil {
		p.sendAsync(target, "knowledge: 失败 "+knowledgeReason(err))
		return
	}
	p.sendAsync(target, fmt.Sprintf("knowledge: 完成 doc=%s chunks=%d result=%s",
		res.DocID, res.Chunks, res.Result))
}

func (p *Plugin) knowledgeListText() string {
	docs := p.idx.docSnapshot()
	chunks := 0
	for _, d := range docs {
		chunks += len(d.ChunkIDs)
	}
	if len(docs) == 0 {
		return "knowledge: (empty)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "knowledge: docs=%d chunks=%d", len(docs), chunks)
	for _, d := range docs {
		title := d.Title
		if title == "" {
			title = d.ID
		}
		fmt.Fprintf(&b, "\n%s %s chunks=%d updated=%s",
			d.ID, title, len(d.ChunkIDs), d.UpdatedAt.UTC().Format(time.RFC3339))
	}
	return b.String()
}

func (p *Plugin) commandJobs(ctx context.Context, ev *bot.Event, args []string) string {
	if len(args) == 0 || args[0] == "status" {
		if len(args) > 1 {
			return paramError("jobs status 不接受参数")
		}
		return p.jobsStatusText()
	}
	if len(args) != 2 || args[0] != "run" {
		return paramError("jobs 需要 run consolidate|cleanup")
	}
	name := args[1]
	if name != "consolidate" && name != "cleanup" {
		return paramError("jobs 任务必须是 consolidate 或 cleanup")
	}
	if name == "consolidate" && (p.idx == nil || p.idx.statusState() != vectorStateReady) {
		return notReadyLine
	}
	target := sessionTargetFromEvent(ev)
	notify := func(summary string, err error) {
		if err != nil {
			p.sendAsync(target, "jobs: 失败 "+name+": "+jobFailureReason(err))
			return
		}
		p.sendAsync(target, "jobs: 完成 "+name+": "+summary)
	}
	if !p.jobs.trigger(ctx, name, notify) {
		return "jobs: 失败 " + name + ": job_busy"
	}
	return "jobs: 已触发 " + name
}

func (p *Plugin) jobsStatusText() string {
	consolidateRunning, cleanupRunning, consolidateLast, consolidateNext, cleanupLast, cleanupNext := p.jobs.status()
	return fmt.Sprintf("jobs: consolidate=%s last=%s next=%s cleanup=%s last=%s next=%s",
		runningLabel(consolidateRunning), formatWhen(consolidateLast), formatWhen(consolidateNext),
		runningLabel(cleanupRunning), formatWhen(cleanupLast), formatWhen(cleanupNext))
}

func runningLabel(running bool) string {
	if running {
		return "running"
	}
	return "idle"
}

func formatWhen(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

func (p *Plugin) commandVector(ctx context.Context, ev *bot.Event, args []string) string {
	if len(args) == 0 || args[0] == "status" {
		if len(args) > 1 {
			return paramError("vector status 不接受参数")
		}
		return p.vectorStatusText()
	}
	if len(args) != 1 || args[0] != "reindex" {
		return paramError("vector 需要 status 或 reindex")
	}
	if p.idx == nil || p.idx.statusState() != vectorStateReady {
		return notReadyLine
	}
	if !p.jobs.tryExclusive("reindex") {
		return "chat-agent: 忙碌: vector"
	}
	target := sessionTargetFromEvent(ev)
	p.deferCommandTask(ctx, func() {
		defer p.jobs.finish("reindex")
		taskCtx, cancel := context.WithTimeout(p.ctx, reindexTimeout)
		defer cancel()
		n, _ := p.runReindex(taskCtx)
		p.sendAsync(target, fmt.Sprintf("vector: 完成 reindexed=%d", n))
	})
	return "vector: 已开始重建"
}

func (p *Plugin) vectorStatusText() string {
	if p.idx == nil {
		return "vector: state=disabled model= dims=0 memory=0 knowledge=0 stale=0"
	}
	state, model, dims, memory, knowledge, stale := p.idx.statusSnapshot()
	return fmt.Sprintf("vector: state=%s model=%s dims=%d memory=%d knowledge=%d stale=%d",
		state, model, dims, memory, knowledge, stale)
}

// commandKind 返回命令事件对应的会话类型。
func commandKind(ev *bot.Event) string {
	if ev == nil || ev.Message == nil {
		return ""
	}
	switch ev.Message.Kind {
	case bot.MessageGroup:
		return "group"
	case bot.MessagePrivate:
		return "private"
	default:
		return ""
	}
}

// commandSession 查找命令事件对应的会话，不创建。
func (p *Plugin) commandSession(ev *bot.Event) *sessionState {
	kind := commandKind(ev)
	if kind == "" {
		return nil
	}
	channelID, userID := "", ""
	if ev.Channel != nil {
		channelID = ev.Channel.ID
	}
	if ev.Sender != nil {
		userID = ev.Sender.ID
	}
	return p.getSession(sessionKey(ev.Platform, ev.BotID, kind, channelID, userID))
}

// subRaw 返回命令原始参数中跳过前 n 个词后的剩余部分。
func subRaw(raw string, n int) string {
	s := strings.TrimSpace(raw)
	for range n {
		i := strings.IndexFunc(s, unicode.IsSpace)
		if i < 0 {
			return ""
		}
		s = strings.TrimLeftFunc(s[i:], unicode.IsSpace)
	}
	return s
}

// sendAsync 向触发命令的会话发送异步结果；发送失败只记日志。
func (p *Plugin) sendAsync(target bot.Target, text string) {
	if p.stopping.Load() || p.ctx == nil {
		return
	}
	if err := p.sendText(p.ctx, target, text); err != nil {
		p.log.Warn("async send failed", "operation", "admin_command", "error", err.Error())
	}
}
