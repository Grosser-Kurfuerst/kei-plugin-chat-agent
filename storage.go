package chatagent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

const (
	// tableName 是向量索引、记忆与知识库共用的逻辑表名。
	tableName = "vector"
	// tableMetaKey 是索引元数据的固定行键。
	tableMetaKey = "meta"
	// storageTimeout 是单次 KV/表写入超时。
	storageTimeout = 2 * time.Second
	// shutdownWaitTimeout 是 Stop 等待后台协程的上限。
	shutdownWaitTimeout = 5 * time.Second

	keyPolicy  = "policy"
	keyAdmins  = "admins"
	keySession = "session:"

	rfc3339Nano = time.RFC3339Nano
)

// 测试可缩短的注入点；默认值与 docs/configuration.md §13.1 一致。
var (
	vectorLoadWorkers  = 4
	vectorReadyTimeout = 2 * time.Second
	tableLoadTimeout   = 30 * time.Second
)

// sessionStorageKey 返回会话覆盖的 KV 逻辑键。
func sessionStorageKey(platform, botID, channelKey string) string {
	sum := sha256.Sum256([]byte(platform + "\x00" + botID + "\x00" + channelKey))
	return keySession + hex.EncodeToString(sum[:])
}

// hash16 返回 SHA-256 前 16 字节的 32 位小写 hex。
func hash16(parts ...string) string {
	h := sha256.New()
	for i, part := range parts {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// normalizeText 去首尾空白并把内部连续空白压成单个空格。
func normalizeText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// encodeVector 把 float32 序列编码为 base64（little-endian）。
func encodeVector(v []float32) string {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// decodeVector 解码 base64 little-endian float32 序列。
func decodeVector(s string) ([]float32, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(raw)%4 != 0 {
		return nil, fmt.Errorf("vector byte length %d not multiple of 4", len(raw))
	}
	out := make([]float32, len(raw)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return out, nil
}

// normalizeVector 返回 L2 归一化副本；零向量返回错误。
func normalizeVector(v []float32) ([]float32, error) {
	var sum float64
	for _, f := range v {
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
			return nil, fmt.Errorf("invalid vector component")
		}
		sum += float64(f) * float64(f)
	}
	if sum == 0 {
		return nil, fmt.Errorf("zero vector")
	}
	norm := math.Sqrt(sum)
	out := make([]float32, len(v))
	for i, f := range v {
		out[i] = float32(float64(f) / norm)
	}
	return out, nil
}

// dot 计算点积；输入已归一化时等于余弦相似度。
func dot(a, b []float32) float64 {
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

func marshalJSON(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}

// writeKind 是写队列中的操作类型。
type writeKind int

const (
	writeKVSet writeKind = iota
	writeKVDelete
	writeRowPut
	writeRowDelete
)

// writeOp 是一次串行写操作。
type writeOp struct {
	kind  writeKind
	key   string
	value []byte
	ttl   time.Duration
	done  chan error
}

// writeQueue 是插件唯一的 KV 与表行写入协程。
type writeQueue struct {
	ctx     context.Context
	storage bot.Storage
	tables  bot.TableStorage
	log     *slog.Logger

	mu     sync.Mutex
	cond   *sync.Cond
	queue  []writeOp
	closed bool
	done   chan struct{}
}

func newWriteQueue(ctx context.Context, storage bot.Storage, tables bot.TableStorage, log *slog.Logger) *writeQueue {
	w := &writeQueue{
		ctx:     ctx,
		storage: storage,
		tables:  tables,
		log:     log,
		done:    make(chan struct{}),
	}
	w.cond = sync.NewCond(&w.mu)
	return w
}

func (w *writeQueue) start() {
	go w.loop()
}

func (w *writeQueue) loop() {
	defer close(w.done)
	for {
		w.mu.Lock()
		for len(w.queue) == 0 && !w.closed {
			w.cond.Wait()
		}
		if len(w.queue) == 0 && w.closed {
			w.mu.Unlock()
			return
		}
		op := w.queue[0]
		w.queue = w.queue[1:]
		w.mu.Unlock()

		err := w.exec(op)
		if err != nil {
			w.log.Warn("storage write failed",
				"operation", "storage", "key", op.key, "error", err.Error())
		}
		if op.done != nil {
			op.done <- err
			close(op.done)
		}
	}
}

func (w *writeQueue) exec(op writeOp) error {
	ctx, cancel := context.WithTimeout(w.ctx, storageTimeout)
	defer cancel()
	switch op.kind {
	case writeKVSet:
		if w.storage == nil {
			return errors.New("storage unavailable")
		}
		return w.storage.Set(ctx, op.key, op.value, op.ttl)
	case writeKVDelete:
		if w.storage == nil {
			return errors.New("storage unavailable")
		}
		return w.storage.Delete(ctx, op.key)
	case writeRowPut:
		return w.putRow(ctx, op.key, op.value)
	case writeRowDelete:
		return w.deleteRow(ctx, op.key)
	default:
		return errors.New("unknown write kind")
	}
}

// putRow 实现文档规定的行写入协议。
func (w *writeQueue) putRow(ctx context.Context, key string, value []byte) error {
	if w.tables == nil {
		return errors.New("table storage unavailable")
	}
	err := w.tables.UpdateRow(ctx, tableName, key, value)
	if err == nil {
		return nil
	}
	if errors.Is(err, bot.ErrNotFound) {
		err = w.tables.InsertRow(ctx, tableName, key, value)
		if err == nil {
			return nil
		}
		if errors.Is(err, bot.ErrRowExists) {
			err = w.tables.UpdateRow(ctx, tableName, key, value)
			if err == nil {
				return nil
			}
		}
		return err
	}
	if errors.Is(err, bot.ErrRowExists) {
		return w.tables.UpdateRow(ctx, tableName, key, value)
	}
	return err
}

// deleteRow 删除表行；行不存在按成功处理。
func (w *writeQueue) deleteRow(ctx context.Context, key string) error {
	if w.tables == nil {
		return errors.New("table storage unavailable")
	}
	err := w.tables.DeleteRow(ctx, tableName, key)
	if err == nil || errors.Is(err, bot.ErrNotFound) {
		return nil
	}
	return err
}

// stopAccepting 关闭写入准入，已入队操作仍会执行。
func (w *writeQueue) stopAccepting() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	w.cond.Broadcast()
}

// waitDrained 等待队列排空与写协程退出。
func (w *writeQueue) waitDrained() {
	<-w.done
}

// submit 入队；关闭后返回 false。
func (w *writeQueue) submit(op writeOp) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false
	}
	w.queue = append(w.queue, op)
	w.cond.Signal()
	return true
}

// submitWait 入队并等待结果；ctx 取消时不再等待（操作可能仍会执行）。
func (w *writeQueue) submitWait(ctx context.Context, op writeOp) error {
	done := make(chan error, 1)
	op.done = done
	if !w.submit(op) {
		return errors.New("storage closed")
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// kvSet 异步写入 KV。
func (p *Plugin) kvSet(key string, value []byte) {
	if p.writer == nil {
		return
	}
	if !p.writer.submit(writeOp{kind: writeKVSet, key: key, value: value}) {
		p.log.Warn("storage write dropped", "operation", "storage", "key", key)
	}
}

// kvDelete 异步删除 KV。
func (p *Plugin) kvDelete(key string) {
	if p.writer == nil {
		return
	}
	if !p.writer.submit(writeOp{kind: writeKVDelete, key: key}) {
		p.log.Warn("storage write dropped", "operation", "storage", "key", key)
	}
}

// putRowWait 写入表行并等待结果。
func (p *Plugin) putRowWait(ctx context.Context, key string, value []byte) error {
	if p.writer == nil {
		return errors.New("storage closed")
	}
	return p.writer.submitWait(ctx, writeOp{kind: writeRowPut, key: key, value: value})
}

// deleteRowWait 删除表行并等待结果。
func (p *Plugin) deleteRowWait(ctx context.Context, key string) error {
	if p.writer == nil {
		return errors.New("storage closed")
	}
	return p.writer.submitWait(ctx, writeOp{kind: writeRowDelete, key: key})
}

// readKey 在限定超时内读取 KV；键不存在返回 (nil, nil)。
func (p *Plugin) readKey(ctx context.Context, key string) ([]byte, error) {
	readCtx, cancel := context.WithTimeout(ctx, storageTimeout)
	defer cancel()
	val, err := p.kv.Get(readCtx, key)
	if errors.Is(err, bot.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return val, nil
}

// safeText 清洗注入块文本：尖括号换全角，控制字符换空格。
func safeText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '<':
			b.WriteRune('‹')
		case '>':
			b.WriteRune('›')
		default:
			if r < 0x20 || r == 0x7f {
				b.WriteRune(' ')
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// truncateRunes 按 rune 截断；超限时保留前 n 个 rune 并用 … 替换最后一个。
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}

// truncateEllipsis 截断到 max 并在末尾追加 …（结果最多 max 个 rune）。
func truncateEllipsis(s string, max int) string {
	return truncateRunes(s, max)
}

// takePrefix 返回前 n 个 rune。
func takePrefix(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// runeLen 返回 rune 数。
func runeLen(s string) int {
	return len([]rune(s))
}
