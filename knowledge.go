package chatagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	knowledgeFetchTimeout    = 20 * time.Second
	knowledgeMaxRedirects    = 3
	knowledgeMaxChunksPerDoc = 500
	knowledgeUserAgent       = "kei-plugin-chat-agent/0.1"
	knowledgeMinChunkRunes   = 20
)

// knowledgeError 携带固定的失败 reason。
type knowledgeError struct {
	reason string
	err    error
}

func (e *knowledgeError) Error() string {
	if e.err == nil {
		return e.reason
	}
	return e.reason + ": " + e.err.Error()
}

func (e *knowledgeError) Unwrap() error { return e.err }

func kerr(reason string, err error) error {
	return &knowledgeError{reason: reason, err: err}
}

// knowledgeReason 提取固定失败 reason，缺省为 internal_error。
func knowledgeReason(err error) string {
	var ke *knowledgeError
	if errors.As(err, &ke) {
		return ke.reason
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "internal_error"
}

// fetchedPage 是抓取结果。
type fetchedPage struct {
	Body        []byte
	ContentType string
}

// knowledgeFetcher 是可注入的抓取接口。
type knowledgeFetcher interface {
	Fetch(ctx context.Context, rawURL string) (*fetchedPage, error)
}

// httpKnowledgeFetcher 是生产抓取实现。
type httpKnowledgeFetcher struct {
	client       *http.Client
	allowedTypes map[string]struct{}
	maxBytes     int
	allowPrivate bool
}

func newKnowledgeFetcher(cfg *config, client *http.Client) knowledgeFetcher {
	allowed := make(map[string]struct{}, len(cfg.KnowledgeAllowedTypes))
	for _, t := range cfg.KnowledgeAllowedTypes {
		allowed[t] = struct{}{}
	}
	return &httpKnowledgeFetcher{
		client:       client,
		allowedTypes: allowed,
		maxBytes:     cfg.KnowledgeMaxBytes,
		allowPrivate: cfg.KnowledgeURLAllowPrivate,
	}
}

func (f *httpKnowledgeFetcher) Fetch(ctx context.Context, rawURL string) (*fetchedPage, error) {
	timeout := knowledgeFetchTimeout
	if f.client != nil && f.client.Timeout > 0 && f.client.Timeout < timeout {
		timeout = f.client.Timeout
	}
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	current := strings.TrimSpace(rawURL)
	u, err := validateKnowledgeURL(fetchCtx, current, f.allowPrivate)
	if err != nil {
		return nil, err
	}
	client := *f.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	for redirects := 0; ; {
		req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, kerr("url_rejected", err)
		}
		req.Header.Set("User-Agent", knowledgeUserAgent)
		resp, err := client.Do(req)
		if err != nil {
			return nil, fetchContextError(fetchCtx, err)
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			_ = resp.Body.Close()
			if loc == "" {
				return nil, kerr("url_rejected", errors.New("redirect without location"))
			}
			next, err := u.Parse(loc)
			if err != nil {
				return nil, kerr("url_rejected", err)
			}
			redirects++
			if redirects > knowledgeMaxRedirects {
				return nil, kerr("url_rejected", errors.New("too many redirects"))
			}
			u, err = validateKnowledgeURL(fetchCtx, next.String(), f.allowPrivate)
			if err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, kerr("fetch_error", fmt.Errorf("status %d", resp.StatusCode))
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(f.maxBytes)+1))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, fetchContextError(fetchCtx, readErr)
		}
		if len(data) > f.maxBytes {
			return nil, kerr("too_large", nil)
		}
		ct := stripMimeParams(resp.Header.Get("Content-Type"))
		if ct == "" {
			return nil, kerr("unsupported_type", nil)
		}
		if _, ok := f.allowedTypes[ct]; !ok {
			return nil, kerr("unsupported_type", fmt.Errorf("content type %s", ct))
		}
		return &fetchedPage{Body: data, ContentType: ct}, nil
	}
}

func fetchContextError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return kerr("timeout", err)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return kerr("timeout", err)
	}
	return kerr("fetch_error", err)
}

// validateKnowledgeURL 校验 scheme、host、userinfo 与解析后的地址段。
func validateKnowledgeURL(ctx context.Context, raw string, allowPrivate bool) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return nil, kerr("url_rejected", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Host == "" {
		return nil, kerr("url_rejected", errors.New("invalid scheme or host"))
	}
	if u.User != nil {
		return nil, kerr("url_rejected", errors.New("userinfo not allowed"))
	}
	if allowPrivate {
		return u, nil
	}
	host := u.Hostname()
	if host == "" {
		return nil, kerr("url_rejected", errors.New("missing host"))
	}
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return nil, kerr("url_rejected", errors.New("blocked address"))
		}
		return u, nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, kerr("fetch_error", err)
	}
	if len(ips) == 0 {
		return nil, kerr("fetch_error", errors.New("no address"))
	}
	for _, ipAddr := range ips {
		if isBlockedIP(ipAddr.IP) {
			return nil, kerr("url_rejected", errors.New("blocked address"))
		}
	}
	return u, nil
}

// blockedNets 是文档列出的 IPv4/IPv6 拒绝段。
var blockedNets = func() []*net.IPNet {
	cidrs := []string{
		"0.0.0.0/8",
		"10.0.0.0/8",
		"100.64.0.0/10",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"172.16.0.0/12",
		"192.0.0.0/24",
		"192.0.2.0/24",
		"192.168.0.0/16",
		"198.18.0.0/15",
		"198.51.100.0/24",
		"203.0.113.0/24",
		"224.0.0.0/4",
		"240.0.0.0/4",
		"::/128",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
		"ff00::/8",
		"2001:db8::/32",
		"2001::/32",
		"2002::/16",
		"64:ff9b::/96",
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// isBlockedIP 判断 IP 是否属于拒绝段；IPv4-mapped IPv6 按 IPv4 再校验。
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsPrivate()
}

var (
	htmlCommentRE  = regexp.MustCompile(`(?s)<!--.*?-->`)
	htmlBlocksRE   = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>|<style\b[^>]*>.*?</style\s*>|<noscript\b[^>]*>.*?</noscript\s*>`)
	htmlTitleRE    = regexp.MustCompile(`(?is)<title\b[^>]*>(.*?)</title\s*>`)
	htmlLIRE       = regexp.MustCompile(`(?i)<li\b[^>]*>`)
	htmlBlockTagRE = regexp.MustCompile(`(?i)</?(?:p|div|br|tr|h[1-6]|section|article|header|footer|ul|ol|table|blockquote|pre|hr)\b[^>]*>`)
	htmlTagRE      = regexp.MustCompile(`<[^>]*>`)
)

// extractHTMLText 是启发式正文提取。
func extractHTMLText(raw string) string {
	s := raw
	s = htmlCommentRE.ReplaceAllString(s, "")
	s = htmlBlocksRE.ReplaceAllString(s, "")
	s = htmlLIRE.ReplaceAllString(s, "\n- ")
	s = htmlBlockTagRE.ReplaceAllString(s, "\n")
	s = htmlTagRE.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return normalizeBodyText(s)
}

// htmlTitle 提取 <title> 文本，实体解码并去空白。
func htmlTitle(raw string) string {
	m := htmlTitleRE.FindStringSubmatch(raw)
	if len(m) < 2 {
		return ""
	}
	t := htmlTagRE.ReplaceAllString(m[1], "")
	t = html.UnescapeString(t)
	return strings.TrimSpace(t)
}

// normalizeBodyText 统一换行、压缩空行并去首尾空白。
func normalizeBodyText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = multiNewline.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// titleFromURL 取 URL 最后一段路径，没有则用 host。
func titleFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	path := strings.TrimSuffix(u.Path, "/")
	if path != "" {
		if i := strings.LastIndex(path, "/"); i >= 0 {
			path = path[i+1:]
		}
		if path != "" {
			return path
		}
	}
	return u.Hostname()
}

// deriveKnowledgeTitle 按固定顺序推导标题并截断到 200 rune。
func deriveKnowledgeTitle(commandTitle, pageTitle, source, body string) string {
	title := strings.TrimSpace(commandTitle)
	if title == "" {
		title = strings.TrimSpace(pageTitle)
	}
	if title == "" && !strings.HasPrefix(source, "text:") {
		title = titleFromURL(source)
	}
	if title == "" {
		title = strings.TrimSpace(takePrefix(body, 40))
	}
	return takePrefix(title, 200)
}

// docIDFor 计算文档 ID。
func docIDFor(normalizedSource string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(normalizedSource)))
	return hex.EncodeToString(sum[:16])
}

// chunkIDFor 计算分块 ID。
func chunkIDFor(docID string, index int, text string) string {
	return hash16(docID, strconv.Itoa(index), text)
}

// contentSHA256 返回正文 UTF-8 字节的 SHA-256。
func contentSHA256(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// chunkText 按 rune 分块，返回去重后至少 20 rune 的块。
func chunkText(text string, size, overlap int) []string {
	r := []rune(text)
	if len(r) == 0 || size <= 0 {
		return nil
	}
	if overlap < 0 {
		overlap = 0
	}
	out := make([]string, 0)
	seen := make(map[string]struct{})
	for start := 0; start < len(r); {
		end := start + size
		if end > len(r) {
			end = len(r)
		}
		if end < len(r) {
			windowStart := end - size*30/100
			if windowStart < start {
				windowStart = start
			}
			cut := -1
			for i := end - 2; i >= windowStart; i-- {
				if r[i] == '\n' && r[i+1] == '\n' {
					cut = i
					break
				}
			}
			if cut >= 0 {
				end = cut
			} else {
				for i := end - 1; i >= windowStart; i-- {
					if isChunkBoundaryRune(r[i]) {
						end = i + 1
						break
					}
				}
			}
			if end <= start {
				end = start + size
				if end > len(r) {
					end = len(r)
				}
			}
		}
		chunk := strings.TrimSpace(string(r[start:end]))
		if runeLen(chunk) >= knowledgeMinChunkRunes {
			if _, ok := seen[chunk]; !ok {
				seen[chunk] = struct{}{}
				out = append(out, chunk)
			}
		}
		if end >= len(r) {
			break
		}
		next := end - overlap
		if next <= start {
			next = start + 1
		}
		start = next
	}
	return out
}

func isChunkBoundaryRune(r rune) bool {
	switch r {
	case '。', '！', '？', '!', '?', '.', ';', '；', '\n':
		return true
	default:
		return false
	}
}

// knowledgeResult 是一次知识库操作的结果。
type knowledgeResult struct {
	DocID  string
	Chunks int
	Result string
}

// importKnowledgeBody 建立或更新一篇文档。source 已规范化到去首尾空白。
func (p *Plugin) importKnowledgeBody(ctx context.Context, source, commandTitle string, rawBody []byte, contentType string) (knowledgeResult, error) {
	if !utf8Valid(rawBody) {
		p.log.Warn("knowledge body invalid utf-8", "operation", "knowledge")
	}
	rawText := strings.ToValidUTF8(string(rawBody), "�")
	isHTML := contentType == "text/html"
	pageTitle := ""
	var body string
	if isHTML {
		pageTitle = htmlTitle(rawText)
		body = extractHTMLText(rawText)
	} else {
		body = normalizeBodyText(rawText)
	}
	if body == "" {
		return knowledgeResult{}, kerr("empty_content", nil)
	}
	source = strings.TrimSpace(source)
	docID := docIDFor(source)
	hash := contentSHA256(body)
	title := deriveKnowledgeTitle(commandTitle, pageTitle, source, body)

	existing, exists := p.idx.docByID(docID)
	if exists && existing.ContentSHA256 == hash {
		if title != existing.Title {
			updated := *existing
			updated.Title = title
			updated.UpdatedAt = p.now().UTC()
			if err := p.putRowWait(ctx, docRowKey(docID), marshalJSON(docRecordOf(&updated))); err != nil {
				return knowledgeResult{}, kerr("storage_error", err)
			}
			p.idx.updateDocMeta(&updated)
		}
		return knowledgeResult{DocID: docID, Chunks: len(existing.ChunkIDs), Result: "unchanged"}, nil
	}

	chunks := chunkText(body, p.cfg.KnowledgeChunkChars, p.cfg.KnowledgeChunkOverlap)
	if len(chunks) == 0 {
		return knowledgeResult{}, kerr("empty_content", nil)
	}
	if len(chunks) > knowledgeMaxChunksPerDoc {
		return knowledgeResult{}, kerr("chunk_limit", nil)
	}

	oldIDs := []string(nil)
	oldVisible := 0
	if exists {
		oldIDs = existing.ChunkIDs
		oldVisible = p.visibleChunkCount(oldIDs)
	}
	if !p.idx.reserveKnowledgeSlots(oldVisible, len(chunks), p.cfg.VectorMaxRecords) {
		return knowledgeResult{}, kerr("vector_full", nil)
	}
	releaseReservation := true
	defer func() {
		if releaseReservation {
			p.idx.releaseSlots(len(chunks))
		}
	}()

	vecs, err := p.embedQueuedBatches(ctx, chunks)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return knowledgeResult{}, kerr("internal_error", err)
		}
		return knowledgeResult{}, kerr("embedding_error", err)
	}
	if len(vecs) != len(chunks) {
		return knowledgeResult{}, kerr("embedding_error", errors.New("embedding count mismatch"))
	}
	now := p.now().UTC()
	entries := make([]*chunkEntry, len(chunks))
	newIDs := make(map[string]struct{}, len(chunks))
	for i, text := range chunks {
		vec, err := normalizeVector(vecs[i])
		if err != nil {
			return knowledgeResult{}, kerr("embedding_error", err)
		}
		entry := &chunkEntry{
			ID:        chunkIDFor(docID, i, text),
			DocID:     docID,
			Index:     i,
			Text:      text,
			Vector:    vec,
			Dims:      len(vec),
			Model:     p.cfg.EmbeddingModel,
			CreatedAt: now,
		}
		entries[i] = entry
		newIDs[entry.ID] = struct{}{}
	}

	// 先写全部分块行，再写 d: 行提交。
	for _, entry := range entries {
		if err := p.putRowWait(ctx, chunkRowKey(entry.ID), marshalJSON(chunkRecordOf(entry))); err != nil {
			return knowledgeResult{}, kerr("storage_error", err)
		}
	}

	doc := &docEntry{
		ID:            docID,
		Source:        source,
		Title:         title,
		ContentSHA256: hash,
		ChunkIDs:      make([]string, len(entries)),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	for i, entry := range entries {
		doc.ChunkIDs[i] = entry.ID
	}
	if exists {
		doc.CreatedAt = existing.CreatedAt
	}
	if err := p.putRowWait(ctx, docRowKey(docID), marshalJSON(docRecordOf(doc))); err != nil {
		return knowledgeResult{}, kerr("storage_error", err)
	}
	p.idx.replaceDoc(doc, entries, oldIDs)
	p.idx.releaseSlots(len(chunks))
	releaseReservation = false

	for _, oldID := range oldIDs {
		if _, keep := newIDs[oldID]; keep {
			continue
		}
		if err := p.deleteRowWait(ctx, chunkRowKey(oldID)); err != nil {
			p.log.Warn("knowledge old chunk delete failed",
				"operation", "knowledge", "reason", "storage_error", "chunk", oldID, "error", err.Error())
		}
	}
	result := "added"
	if exists {
		result = "updated"
	}
	return knowledgeResult{DocID: docID, Chunks: len(entries), Result: result}, nil
}

// visibleChunkCount 统计给定 ID 中实际可见的分块数。
func (p *Plugin) visibleChunkCount(ids []string) int {
	p.idx.mu.RLock()
	defer p.idx.mu.RUnlock()
	n := 0
	for _, id := range ids {
		if _, ok := p.idx.chunks[id]; ok {
			n++
		}
	}
	return n
}

func utf8Valid(b []byte) bool {
	return utf8.Valid(b)
}

// addKnowledgeURL 抓取 URL 并建立或更新文档。
func (p *Plugin) addKnowledgeURL(ctx context.Context, rawURL, title string) (knowledgeResult, error) {
	page, err := p.fetcher.Fetch(ctx, rawURL)
	if err != nil {
		return knowledgeResult{}, err
	}
	return p.importKnowledgeBody(ctx, rawURL, title, page.Body, page.ContentType)
}

// addKnowledgeText 以内联文本建立或更新文档。
func (p *Plugin) addKnowledgeText(ctx context.Context, text string) (knowledgeResult, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return knowledgeResult{}, kerr("invalid_text", nil)
	}
	if len(trimmed) > p.cfg.KnowledgeMaxBytes {
		return knowledgeResult{}, kerr("too_large", nil)
	}
	return p.importKnowledgeBody(ctx, "text:"+trimmed, "", []byte(trimmed), "text/plain")
}

// deleteKnowledgeDoc 删除文档及其分块。
func (p *Plugin) deleteKnowledgeDoc(ctx context.Context, id string) (knowledgeResult, error) {
	doc, ok := p.idx.docByID(id)
	if !ok {
		return knowledgeResult{}, kerr("not_found", nil)
	}
	if err := p.deleteRowWait(ctx, docRowKey(id)); err != nil {
		return knowledgeResult{}, kerr("storage_error", err)
	}
	p.idx.removeDoc(id)
	for _, cid := range doc.ChunkIDs {
		if err := p.deleteRowWait(ctx, chunkRowKey(cid)); err != nil {
			p.log.Warn("knowledge chunk delete failed",
				"operation", "knowledge", "reason", "storage_error", "chunk", cid, "error", err.Error())
		}
	}
	return knowledgeResult{DocID: id, Chunks: len(doc.ChunkIDs), Result: "deleted"}, nil
}

// refreshKnowledgeDoc 重新抓取 URL 文档；text 文档直接返回 unchanged。
func (p *Plugin) refreshKnowledgeDoc(ctx context.Context, id string) (knowledgeResult, error) {
	doc, ok := p.idx.docByID(id)
	if !ok {
		return knowledgeResult{}, kerr("not_found", nil)
	}
	if strings.HasPrefix(doc.Source, "text:") {
		p.log.Debug("knowledge refresh skipped", "operation", "knowledge_refresh", "result", "skip", "doc", id)
		return knowledgeResult{DocID: id, Chunks: len(doc.ChunkIDs), Result: "unchanged"}, nil
	}
	page, err := p.fetcher.Fetch(ctx, doc.Source)
	if err != nil {
		return knowledgeResult{}, err
	}
	return p.importKnowledgeBody(ctx, doc.Source, "", page.Body, page.ContentType)
}
