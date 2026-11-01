package chatagent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// visionFetchTimeout 是单批图片下载的总超时。
	visionFetchTimeout = 10 * time.Second
	// visionMaxImagesPerRequest 是单次请求最多附加的图片数。
	visionMaxImagesPerRequest = 4
	// visionMaxImageBytes 是单图大小上限。
	visionMaxImageBytes = 4 << 20
	// visionMaxRedirects 是单图允许的重定向跳数。
	visionMaxRedirects = 3
)

var visionAllowedFormats = []string{"image/jpeg", "image/png", "image/gif", "image/webp"}

// newVisionClient 从宿主 transport 派生受限图片下载客户端，不修改宿主客户端。
func newVisionClient(host *http.Client) *http.Client {
	if host == nil {
		host = http.DefaultClient
	}
	clone := *host
	if host.Transport == nil {
		tr, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			tr = &http.Transport{}
		} else {
			tr = tr.Clone()
		}
		tr.DialContext = safeDialContext(tr.DialContext)
		clone.Transport = tr
	} else if tr, ok := host.Transport.(*http.Transport); ok {
		tr2 := tr.Clone()
		// 自定义 DialContext 视为测试注入，保留其行为（可绕过地址限制）。
		if tr.DialContext == nil {
			tr2.DialContext = safeDialContext(nil)
		}
		clone.Transport = tr2
	}
	return &clone
}

// safeDialContext 在拨号前解析并拒绝回环/私网/保留地址。
func safeDialContext(base func(ctx context.Context, network, addr string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	if base == nil {
		d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		base = d.DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no address for %s", host)
		}
		// 任一跳解析到禁止地址即整体拒绝，避免 DNS 重绑定。
		for _, ipAddr := range ips {
			if isBlockedIP(ipAddr.IP) {
				return nil, fmt.Errorf("blocked address")
			}
		}
		var lastErr error
		for _, ipAddr := range ips {
			conn, err := base(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		if lastErr == nil {
			lastErr = errors.New("dial failed")
		}
		return nil, lastErr
	}
}

// fetchImagesForChat 从最新到最旧选择并下载最多 4 张成功图片。
func (p *Plugin) fetchImagesForChat(ctx context.Context, turns []Turn) map[uint64][]*imageAttachment {
	out := make(map[uint64][]*imageAttachment)
	if !p.cfg.VisionEnabled || len(turns) == 0 {
		return out
	}
	fetchCtx, cancel := context.WithTimeout(ctx, visionFetchTimeout)
	defer cancel()
	success := 0
	for i := len(turns) - 1; i >= 0 && success < visionMaxImagesPerRequest; i-- {
		t := turns[i]
		if len(t.Images) == 0 {
			continue
		}
		aligned := out[t.Seq]
		if aligned == nil {
			aligned = make([]*imageAttachment, len(t.Images))
		}
		for j := len(t.Images) - 1; j >= 0 && success < visionMaxImagesPerRequest; j-- {
			att, reason := p.fetchImage(fetchCtx, t.Images[j])
			if att == nil {
				p.logVisionFailure(reason, t.Images[j], 0)
				continue
			}
			aligned[j] = att
			success++
			p.logVisionSuccess(t.Images[j], att.Bytes)
		}
		out[t.Seq] = aligned
	}
	return out
}

// fetchImagesForExtraction 按消息顺序下载一批图片，成功数最多 4。
func (p *Plugin) fetchImagesForExtraction(ctx context.Context, turns []Turn, max int) map[uint64][]*imageAttachment {
	out := make(map[uint64][]*imageAttachment)
	if !p.cfg.VisionEnabled || len(turns) == 0 {
		return out
	}
	fetchCtx, cancel := context.WithTimeout(ctx, visionFetchTimeout)
	defer cancel()
	success := 0
	for _, t := range turns {
		if len(t.Images) == 0 {
			continue
		}
		limit := len(t.Images)
		if limit > visionMaxImagesPerRequest {
			limit = visionMaxImagesPerRequest
			p.logVisionFailure("limit", t.Images[visionMaxImagesPerRequest], 0)
		}
		aligned := make([]*imageAttachment, len(t.Images))
		for j := 0; j < limit && success < max; j++ {
			att, reason := p.fetchImage(fetchCtx, t.Images[j])
			if att == nil {
				p.logVisionFailure(reason, t.Images[j], 0)
				continue
			}
			aligned[j] = att
			success++
			p.logVisionSuccess(t.Images[j], att.Bytes)
		}
		out[t.Seq] = aligned
	}
	return out
}

// fetchImage 下载并编码一张图片；失败返回 reason。
func (p *Plugin) fetchImage(ctx context.Context, rawURL string) (*imageAttachment, string) {
	if ctx.Err() != nil {
		return nil, "fetch_error"
	}
	current := strings.TrimSpace(rawURL)
	client := *p.visionClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	for redirects := 0; ; {
		u, err := parseRemoteURL(current)
		if err != nil {
			return nil, "bad_url"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, "bad_url"
		}
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, "fetch_error"
			}
			return nil, "fetch_error"
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			_ = resp.Body.Close()
			if loc == "" {
				return nil, "redirect"
			}
			next, err := u.Parse(loc)
			if err != nil {
				return nil, "redirect"
			}
			if _, err := parseRemoteURL(next.String()); err != nil {
				return nil, "redirect"
			}
			redirects++
			if redirects > visionMaxRedirects {
				return nil, "redirect"
			}
			current = next.String()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, "status"
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, visionMaxImageBytes+1))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, "fetch_error"
		}
		if len(data) > visionMaxImageBytes {
			return nil, "too_large"
		}
		mime, ok := detectImageMIME(resp.Header.Get("Content-Type"), data)
		if !ok {
			if strings.HasPrefix(strings.ToLower(stripMimeParams(resp.Header.Get("Content-Type"))), "image/") {
				return nil, "format"
			}
			return nil, "not_image"
		}
		return &imageAttachment{
			MIME:    mime,
			DataURL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data),
			Bytes:   len(data),
		}, ""
	}
}

// parseRemoteURL 校验图片 URL：仅 http(s)、带 host、无 userinfo。
func parseRemoteURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Host == "" {
		return nil, errors.New("invalid scheme or host")
	}
	if u.User != nil {
		return nil, errors.New("userinfo not allowed")
	}
	return u, nil
}

// detectImageMIME 优先使用响应 Content-Type，否则用内容嗅探；只接受白名单。
func detectImageMIME(contentType string, data []byte) (string, bool) {
	ct := stripMimeParams(contentType)
	if allowedImageMIME(ct) {
		return ct, true
	}
	detected := stripMimeParams(http.DetectContentType(data))
	if allowedImageMIME(detected) {
		return detected, true
	}
	return "", false
}

func stripMimeParams(s string) string {
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(strings.TrimSpace(s))
}

func allowedImageMIME(mime string) bool {
	for _, allowed := range visionAllowedFormats {
		if mime == allowed {
			return true
		}
	}
	return false
}

func (p *Plugin) logVisionFailure(reason, rawURL string, bytes int) {
	host := ""
	if u, err := url.Parse(rawURL); err == nil {
		host = u.Host
	}
	p.log.Warn("vision image skipped",
		"operation", "vision", "reason", reason, "host", host, "bytes", bytes)
}

func (p *Plugin) logVisionSuccess(rawURL string, bytes int) {
	if p.cfg.DebugPrompts {
		host := ""
		if u, err := url.Parse(rawURL); err == nil {
			host = u.Host
		}
		p.log.Debug("vision image fetched", "operation", "vision", "host", host, "bytes", bytes)
	}
}
