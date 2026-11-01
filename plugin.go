package chatagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	rand "math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// Plugin 是 chat-agent 插件实例。
type Plugin struct {
	lifecycleMu sync.Mutex
	started     bool

	cfg    *config
	log    *slog.Logger
	pctx   bot.PluginContext
	bot    bot.BotAPI
	kv     bot.Storage
	tables bot.TableStorage

	ctx         context.Context
	cancel      context.CancelFunc
	stopping    atomic.Bool
	stopOnce    sync.Once
	stopErr     error
	bgMu        sync.Mutex
	waitStarted bool
	wg          sync.WaitGroup
	sem         chan struct{}

	sessionsMu sync.Mutex
	sessions   map[string]*sessionState

	policy *policyStore
	admins *adminStore

	writer      *writeQueue
	embedWorker *embedQueue
	idx         *vectorIndex
	jobs        *scheduler

	chat         chatCompleter
	embedder     embedder
	visionClient *http.Client
	fetcher      knowledgeFetcher

	now       func() time.Time
	randFloat func() float64
	afterFunc timerFunc
}

// Setup 读取配置、校验权限并注册四条规则。
func (p *Plugin) Setup(ctx context.Context, reg bot.Registrar) error {
	pctx, ok := bot.PluginContextFrom(ctx)
	if !ok {
		return errors.New("chat-agent: 缺少 PluginContext")
	}
	cfg, err := loadConfig(pctx.Config)
	if err != nil {
		return err
	}
	if pctx.HTTPClient == nil {
		return errors.New("chat-agent: 需要 network 权限")
	}
	if pctx.Tables == nil && (cfg.MemoryEnabled || cfg.RAGEnabled) {
		return errors.New("chat-agent: 需要 TableStorage 支持")
	}
	if pctx.Logger == nil {
		pctx.Logger = slog.Default()
	}

	p.pctx = pctx
	p.cfg = cfg
	p.log = pctx.Logger
	p.bot = pctx.Bot
	p.kv = pctx.Storage
	p.tables = pctx.Tables
	if p.now == nil {
		p.now = time.Now
	}
	if p.randFloat == nil {
		p.randFloat = rand.Float64
	}
	if p.afterFunc == nil {
		p.afterFunc = defaultAfterFunc
	}
	p.sessions = make(map[string]*sessionState)
	p.sem = make(chan struct{}, cfg.LimitsMaxConcurrent)
	p.policy = newPolicyStore(cfg)
	p.admins = newAdminStore(cfg)
	p.stopping.Store(false)
	if p.visionClient == nil {
		p.visionClient = newVisionClient(pctx.HTTPClient)
	}
	if p.chat == nil {
		p.chat = newChatCompleter(cfg, pctx.HTTPClient, p.log)
	}
	if p.embedder == nil {
		p.embedder = newHTTPEmbedder(cfg, pctx.HTTPClient, p.log)
	}
	if p.fetcher == nil {
		p.fetcher = newKnowledgeFetcher(cfg, pctx.HTTPClient)
	}
	p.jobs = newScheduler(p)

	reg.OnEvent(bot.EventMessage, p.handleGroupMessage,
		bot.WithKind(bot.MessageGroup), bot.WithPriority(0), bot.WithID("chat-agent:group"))
	reg.OnEvent(bot.EventMessage, p.handlePrivateMessage,
		bot.WithKind(bot.MessagePrivate), bot.WithPriority(0), bot.WithID("chat-agent:private"))
	reg.OnCommand("chat", p.handleCommand,
		bot.WithPriority(100), bot.WithID("chat-agent:admin"))
	reg.OnCommand("chat-admin", p.handleCommand,
		bot.WithAdmin(), bot.WithPriority(100), bot.WithID("chat-agent:core-admin"))
	return nil
}

// Start 启动插件级上下文、写协程与后台任务。
func (p *Plugin) Start(ctx context.Context) error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.cfg == nil {
		return errors.New("chat-agent: 插件未完成 Setup")
	}
	if p.started {
		return nil
	}
	if p.ctx != nil {
		return errors.New("chat-agent: 插件不可重复启动")
	}

	p.ctx, p.cancel = context.WithCancel(context.WithoutCancel(ctx))
	p.stopping.Store(false)

	p.policy.load(p.ctx, p)
	p.admins.load(p.ctx, p)
	if len(p.admins.list()) == 0 {
		p.log.Warn("no super admin configured; use /chat-admin to recover")
	}

	var rows []bot.TableRow
	featuresOn := p.cfg.MemoryEnabled || p.cfg.RAGEnabled
	if featuresOn {
		if p.tables == nil {
			p.shutdownEarly()
			return errors.New("chat-agent: 需要 TableStorage 支持")
		}
		tableCtx, cancel := context.WithTimeout(p.ctx, storageTimeout)
		err := p.tables.CreateTable(tableCtx, tableName)
		cancel()
		if err != nil && !errors.Is(err, bot.ErrTableExists) {
			p.shutdownEarly()
			return fmt.Errorf("chat-agent: 存储表初始化失败: %w", err)
		}
		loadCtx, cancel := context.WithTimeout(p.ctx, tableLoadTimeout)
		rows, err = p.tables.ListRows(loadCtx, tableName)
		cancel()
		if err != nil {
			p.shutdownEarly()
			return fmt.Errorf("chat-agent: 存储表加载失败: %w", err)
		}
	}

	p.writer = newWriteQueue(p.ctx, p.kv, p.tables, p.log)
	p.writer.start()
	if featuresOn {
		p.embedWorker = newEmbedQueue(p.ctx, p.embedder, p.log)
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.embedWorker.loop()
		}()
	}

	p.idx = newVectorIndex(p.cfg)
	if featuresOn {
		p.idx.state = vectorStateLoading
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.loadVector(rows)
		}()
	} else {
		p.idx.state = vectorStateDisabled
		p.idx.markReady()
		p.jobs.start()
	}
	p.started = true
	return nil
}

// shutdownEarly 释放 Start 早期失败时已创建的上下文。
func (p *Plugin) shutdownEarly() {
	p.stopping.Store(true)
	if p.cancel != nil {
		p.cancel()
	}
}

// Stop 按固定顺序关闭插件；重复调用幂等。
func (p *Plugin) Stop(ctx context.Context) error {
	p.stopOnce.Do(func() {
		p.stopErr = p.doStop(ctx)
	})
	return p.stopErr
}

func (p *Plugin) doStop(ctx context.Context) error {
	p.lifecycleMu.Lock()
	if !p.started {
		p.lifecycleMu.Unlock()
		p.stopping.Store(true)
		if p.cancel != nil {
			p.cancel()
		}
		return nil
	}
	p.started = false
	p.lifecycleMu.Unlock()

	p.stopping.Store(true)

	if p.writer != nil {
		p.writer.stopAccepting()
	}
	if p.jobs != nil {
		p.jobs.stop()
	}
	if p.writer != nil {
		p.writer.waitDrained()
	}
	if p.cancel != nil {
		p.cancel()
	}
	p.invalidateAllSessions()
	if p.idx != nil {
		p.idx.markReady()
	}

	p.bgMu.Lock()
	p.waitStarted = true
	p.bgMu.Unlock()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(shutdownWaitTimeout):
		p.log.Error("shutdown incomplete",
			"operation", "message", "reason", "shutdown_incomplete")
		return errors.New("chat-agent: 关闭不完整")
	}
}

// runBackground 在 wg 中执行后台函数并隔离 panic；Wait 开始后拒绝新任务。
func (p *Plugin) runBackground(fn func()) bool {
	p.bgMu.Lock()
	if p.waitStarted {
		p.bgMu.Unlock()
		return false
	}
	p.wg.Add(1)
	p.bgMu.Unlock()
	go func() {
		defer p.wg.Done()
		defer func() {
			if v := recover(); v != nil {
				p.log.Error("background panic",
					"operation", "message", "error", fmt.Sprint(v))
			}
		}()
		fn()
	}()
	return true
}
