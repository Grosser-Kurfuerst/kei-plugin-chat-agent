package chatagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	// embeddingBatchSize 是单次 Embeddings 请求的最大输入条数。
	embeddingBatchSize = 16
	// embeddingMaxInputChars 是单条嵌入输入的 rune 上限。
	embeddingMaxInputChars = 6000
)

// embedder 是 Embeddings 的注入接口；返回顺序与 inputs 一致。
type embedder interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
}

// httpEmbedder 是 OpenAI 兼容 /embeddings 的生产实现。
type httpEmbedder struct {
	baseURL      string
	apiKey       string
	model        string
	dimensions   int
	timeout      time.Duration
	maxRetries   int
	extraHeaders map[string]string
	client       *http.Client
	log          *slog.Logger
	debug        bool
}

func newHTTPEmbedder(cfg *config, client *http.Client, log *slog.Logger) embedder {
	return &httpEmbedder{
		baseURL:      strings.TrimRight(cfg.effectiveEmbeddingURL(), "/"),
		apiKey:       cfg.effectiveEmbeddingKey(),
		model:        cfg.EmbeddingModel,
		dimensions:   cfg.EmbeddingDimensions,
		timeout:      cfg.EmbeddingTimeout,
		maxRetries:   cfg.EmbeddingMaxRetries,
		extraHeaders: cfg.EmbeddingExtraHeaders,
		client:       noRedirectClient(client),
		log:          log,
		debug:        cfg.DebugPrompts,
	}
}

type embedWireRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

type embedWireResponse struct {
	Data []json.RawMessage `json:"data"`
}

type embedWireItem struct {
	Index     *int            `json:"index"`
	Embedding json.RawMessage `json:"embedding"`
}

func (e *httpEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return [][]float32{}, nil
	}
	truncated := make([]string, len(inputs))
	for i, s := range inputs {
		truncated[i] = takePrefix(s, embeddingMaxInputChars)
	}
	wire := embedWireRequest{Model: e.model, Input: truncated, Dimensions: e.dimensions}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, classify(errInvalidShape, err)
	}
	endpoint := e.baseURL + "/embeddings"
	if e.debug {
		e.log.Debug("embedding request",
			"operation", "vector", "url", logURL(endpoint), "model", e.model, "input_count", len(truncated))
	}
	respBody, err := httpPost(ctx, e.client, endpoint, e.apiKey, body, e.extraHeaders, e.timeout, e.maxRetries, e.log, "vector")
	if err != nil {
		return nil, err
	}
	var parsed embedWireResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, classify(errInvalidJSON, err)
	}
	if len(parsed.Data) != len(inputs) {
		return nil, classify(errInvalidShape, fmt.Errorf("embedding count %d != %d", len(parsed.Data), len(inputs)))
	}
	type item struct {
		index int
		vec   []float64
	}
	items := make([]item, 0, len(parsed.Data))
	for _, raw := range parsed.Data {
		var wire embedWireItem
		if err := json.Unmarshal(raw, &wire); err != nil {
			return nil, classify(errInvalidShape, err)
		}
		if wire.Index == nil {
			return nil, classify(errInvalidShape, errors.New("embedding index missing"))
		}
		if len(wire.Embedding) == 0 || string(wire.Embedding) == "null" {
			return nil, classify(errInvalidShape, errors.New("embedding missing"))
		}
		var vec []float64
		if err := json.Unmarshal(wire.Embedding, &vec); err != nil {
			return nil, classify(errInvalidShape, err)
		}
		items = append(items, item{index: *wire.Index, vec: vec})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].index < items[j].index })
	for i, it := range items {
		if it.index != i {
			return nil, classify(errInvalidShape, fmt.Errorf("embedding index %d at position %d", it.index, i))
		}
	}
	out := make([][]float32, len(items))
	expectedDims := 0
	if e.dimensions > 0 {
		expectedDims = e.dimensions
	}
	for i, it := range items {
		if len(it.vec) == 0 {
			return nil, classify(errInvalidVec, errors.New("empty embedding"))
		}
		if expectedDims == 0 {
			expectedDims = len(it.vec)
		} else if len(it.vec) != expectedDims {
			return nil, classify(errInvalidShape, fmt.Errorf("embedding dimension %d != %d", len(it.vec), expectedDims))
		}
		vec := make([]float32, len(it.vec))
		var norm float64
		for j, f := range it.vec {
			f32, ok := finiteFloat32(f)
			if !ok {
				return nil, classify(errInvalidVec, errors.New("embedding contains NaN/Inf"))
			}
			vec[j] = f32
			norm += f * f
		}
		if norm == 0 || math.IsNaN(norm) {
			return nil, classify(errInvalidVec, errors.New("zero embedding"))
		}
		out[i] = vec
	}
	return out, nil
}

// embedJob 是后台 Embedding 队列中的一次请求。
type embedJob struct {
	ctx    context.Context
	texts  []string
	result chan embedResult
}

type embedResult struct {
	vecs [][]float32
	err  error
}

// embedQueue 是唯一的后台 Embedding 工作协程。
type embedQueue struct {
	ctx      context.Context
	embedder embedder
	log      *slog.Logger
	jobs     chan embedJob
}

func newEmbedQueue(ctx context.Context, e embedder, log *slog.Logger) *embedQueue {
	return &embedQueue{
		ctx:      ctx,
		embedder: e,
		log:      log,
		jobs:     make(chan embedJob, 64),
	}
}

func (q *embedQueue) loop() {
	for {
		select {
		case <-q.ctx.Done():
			for {
				select {
				case job := <-q.jobs:
					job.result <- embedResult{err: q.ctx.Err()}
				default:
					return
				}
			}
		case job := <-q.jobs:
			if err := job.ctx.Err(); err != nil {
				job.result <- embedResult{err: err}
				continue
			}
			vecs, err := q.embedder.Embed(job.ctx, job.texts)
			if err != nil {
				q.log.Warn("embedding failed", "operation", "vector", "error", err.Error())
			}
			job.result <- embedResult{vecs: vecs, err: err}
		}
	}
}

// embedQueued 提交到后台队列并等待结果。
func (p *Plugin) embedQueued(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	if p.embedWorker == nil {
		return nil, classify(errTransport, errors.New("embedding worker unavailable"))
	}
	job := embedJob{ctx: ctx, texts: texts, result: make(chan embedResult, 1)}
	select {
	case p.embedWorker.jobs <- job:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.ctx.Done():
		return nil, p.ctx.Err()
	default:
		return nil, classify(errTransport, errors.New("embedding queue full"))
	}
	select {
	case res := <-job.result:
		return res.vecs, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.ctx.Done():
		return nil, p.ctx.Err()
	}
}

// embedQueuedBatches 按 embedding_batch_size 分批提交后台 Embedding 队列。
func (p *Plugin) embedQueuedBatches(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += embeddingBatchSize {
		end := start + embeddingBatchSize
		if end > len(texts) {
			end = len(texts)
		}
		vecs, err := p.embedQueued(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		if len(vecs) != end-start {
			return nil, classify(errInvalidShape, errors.New("embedder returned wrong batch size"))
		}
		out = append(out, vecs...)
	}
	return out, nil
}
