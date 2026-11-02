package chatagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// chatCompleter 是 Chat Completions 的注入接口。
type chatCompleter interface {
	Complete(ctx context.Context, req ChatRequest) (ChatResult, error)
}

// ChatRequest 是一次 Chat Completions 请求。
type ChatRequest struct {
	Model        string
	Messages     []ChatMessage
	Temperature  float64
	MaxTokens    int
	ExtraHeaders map[string]string
}

// ChatMessage 是一条 Chat 消息；Content 为 string 或 []ContentPart。
type ChatMessage struct {
	Role    string
	Content any
}

// ContentPart 是多模态 content 的一部分。
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL 是 image_url 内容块。
type ImageURL struct {
	URL string `json:"url"`
}

// ChatResult 是解析后的 Chat 响应。
type ChatResult struct {
	Content      string
	FinishReason string
}

// errorCategory 是 LLM/Embedding 错误的固定分类。
type errorCategory string

const (
	errTimeout      errorCategory = "timeout"
	errCanceled     errorCategory = "canceled"
	errHTTP429      errorCategory = "http_429"
	errHTTP4xx      errorCategory = "http_4xx"
	errHTTP5xx      errorCategory = "http_5xx"
	errTransport    errorCategory = "transport"
	errInvalidJSON  errorCategory = "invalid_json"
	errInvalidShape errorCategory = "invalid_shape"
	errInvalidVec   errorCategory = "invalid_vector"
)

// classifiedError 携带错误分类。
type classifiedError struct {
	category errorCategory
	err      error
}

func (e *classifiedError) Error() string {
	if e.err == nil {
		return string(e.category)
	}
	return string(e.category) + ": " + e.err.Error()
}

func (e *classifiedError) Unwrap() error { return e.err }

func classify(cat errorCategory, err error) error {
	return &classifiedError{category: cat, err: err}
}

// categoryOf 返回错误分类；未知错误按 internal 处理。
func categoryOf(err error) errorCategory {
	var ce *classifiedError
	if errors.As(err, &ce) {
		return ce.category
	}
	if errors.Is(err, context.Canceled) {
		return errCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errTimeout
	}
	return "internal"
}

// httpChatCompleter 是基于 net/http 的生产实现。
type httpChatCompleter struct {
	baseURL      string
	apiKey       string
	model        string
	timeout      time.Duration
	maxRetries   int
	extraHeaders map[string]string
	client       *http.Client
	log          *slog.Logger
	debug        bool
}

func newChatCompleter(cfg *config, client *http.Client, log *slog.Logger) chatCompleter {
	return &httpChatCompleter{
		baseURL:      strings.TrimRight(cfg.LLMBaseURL, "/"),
		apiKey:       cfg.LLMAPIKey,
		model:        cfg.LLMModel,
		timeout:      cfg.LLMTimeout,
		maxRetries:   cfg.LLMMaxRetries,
		extraHeaders: cfg.LLMExtraHeaders,
		client:       noRedirectClient(client),
		log:          log,
		debug:        cfg.DebugPrompts,
	}
}

// noRedirectClient 克隆 client 并禁止自动重定向，避免 3xx 跨域重放请求头。
func noRedirectClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &clone
}

type chatWireMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type chatWireRequest struct {
	Model       string            `json:"model"`
	Messages    []chatWireMessage `json:"messages"`
	Temperature float64           `json:"temperature"`
	MaxTokens   int               `json:"max_tokens"`
	Stream      bool              `json:"stream"`
}

type chatWireResponse struct {
	Choices []struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

func (c *httpChatCompleter) Complete(ctx context.Context, req ChatRequest) (ChatResult, error) {
	model := req.Model
	if model == "" {
		model = c.model
	}
	wire := chatWireRequest{
		Model:       model,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      false,
	}
	wire.Messages = make([]chatWireMessage, len(req.Messages))
	for i, m := range req.Messages {
		wire.Messages[i] = chatWireMessage(m)
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return ChatResult{}, classify(errInvalidShape, err)
	}
	endpoint := c.baseURL + "/chat/completions"
	headers := c.extraHeaders
	if len(req.ExtraHeaders) > 0 {
		headers = mergeHeaders(headers, req.ExtraHeaders)
	}
	if c.debug {
		c.log.Debug("chat request",
			"operation", "generate",
			"url", logURL(endpoint),
			"model", model,
			"messages", debugMessages(wire.Messages),
		)
	}
	respBody, err := httpPost(ctx, c.client, endpoint, c.apiKey, body, headers, c.timeout, c.maxRetries, c.log, "generate")
	if err != nil {
		return ChatResult{}, err
	}
	var parsed chatWireResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return ChatResult{}, classify(errInvalidJSON, err)
	}
	if len(parsed.Choices) == 0 {
		return ChatResult{}, classify(errInvalidShape, errors.New("choices is empty"))
	}
	raw := bytes.TrimSpace(parsed.Choices[0].Message.Content)
	if len(raw) == 0 || raw[0] != '"' {
		return ChatResult{}, classify(errInvalidShape, errors.New("message content is not a string"))
	}
	var content string
	if err := json.Unmarshal(raw, &content); err != nil {
		return ChatResult{}, classify(errInvalidShape, err)
	}
	if content == "" {
		return ChatResult{}, classify(errInvalidShape, errors.New("message content is empty"))
	}
	if c.debug {
		c.log.Debug("chat response",
			"operation", "generate",
			"finish_reason", parsed.Choices[0].FinishReason,
			"content", content,
		)
	} else if parsed.Choices[0].FinishReason != "" {
		c.log.Debug("chat response",
			"operation", "generate", "finish_reason", parsed.Choices[0].FinishReason)
	}
	return ChatResult{Content: content, FinishReason: parsed.Choices[0].FinishReason}, nil
}

// httpPost 发送 JSON POST 并按配置重试；所有请求共享同一套超时与分类语义。
func httpPost(ctx context.Context, client *http.Client, endpoint, apiKey string, body []byte, headers map[string]string, timeout time.Duration, maxRetries int, log *slog.Logger, operation string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil, classify(errCanceled, err)
			}
			return nil, classify(errTimeout, err)
		}
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		data, err := httpPostOnce(attemptCtx, client, endpoint, apiKey, body, headers, log, operation)
		cancel()
		if err == nil {
			return data, nil
		}
		cat := categoryOf(err)
		if !retryableCategory(cat) || attempt >= maxRetries {
			return nil, err
		}
		if err := sleepBackoff(ctx, attempt); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil, classify(errCanceled, err)
			}
			return nil, classify(errTimeout, err)
		}
	}
}

func httpPostOnce(ctx context.Context, client *http.Client, endpoint, apiKey string, body []byte, headers map[string]string, log *slog.Logger, operation string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, classify(errTransport, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, classifyNetworkError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	limit := int64(8 << 20)
	if resp.StatusCode != http.StatusOK {
		limit = 4096
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, limit))
	log.Debug("http request",
		"operation", operation,
		"method", http.MethodPost,
		"url", logURL(endpoint),
		"status", resp.StatusCode,
		"duration_ms", time.Since(start).Milliseconds(),
	)
	if readErr != nil {
		return nil, classify(errTransport, readErr)
	}
	if resp.StatusCode == http.StatusOK {
		return data, nil
	}
	errBody := truncateRunes(string(data), 500)
	return nil, classify(statusCategory(resp.StatusCode), fmt.Errorf("status %d: %s", resp.StatusCode, sanitizeLogText(errBody)))
}

func mergeHeaders(base, override map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

func retryableCategory(cat errorCategory) bool {
	switch cat {
	case errHTTP429, errHTTP5xx, errTransport, errTimeout:
		return true
	default:
		return false
	}
}

func statusCategory(status int) errorCategory {
	switch {
	case status == http.StatusTooManyRequests:
		return errHTTP429
	case status >= 500:
		return errHTTP5xx
	case status >= 400:
		return errHTTP4xx
	default:
		return errHTTP4xx
	}
}

func classifyNetworkError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return classify(errCanceled, ctx.Err())
		}
		return classify(errTimeout, ctx.Err())
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Timeout() {
		return classify(errTimeout, err)
	}
	return classify(errTransport, err)
}

func sleepBackoff(ctx context.Context, attempt int) error {
	delay := 30 * time.Second
	switch attempt {
	case 0:
		delay = time.Second
	case 1:
		delay = 2 * time.Second
	case 2:
		delay = 4 * time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func logURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host + u.Path
}

func debugMessages(msgs []chatWireMessage) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		switch content := m.Content.(type) {
		case string:
			out = append(out, m.Role+": "+content)
		case []ContentPart:
			parts := make([]string, 0, len(content))
			for _, p := range content {
				if p.Type == "image_url" && p.ImageURL != nil {
					parts = append(parts, imageDebugSummary(p.ImageURL.URL))
					continue
				}
				parts = append(parts, p.Text)
			}
			out = append(out, m.Role+": "+strings.Join(parts, "\n"))
		default:
			out = append(out, fmt.Sprint(content))
		}
	}
	return out
}

// imageDebugSummary 把 data URL 折叠为 [image <mime> <bytes>B]。
func imageDebugSummary(dataURL string) string {
	const prefix = "data:"
	if !strings.HasPrefix(dataURL, prefix) {
		return "[image]"
	}
	rest := strings.TrimPrefix(dataURL, prefix)
	semi := strings.IndexByte(rest, ';')
	if semi < 0 {
		return "[image]"
	}
	mime := rest[:semi]
	b64 := rest[semi+1:]
	if i := strings.IndexByte(b64, ','); i >= 0 {
		b64 = b64[i+1:]
	}
	bytesLen := len(b64) / 4 * 3
	return "[image " + mime + " " + strconv.Itoa(bytesLen) + "B]"
}

func sanitizeLogText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			b.WriteRune(' ')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// finiteFloat32 把 float64 转为 float32 并拒绝 NaN/Inf/溢出。
func finiteFloat32(v float64) (float32, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v > math.MaxFloat32 || v < -math.MaxFloat32 {
		return 0, false
	}
	return float32(v), true
}
