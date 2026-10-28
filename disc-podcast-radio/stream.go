package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const maxPlaylistBytes = 4 << 20

// --- HTTP clients ---

func newHTTPClients() (*http.Client, *http.Client) {
	base := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialIPv4First,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   12 * time.Second,
		ResponseHeaderTimeout: 18 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS10},
	}
	insecureTransport := base.Clone()
	insecureTransport.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true, // fallback for players/streams with old or invalid certs
		MinVersion:         tls.VersionTLS10,
	}
	return &http.Client{Transport: base}, &http.Client{Transport: insecureTransport}
}

func isTLSError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "x509") || strings.Contains(s, "certificate") ||
		strings.Contains(s, "tls:") || strings.Contains(s, "handshake") ||
		strings.Contains(s, "server name")
}

func (a *App) doWithFallback(req *http.Request) (*http.Response, error) {
	resp, err := a.client.Do(req)
	if err == nil {
		return resp, nil
	}
	if req.Context().Err() != nil || !isTLSError(err) {
		return nil, err
	}
	a.log.Warnf("TLS 校验失败，用兼容模式重试: %s (%v)", req.URL.Redacted(), err)
	req2 := req.Clone(req.Context())
	if req.GetBody != nil {
		if body, berr := req.GetBody(); berr == nil {
			req2.Body = body
		}
	}
	return a.insecureClient.Do(req2)
}

func (a *App) fetchBytes(ctx context.Context, rawURL string, max int64) ([]byte, string, error) {
	if err := validateHTTPURL(rawURL); err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	resp, err := a.doWithFallback(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", httpStatusError(resp)
	}
	ct := resp.Header.Get("Content-Type")
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, ct, err
	}
	if int64(len(data)) > max {
		return nil, ct, fmt.Errorf("响应超过 %d 字节限制", max)
	}
	return data, ct, nil
}

func httpStatusError(resp *http.Response) error {
	excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	msg := fmt.Sprintf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	if s := strings.TrimSpace(string(excerpt)); s != "" {
		s = strings.Join(strings.Fields(s), " ")
		if len(s) > 180 {
			s = s[:180] + "…"
		}
		msg += ": " + s
	}
	return errors.New(msg)
}

func validateHTTPURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("音频地址为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("音频地址格式错误: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("仅支持 http/https 地址，当前为 %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("音频地址缺少主机名")
	}
	return nil
}

func (a *App) openUpstream(ctx context.Context, rawURL, rangeHeader string) (*http.Response, error) {
	if err := validateHTTPURL(rawURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Icy-MetaData", "0")
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, err := a.doWithFallback(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := httpStatusError(resp)
		resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

// --- Media token registry ---

func (a *App) registerMedia(kind, id, rawURL, title, subtitle, cover, ext, contentType, format string) *MediaEntry {
	if id == "" {
		id = rawURL
	}
	token := stableHash(kind+"|"+id, 16)
	ext = normalizeExt(ext)
	if ext == "" {
		ext = "mp3"
	}
	entry := &MediaEntry{
		Token:       token,
		Kind:        kind,
		ID:          id,
		URL:         rawURL,
		Title:       title,
		Subtitle:    subtitle,
		Cover:       cover,
		Ext:         ext,
		ContentType: normalizeContentType(contentType),
		Format:      format,
		Created:     time.Now().Unix(),
	}
	a.mu.Lock()
	if old, ok := a.media[token]; ok {
		if old.Created > 0 {
			entry.Created = old.Created
		}
		entry.PlaceholderPath = old.PlaceholderPath
		entry.ResolvedURL = old.ResolvedURL
	}
	a.media[token] = entry
	a.mu.Unlock()
	return entry
}

func (a *App) lookupMedia(token string) *MediaEntry {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if m, ok := a.media[token]; ok {
		c := *m
		return &c
	}
	return nil
}

func (a *App) mediaProxyURL(entry *MediaEntry) string {
	if entry == nil {
		return ""
	}
	ext := normalizeExt(entry.Ext)
	if ext == "" {
		ext = "mp3"
	}
	return fmt.Sprintf("http://127.0.0.1:%d/audio/%s.%s", a.port, entry.Token, ext)
}

func normalizeExt(ext string) string {
	ext = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
	switch ext {
	case "mp3", "mp2", "mpeg":
		return "mp3"
	case "aac", "adts", "aacp":
		return "aac"
	case "m4a", "m4b", "mp4":
		return "m4a"
	case "flac":
		return "flac"
	case "ogg", "oga", "opus":
		return "ogg"
	case "wav":
		return "wav"
	case "ape":
		return "ape"
	case "wma":
		return "wma"
	case "m3u8":
		return "aac"
	case "ts":
		return "aac"
	default:
		return ext
	}
}

func extFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(u.Path)), ".")
	if ext == "" {
		return ""
	}
	return normalizeExt(ext)
}

func extFromContentType(ct string) string {
	switch normalizeContentType(ct) {
	case "audio/mpeg":
		return "mp3"
	case "audio/aac":
		return "aac"
	case "audio/mp4":
		return "m4a"
	case "audio/flac":
		return "flac"
	case "audio/ogg":
		return "ogg"
	case "audio/wav":
		return "wav"
	case "audio/ape":
		return "ape"
	case "audio/x-ms-wma":
		return "wma"
	}
	return ""
}

func normalizeContentType(ct string) string {
	if ct == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		mt = strings.TrimSpace(strings.Split(ct, ";")[0])
	}
	switch strings.ToLower(mt) {
	case "audio/mpeg", "audio/mp3", "audio/x-mp3", "audio/mpeg3", "audio/x-mpeg":
		return "audio/mpeg"
	case "audio/aac", "audio/aacp", "audio/x-aac", "audio/x-aacp", "audio/lc-aac", "audio/he-aac v1", "audio/he-aac v2":
		return "audio/aac"
	case "audio/mp4", "audio/m4a", "audio/x-m4a", "video/mp4":
		return "audio/mp4"
	case "audio/ogg", "application/ogg", "audio/x-ogg", "audio/opus":
		return "audio/ogg"
	case "audio/flac", "audio/x-flac":
		return "audio/flac"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "audio/wav"
	case "audio/ape", "audio/x-ape":
		return "audio/ape"
	case "audio/x-ms-wma", "audio/wma":
		return "audio/x-ms-wma"
	case "video/mp2t", "video/mpegts", "application/mp2t":
		return "video/mp2t"
	case "application/vnd.apple.mpegurl", "application/x-mpegurl", "audio/mpegurl", "audio/x-mpegurl":
		return "application/vnd.apple.mpegurl"
	case "audio/x-scpls", "application/pls+xml":
		return "audio/x-scpls"
	case "text/html", "application/xhtml+xml":
		return "text/html"
	case "application/octet-stream":
		return "application/octet-stream"
	default:
		return strings.ToLower(mt)
	}
}

func mimeForExt(ext string) string {
	switch normalizeExt(ext) {
	case "mp3":
		return "audio/mpeg"
	case "aac":
		return "audio/aac"
	case "m4a":
		return "audio/mp4"
	case "flac":
		return "audio/flac"
	case "ogg":
		return "audio/ogg"
	case "wav":
		return "audio/wav"
	case "ape":
		return "audio/ape"
	case "wma":
		return "audio/x-ms-wma"
	default:
		return "audio/mpeg"
	}
}

func protocolInfoFor(ct string) string {
	ct = normalizeContentType(ct)
	if ct == "" || ct == "application/octet-stream" || ct == "video/mp2t" ||
		ct == "application/vnd.apple.mpegurl" || ct == "text/html" {
		ct = "audio/mpeg"
	}
	return "http-get:*:" + ct + ":*"
}

func albumForType(mediaType string) string {
	if mediaType == "podcast" {
		return "DISC 播客"
	}
	return "DISC 网络电台"
}

func didlMetadata(title, artist, album, cover, mediaURL, contentType string) string {
	if title == "" {
		title = "DISC 流媒体"
	}
	mime := "audio/mpeg"
	if contentType != "" && contentType != "application/octet-stream" {
		mime = contentType
	}
	var b strings.Builder
	b.WriteString(`<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`)
	b.WriteString(`<item id="online" parentID="0" restricted="1">`)
	b.WriteString("<dc:title>" + xmlEscape(title) + "</dc:title>")
	if artist != "" {
		b.WriteString("<upnp:artist>" + xmlEscape(artist) + "</upnp:artist>")
	}
	if album != "" {
		b.WriteString("<upnp:album>" + xmlEscape(album) + "</upnp:album>")
	}
	b.WriteString("<upnp:class>object.item.audioItem.musicTrack</upnp:class>")
	if cover != "" && (strings.HasPrefix(cover, "http://") || strings.HasPrefix(cover, "https://")) {
		b.WriteString("<upnp:albumArtURI>" + xmlEscape(cover) + "</upnp:albumArtURI>")
	}
	b.WriteString(`<res protocolInfo="http-get:*:` + mime + `:*" duration="0:00:00">` + xmlEscape(mediaURL) + `</res>`)
	b.WriteString(`</item></DIDL-Lite>`)
	return b.String()
}

// --- Probing ---

type ProbeResult struct {
	URL         string `json:"url"`
	FinalURL    string `json:"final_url"`
	ContentType string `json:"content_type"`
	Ext         string `json:"ext"`
	Format      string `json:"format"`
	Status      int    `json:"status"`
	Bytes       int64  `json:"bytes"`
	ElapsedMS   int64  `json:"elapsed_ms"`
	Error       string `json:"error,omitempty"`
}

func (a *App) probeStream(ctx context.Context, rawURL string) (*ProbeResult, error) {
	start := time.Now()
	if err := validateHTTPURL(rawURL); err != nil {
		return nil, err
	}
	res := &ProbeResult{URL: rawURL}
	var err error
	if looksLikeHLSURL(rawURL) {
		err = a.probeHLSURL(ctx, rawURL, res, 0)
	} else {
		err = a.probeDirect(ctx, rawURL, res, 0)
	}
	res.ElapsedMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = err.Error()
		a.recordProbe(res)
		return nil, err
	}
	if res.Ext == "" {
		res.Ext = extFromContentType(res.ContentType)
	}
	if res.Ext == "" {
		res.Ext = "mp3"
	}
	res.ContentType = normalizeContentType(res.ContentType)
	if res.ContentType == "" || res.ContentType == "application/octet-stream" {
		res.ContentType = mimeForExt(res.Ext)
	}
	a.recordProbe(res)
	return res, nil
}

func (a *App) recordProbe(res *ProbeResult) {
	msg := fmt.Sprintf("%s type=%s ext=%s format=%s bytes=%d elapsed=%dms", res.URL, res.ContentType, res.Ext, res.Format, res.Bytes, res.ElapsedMS)
	if res.Error != "" {
		msg += " error=" + res.Error
	}
	a.mu.Lock()
	a.lastProbe = msg
	a.mu.Unlock()
	if res.Error != "" {
		a.log.Warnf("探测音频失败: %s", msg)
	} else {
		a.log.Infof("探测音频成功: %s", msg)
	}
}

func (a *App) probeDirect(ctx context.Context, rawURL string, res *ProbeResult, depth int) error {
	if depth > 3 {
		return errors.New("播放列表嵌套层级过深")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Icy-MetaData", "0")
	req.Header.Set("Range", "bytes=0-8191")
	resp, err := a.doWithFallback(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpStatusError(resp)
	}
	res.Status = resp.StatusCode
	res.FinalURL = resp.Request.URL.String()
	ct := normalizeContentType(resp.Header.Get("Content-Type"))
	br := bufio.NewReaderSize(resp.Body, 16<<10)
	peek, _ := br.Peek(8192)
	if isPlaylistBody(peek, ct) {
		data := make([]byte, 0, 8192)
		tmp := make([]byte, 8192)
		for len(data) < 512<<10 {
			n, er := br.Read(tmp)
			if n > 0 {
				data = append(data, tmp[:n]...)
			}
			if er != nil {
				break
			}
			if n == 0 {
				break
			}
		}
		res.Bytes = int64(len(data))
		if bytes.Contains(data, []byte("#EXT-X-")) {
			return a.probeHLSURL(ctx, res.FinalURL, res, depth+1)
		}
		next := firstPlaylistURL(string(data), res.FinalURL)
		if next == "" {
			return errors.New("播放列表中没有可用的音频地址")
		}
		return a.probeStreamInto(ctx, next, res, depth+1)
	}

	if ct == "" || ct == "application/octet-stream" {
		ct = sniffContentType(peek)
	}
	if ct == "text/html" {
		return errors.New("地址返回了网页而不是音频")
	}
	if ct == "video/mp2t" {
		d := &tsDemux{}
		var buf bytes.Buffer
		_, _ = d.feed(peek, &buf)
		res.Format = "ts"
		if d.audioFormat == "mp3" {
			res.Ext = "mp3"
			res.ContentType = "audio/mpeg"
		} else {
			res.Ext = "aac"
			res.ContentType = "audio/aac"
		}
		res.Bytes = int64(len(peek))
		return nil
	}
	if ct == "" {
		ct = mimeForExt(extFromURL(rawURL))
	}
	res.ContentType = ct
	res.Ext = extFromContentType(ct)
	if res.Ext == "" {
		res.Ext = extFromURL(rawURL)
	}
	if res.Ext == "" {
		res.Ext = "mp3"
	}
	res.Format = "direct"
	res.Bytes = int64(len(peek))
	return nil
}

func (a *App) probeStreamInto(ctx context.Context, rawURL string, res *ProbeResult, depth int) error {
	if looksLikeHLSURL(rawURL) {
		return a.probeHLSURL(ctx, rawURL, res, depth)
	}
	return a.probeDirect(ctx, rawURL, res, depth)
}

func (a *App) probeHLSURL(ctx context.Context, playlistURL string, res *ProbeResult, depth int) error {
	if depth > 4 {
		return errors.New("HLS 播放列表嵌套层级过深")
	}
	pl, err := a.loadMediaPlaylist(ctx, playlistURL, depth)
	if err != nil {
		return err
	}
	res.FinalURL = pl.URL
	res.Format = "hls"
	res.Status = 200

	start := 0
	if !pl.EndList && len(pl.Segments) > 1 {
		start = len(pl.Segments) - 1
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		idx := start - attempt
		if idx < 0 || idx >= len(pl.Segments) {
			continue
		}
		seg := pl.Segments[idx]
		data, _, err := a.fetchHLSSegment(ctx, seg)
		if err != nil {
			lastErr = err
			continue
		}
		res.Bytes = int64(len(data))
		format := detectSegmentFormat(data)
		switch format {
		case "ts":
			d := &tsDemux{}
			var buf bytes.Buffer
			_, _ = d.feed(data, &buf)
			if d.audioFormat == "mp3" {
				res.Ext = "mp3"
				res.ContentType = "audio/mpeg"
			} else {
				res.Ext = "aac"
				res.ContentType = "audio/aac"
			}
		case "mp4":
			res.Ext = "m4a"
			res.ContentType = "audio/mp4"
		case "mp3":
			res.Ext = "mp3"
			res.ContentType = "audio/mpeg"
		case "aac":
			res.Ext = "aac"
			res.ContentType = "audio/aac"
		default:
			res.Ext = "aac"
			res.ContentType = "audio/aac"
		}
		return nil
	}
	if lastErr != nil {
		return fmt.Errorf("HLS 分片下载失败: %w", lastErr)
	}
	return errors.New("HLS 播放列表没有可播放分片")
}

// --- Playback ---

func (a *App) PlayStream(ctx context.Context, streamURL, title, subtitle, cover, mediaType string) error {
	if err := validateHTTPURL(streamURL); err != nil {
		a.setStatusError(err.Error())
		return err
	}
	if title == "" {
		title = "DISC 流媒体"
	}
	if mediaType == "" {
		mediaType = "radio"
	}
	album := "DISC 网络电台"
	if mediaType == "podcast" {
		album = "DISC 播客"
	}

	// Transparently resolve RSS feed URLs to the latest playable audio stream
	if strings.HasSuffix(strings.ToLower(streamURL), ".xml") || strings.Contains(streamURL, "feed") || mediaType == "podcast" {
		isFeedURL := strings.HasSuffix(strings.ToLower(streamURL), ".xml")
		if !isFeedURL {
			a.mu.RLock()
			for _, pf := range a.podcasts {
				if pf.URL == streamURL {
					isFeedURL = true
					break
				}
			}
			a.mu.RUnlock()
		}
		if isFeedURL {
			a.log.Infof("检测到播客 RSS 订阅链接: %s，正在自动获取最新单集音频…", streamURL)
			feed, ferr := a.FetchPodcastEpisodes(ctx, streamURL, false)
			if ferr == nil && feed != nil && len(feed.Episodes) > 0 {
				a.log.Infof("成功获取单集: %s (%s)", feed.Episodes[0].Title, feed.Episodes[0].AudioURL)
				streamURL = feed.Episodes[0].AudioURL
				if subtitle == "" || subtitle == "未知单集" || subtitle == "播客" {
					subtitle = feed.Episodes[0].Title
				}
				if feed.Cover != "" {
					cover = feed.Cover
				}
			} else {
				a.log.Warnf("从 RSS %s 获取单集失败: %v", streamURL, ferr)
			}
		}
	}

	a.mu.Lock()
	a.status.Playing = false
	a.status.Type = mediaType
	a.status.Title = title
	a.status.SubTitle = subtitle
	a.status.Cover = cover
	a.status.StreamURL = streamURL
	a.status.ProxyURL = ""
	a.status.ContentType = ""
	a.status.RendererState = "CONNECTING"
	a.status.Error = ""
	a.status.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
	a.mu.Unlock()

	ext := "mp3"
	ct := "audio/mpeg"
	format := ""
	resolvedURL := streamURL

	directExt := extFromURL(streamURL)
	isKnownDirect := (directExt == "mp3" || directExt == "m4a" || directExt == "aac" || directExt == "flac" || directExt == "wav" || directExt == "ogg") && !looksLikeHLSURL(streamURL)

	if isKnownDirect {
		ext = directExt
		ct = mimeForExt(ext)
		a.log.Infof("直链音频跳过探测直接推流: %s (ext=%s ct=%s)", streamURL, ext, ct)
	} else {
		// Fast probe (3 seconds timeout), fallback to URL ext MIME if timed out or failed
		probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Second)
		probe, err := a.probeStream(probeCtx, streamURL)
		probeCancel()
		if err == nil && probe != nil {
			if probe.Ext != "" {
				ext = probe.Ext
			}
			if probe.ContentType != "" {
				ct = probe.ContentType
			}
			format = probe.Format
			if probe.FinalURL != "" {
				resolvedURL = probe.FinalURL
			}
		} else {
			if uExt := extFromURL(streamURL); uExt != "" {
				ext = uExt
				ct = mimeForExt(ext)
			}
		}
	}

	// Secondary check: if probe returned xml/html content-type for a podcast URL
	if strings.Contains(ct, "xml") || strings.Contains(ct, "text/html") {
		a.log.Warnf("探测到非音频 Content-Type: %s (%s)，尝试作为播客 RSS 解析…", ct, streamURL)
		feed, ferr := a.FetchPodcastEpisodes(ctx, streamURL, false)
		if ferr == nil && feed != nil && len(feed.Episodes) > 0 {
			a.log.Infof("成功从 RSS 解析到最新音频: %s -> %s", feed.Episodes[0].Title, feed.Episodes[0].AudioURL)
			streamURL = feed.Episodes[0].AudioURL
			subtitle = feed.Episodes[0].Title
			if feed.Cover != "" {
				cover = feed.Cover
			}
			probeCtx2, probeCancel2 := context.WithTimeout(ctx, 3*time.Second)
			probe2, err2 := a.probeStream(probeCtx2, streamURL)
			probeCancel2()
			if err2 == nil && probe2 != nil {
				if probe2.Ext != "" {
					ext = probe2.Ext
				}
				if probe2.ContentType != "" {
					ct = probe2.ContentType
				}
				format = probe2.Format
				if probe2.FinalURL != "" {
					resolvedURL = probe2.FinalURL
				}
			} else {
				ext = "mp3"
				ct = "audio/mpeg"
			}
		}
	}

	entry := a.registerMedia(mediaType, stableHash(streamURL, 20), streamURL, title, subtitle, cover, ext, ct, format)
	a.mu.Lock()
	if old, ok := a.media[entry.Token]; ok {
		old.ResolvedURL = resolvedURL
		old.ContentType = ct
		old.Ext = ext
		old.Format = format
		c := *old
		entry = &c
	}
	a.mu.Unlock()
	_ = a.saveMedia()

	r, err := a.ensureRenderer(8 * time.Second)
	if err != nil {
		msg := "未找到播放器: " + err.Error()
		a.setStatusError(msg)
		a.setLastStreamError(msg)
		return errors.New(msg)
	}

	proxyURL := a.mediaProxyURL(entry)
	a.mu.Lock()
	a.status.ProxyURL = proxyURL
	a.status.ContentType = ct
	a.mu.Unlock()

	// Stop whatever is currently playing
	stopCtx, stopCancel := context.WithTimeout(ctx, 2*time.Second)
	_ = a.stopRenderer(stopCtx, r)
	stopCancel()

	// Prepare cover image and cache bytes
	coverBytes := a.prepareCoverImage(ctx, cover, mediaType, title, subtitle)
	a.currentCoverMu.Lock()
	a.currentCoverBytes = coverBytes
	a.currentCoverMu.Unlock()

	// Write directly to /usr/data/fiio/net_cover.jpg so it is immediately available
	_ = os.MkdirAll("/usr/data/fiio", 0755)
	_ = os.WriteFile("/usr/data/fiio/net_cover.jpg", coverBytes, 0644)

	localCoverURL := fmt.Sprintf("http://127.0.0.1:%d/cover.jpg", a.port)
	a.mu.Lock()
	a.status.Cover = localCoverURL
	a.mu.Unlock()

	meta := didlMetadata(title, subtitle, album, localCoverURL, proxyURL, ct)
	setCtx, setCancel := context.WithTimeout(ctx, 6*time.Second)
	err = a.setAVTransportURI(setCtx, r, proxyURL, meta)
	setCancel()
	if err != nil {
		a.log.Warnf("SetAVTransportURI 带元数据失败，尝试空元数据: %v", err)
		retryCtx, retryCancel := context.WithTimeout(ctx, 6*time.Second)
		err = a.setAVTransportURI(retryCtx, r, proxyURL, "")
		retryCancel()
	}
	if err != nil {
		a.log.Warnf("设置播放地址遇到连接失败 (%v)，播放器可能刚重启或更换端口，立即重新发现…", err)
		a.setRenderer(nil)
		if freshR, ferr := a.ensureRenderer(6 * time.Second); ferr == nil && freshR != nil {
			r = freshR
			a.log.Infof("已重新发现活跃播放器: %s，立即重新下发播放地址", r.ControlURL)
			retryCtx, retryCancel := context.WithTimeout(ctx, 6*time.Second)
			err = a.setAVTransportURI(retryCtx, r, proxyURL, meta)
			retryCancel()
			if err != nil {
				retryCtx2, retryCancel2 := context.WithTimeout(ctx, 6*time.Second)
				err = a.setAVTransportURI(retryCtx2, r, proxyURL, "")
				retryCancel2()
			}
		}
	}
	if err != nil {
		msg := "设置播放地址失败: " + err.Error()
		a.setStatusError(msg)
		a.setLastStreamError(msg)
		return errors.New(msg)
	}

	playCtx, playCancel := context.WithTimeout(ctx, 6*time.Second)
	err = a.rendererPlay(playCtx, r)
	playCancel()
	if err != nil {
		a.log.Warnf("启动播放遇到连接断开，重试 Play: %v", err)
		retryPlayCtx, retryPlayCancel := context.WithTimeout(ctx, 6*time.Second)
		err = a.rendererPlay(retryPlayCtx, r)
		retryPlayCancel()
	}
	if err != nil {
		a.log.Warnf("启动播放重试失败 (%v)，尝试重新发现播放器并恢复播放…", err)
		a.setRenderer(nil)
		if freshR, ferr := a.ensureRenderer(6 * time.Second); ferr == nil && freshR != nil {
			r = freshR
			_ = a.setAVTransportURI(ctx, r, proxyURL, meta)
			playRetryCtx2, playRetryCancel2 := context.WithTimeout(ctx, 6*time.Second)
			err = a.rendererPlay(playRetryCtx2, r)
			playRetryCancel2()
		}
	}
	if err != nil {
		msg := "启动播放失败: " + err.Error()
		a.setStatusError(msg)
		a.setLastStreamError(msg)
		return errors.New(msg)
	}

	a.mu.Lock()
	a.status.Playing = true
	a.status.Error = ""
	a.status.RendererState = "PLAYING"
	a.status.RendererURI = proxyURL
	a.status.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
	a.mu.Unlock()

	// Remove any online-music session files so native DLNA UI and stream audio info are fully visible
	_ = os.Remove("/tmp/disc-online-music/session")
	_ = os.Remove("/tmp/disc-online-music/active")
	_ = os.Remove("/tmp/disc-online-music/nowplaying")

	a.log.Infof("开始播放: %s / %s  renderer=%s proxy=%s cover=%s", title, subtitle, r.ControlURL, proxyURL, localCoverURL)
	return nil
}

// waitForPlayback checks if renderer enters PLAYING non-destructively for placeholder or transition checks
func (a *App) waitForPlayback(ctx context.Context, r *RendererInfo, expectedURI string) (string, error) {
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(3 * time.Second)
	lastState := "TRANSITIONING"
	for {
		select {
		case <-ctx.Done():
			return lastState, ctx.Err()
		case <-timeout:
			return lastState, nil
		case <-ticker.C:
			qctx, qcancel := context.WithTimeout(ctx, 800*time.Millisecond)
			info, err := a.getTransportInfo(qctx, r)
			qcancel()
			if err == nil && info != nil {
				st := info["CurrentTransportState"]
				if st != "" {
					lastState = st
					if st == "PLAYING" {
						return "PLAYING", nil
					}
				}
			}
		}
	}
}

func (a *App) StopPlay() error {
	a.mu.Lock()
	a.status.Playing = false
	a.status.Error = ""
	a.status.RendererState = "STOPPED"
	a.status.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
	a.mu.Unlock()

	_ = os.Remove("/tmp/disc-online-music/active")

	r := a.currentRenderer()
	if r == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(a.ctx, 4*time.Second)
	defer cancel()
	if err := a.stopRenderer(ctx, r); err != nil {
		a.log.Warnf("停止播放失败: %v", err)
		return err
	}
	a.log.Infof("播放已停止")
	return nil
}

// --- HTTP audio proxy ---

func (a *App) handleAudio(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	base := strings.TrimPrefix(r.URL.Path, "/audio/")
	token := strings.SplitN(base, ".", 2)[0]
	entry := a.lookupMedia(token)
	if entry == nil {
		a.log.Warnf("音频代理 404: token=%s ua=%q range=%q", token, r.UserAgent(), r.Header.Get("Range"))
		http.Error(w, "media token not found", http.StatusNotFound)
		return
	}
	a.log.Infof("音频代理开始: %s type=%s ext=%s ua=%q range=%q", entry.Title, entry.ContentType, entry.Ext, r.UserAgent(), r.Header.Get("Range"))
	atomic.AddInt64(&a.activeStreams, 1)
	defer atomic.AddInt64(&a.activeStreams, -1)
	start := time.Now()
	cw := &countingWriter{ResponseWriter: w}
	if err := a.streamEntry(r.Context(), cw, r, entry, 0); err != nil {
		a.log.Warnf("音频代理失败: %s: %v (已发送 %d 字节, %s)", entry.Title, err, cw.n, time.Since(start).Round(time.Millisecond))
		a.setLastStreamError(entry.Title + ": " + err.Error())
		if cw.n == 0 {
			http.Error(w, "音频代理失败: "+err.Error(), http.StatusBadGateway)
		}
		return
	}
	a.log.Infof("音频代理结束: %s (%d 字节, %s)", entry.Title, cw.n, time.Since(start).Round(time.Millisecond))
}

type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(b []byte) (int, error) {
	n, err := c.ResponseWriter.Write(b)
	c.n += int64(n)
	return n, err
}

func (c *countingWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (a *App) streamEntry(ctx context.Context, w http.ResponseWriter, r *http.Request, entry *MediaEntry, depth int) error {
	if depth > 3 {
		return errors.New("播放列表嵌套层级过深")
	}
	target := entry.ResolvedURL
	if target == "" {
		target = entry.URL
	}
	if entry.Format == "hls" || looksLikeHLSURL(target) {
		return a.serveHLS(ctx, w, r, entry, target)
	}


	resp, err := a.openUpstream(ctx, target, r.Header.Get("Range"))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	br := bufio.NewReaderSize(resp.Body, 32<<10)
	peek, _ := br.Peek(8192)
	ct := normalizeContentType(resp.Header.Get("Content-Type"))
	if isPlaylistBody(peek, ct) {
		data := make([]byte, 0, 8192)
		tmp := make([]byte, 8192)
		for len(data) < 512<<10 {
			n, er := br.Read(tmp)
			if n > 0 {
				data = append(data, tmp[:n]...)
			}
			if er != nil || n == 0 {
				break
			}
		}
		baseURL := resp.Request.URL.String()
		_ = resp.Body.Close()
		if bytes.Contains(data, []byte("#EXT-X-")) {
			return a.serveHLS(ctx, w, r, entry, baseURL)
		}
		next := firstPlaylistURL(string(data), baseURL)
		if next == "" {
			return errors.New("播放列表中没有音频地址")
		}
		entry2 := *entry
		entry2.ResolvedURL = next
		entry2.Format = ""
		return a.streamEntry(ctx, w, r, &entry2, depth+1)
	}
	if ct == "" || ct == "application/octet-stream" {
		ct = sniffContentType(peek)
	}
	if ct == "text/html" {
		return errors.New("源地址返回网页而不是音频")
	}
	if ct == "video/mp2t" {
		return a.serveTSResponse(ctx, w, br, resp, r)
	}
	if ct == "" {
		ct = mimeForExt(entry.Ext)
	}
	if ct == "application/vnd.apple.mpegurl" {
		return a.serveHLS(ctx, w, r, entry, resp.Request.URL.String())
	}

	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Cache-Control", "no-cache, no-store")
	if resp.ContentLength >= 0 && resp.Header.Get("Content-Length") != "" {
		h.Set("Content-Length", resp.Header.Get("Content-Length"))
	}
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		h.Set("Content-Range", cr)
	}
	if resp.StatusCode == http.StatusPartialContent || resp.Header.Get("Accept-Ranges") != "" {
		h.Set("Accept-Ranges", "bytes")
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !resp.Uncompressed {
		h.Set("Content-Encoding", enc)
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return nil
	}
	live := entry.Kind == "radio"
	return copyAndFlush(ctx, w, br, live)
}

func (a *App) serveTSResponse(ctx context.Context, w http.ResponseWriter, br *bufio.Reader, resp *http.Response, r *http.Request) error {
	d := &tsDemux{}
	var out bytes.Buffer
	buf := make([]byte, 64<<10)
	for out.Len() < 256<<10 {
		n, er := br.Read(buf)
		if n > 0 {
			_, _ = d.feed(buf[:n], &out)
			if d.audioFormat != "" && out.Len() > 0 {
				break
			}
		}
		if er != nil || n == 0 {
			if er != nil && er != io.EOF {
				return er
			}
			break
		}
	}
	if out.Len() == 0 {
		return errors.New("MPEG-TS 流中没有找到音频")
	}
	ct := "audio/aac"
	if d.audioFormat == "mp3" {
		ct = "audio/mpeg"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return nil
	}
	if _, err := w.Write(out.Bytes()); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	for {
		n, er := br.Read(buf)
		if n > 0 {
			var tmp bytes.Buffer
			_, _ = d.feed(buf[:n], &tmp)
			if tmp.Len() > 0 {
				if _, ew := w.Write(tmp.Bytes()); ew != nil {
					return ew
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		}
		if er != nil {
			if er == io.EOF {
				return nil
			}
			return er
		}
	}
}

const (
	defaultBurstBytes   = 2 * 1024 * 1024 // 2 MB 初始突发：保证播放器秒开缓冲并解析元数据 (彻底消除 FFmpeg 探测被限速卡死)
	defaultPaceBytesSec = 96 * 1024       // 96 KB/s (~768 kbps) 平滑控速：保证高码率不卡顿并防止环形缓冲溢出
)

func copyAndFlush(ctx context.Context, w http.ResponseWriter, src io.Reader, live bool) error {
	buf := make([]byte, 16<<10)
	lastFlush := time.Now()
	var totalSent int64
	var burstOverTime time.Time

	timer := time.NewTimer(0)
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, ew := w.Write(buf[:n]); ew != nil {
				return ew
			}
			totalSent += int64(n)

			if live {
				if time.Since(lastFlush) > 100*time.Millisecond {
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					lastFlush = time.Now()
				}
			} else {
				flushInterval := 250 * time.Millisecond
				if totalSent <= defaultBurstBytes {
					flushInterval = 50 * time.Millisecond // 初始突发阶段更频繁flush，降低首帧等待时延
				}
				if time.Since(lastFlush) > flushInterval {
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					lastFlush = time.Now()
				}
				if totalSent > defaultBurstBytes {
					if burstOverTime.IsZero() {
						burstOverTime = time.Now()
					}
					pacedBytes := totalSent - defaultBurstBytes
					expectedElapsed := time.Duration(pacedBytes) * time.Second / defaultPaceBytesSec
					actualElapsed := time.Since(burstOverTime)
					if expectedElapsed > actualElapsed {
						sleepDur := expectedElapsed - actualElapsed
						if sleepDur > 250*time.Millisecond {
							sleepDur = 250 * time.Millisecond
						}
						timer.Reset(sleepDur)
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-timer.C:
						}
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func ffmpegBinary() string {
	for _, p := range []string{"/usr/bin/ffmpeg", "/usr/local/bin/ffmpeg"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return ""
}


// --- Playlist / HLS parsing ---

func looksLikeHLSURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	p := strings.ToLower(u.Path)
	return strings.HasSuffix(p, ".m3u8") || strings.Contains(strings.ToLower(u.RawQuery), "m3u8")
}

func isPlaylistBody(peek []byte, contentType string) bool {
	ct := normalizeContentType(contentType)
	if ct == "application/vnd.apple.mpegurl" || ct == "audio/x-scpls" {
		return true
	}
	t := bytes.TrimSpace(peek)
	return bytes.HasPrefix(t, []byte("#EXTM3U")) || bytes.HasPrefix(t, []byte("#EXTINF")) ||
		bytes.HasPrefix([]byte(strings.ToLower(string(t))), []byte("[playlist]"))
}

func firstPlaylistURL(text, base string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(strings.ToLower(line), "file") && strings.Contains(line, "=") {
			line = strings.TrimSpace(strings.SplitN(line, "=", 2)[1])
		}
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			return line
		}
		if base != "" && !strings.Contains(line, "://") {
			return resolveURL(base, line)
		}
	}
	return ""
}

type hlsVariant struct {
	URL       string
	Bandwidth int
}

type hlsSegment struct {
	Seq           int64
	URL           string
	Duration      float64
	ByteRange     string
	Discontinuity bool
}

type hlsPlaylist struct {
	URL            string
	Master         bool
	Variants       []hlsVariant
	MediaSequence  int64
	TargetDuration float64
	EndList        bool
	Segments       []hlsSegment
	MapURL         string
	Encrypted      bool
}

func (a *App) loadMediaPlaylist(ctx context.Context, raw string, depth int) (*hlsPlaylist, error) {
	if depth > 4 {
		return nil, errors.New("HLS 播放列表嵌套过深")
	}
	data, _, err := a.fetchBytes(ctx, raw, maxPlaylistBytes)
	if err != nil {
		return nil, err
	}
	text := strings.TrimPrefix(string(data), "\ufeff")
	if !strings.HasPrefix(strings.TrimSpace(text), "#EXTM3U") {
		return nil, fmt.Errorf("不是有效的 HLS 播放列表: %s", raw)
	}
	pl, err := parseHLSPlaylist(text, raw)
	if err != nil {
		return nil, err
	}
	if pl.Master {
		if len(pl.Variants) == 0 {
			return nil, errors.New("HLS master 播放列表没有清晰度条目")
		}
		best := pl.Variants[0]
		for _, v := range pl.Variants[1:] {
			if v.Bandwidth > best.Bandwidth {
				best = v
			}
		}
		return a.loadMediaPlaylist(ctx, best.URL, depth+1)
	}
	if pl.Encrypted {
		return nil, errors.New("该 HLS 使用了 AES-128 加密，当前版本暂不支持")
	}
	if len(pl.Segments) == 0 {
		return nil, errors.New("HLS 播放列表没有分片")
	}
	return pl, nil
}

func parseHLSPlaylist(text, base string) (*hlsPlaylist, error) {
	pl := &hlsPlaylist{URL: base}
	var pendingDuration float64
	var pendingBandwidth int = -1
	var pendingRange string
	var pendingDisc bool
	for _, rawLine := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:") {
			pl.MediaSequence, _ = strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:")), 10, 64)
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-TARGETDURATION:") {
			pl.TargetDuration, _ = strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:")), 64)
			continue
		}
		if line == "#EXT-X-ENDLIST" {
			pl.EndList = true
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-MAP:") {
			if u := hlsAttr(line, "URI"); u != "" {
				pl.MapURL = resolveURL(base, u)
			}
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-KEY:") {
			method := strings.ToUpper(hlsAttr(line, "METHOD"))
			if method != "" && method != "NONE" {
				pl.Encrypted = true
			}
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-BYTERANGE:") {
			pendingRange = strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-BYTERANGE:"))
			continue
		}
		if line == "#EXT-X-DISCONTINUITY" {
			pendingDisc = true
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			v := strings.TrimSpace(strings.TrimPrefix(line, "#EXTINF:"))
			if i := strings.Index(v, ","); i >= 0 {
				v = v[:i]
			}
			pendingDuration, _ = strconv.ParseFloat(strings.TrimSpace(v), 64)
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			pendingBandwidth = 0
			if b := hlsAttr(line, "BANDWIDTH"); b != "" {
				pendingBandwidth, _ = strconv.Atoi(b)
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		abs := resolveURL(base, line)
		if pendingBandwidth >= 0 {
			pl.Variants = append(pl.Variants, hlsVariant{URL: abs, Bandwidth: pendingBandwidth})
			pendingBandwidth = -1
			continue
		}
		seg := hlsSegment{
			Seq:           pl.MediaSequence + int64(len(pl.Segments)),
			URL:           abs,
			Duration:      pendingDuration,
			ByteRange:     pendingRange,
			Discontinuity: pendingDisc,
		}
		pl.Segments = append(pl.Segments, seg)
		pendingDuration = 0
		pendingRange = ""
		pendingDisc = false
	}
	pl.Master = len(pl.Variants) > 0 && len(pl.Segments) == 0
	return pl, nil
}

func hlsAttr(line, key string) string {
	re := regexp.MustCompile(`(?i)(?:^|[:,])\s*` + regexp.QuoteMeta(key) + `\s*=\s*"([^"]*)"`)
	if m := re.FindStringSubmatch(line); len(m) > 1 {
		return m[1]
	}
	re2 := regexp.MustCompile(`(?i)(?:^|[:,])\s*` + regexp.QuoteMeta(key) + `\s*=\s*([^,]+)`)
	if m := re2.FindStringSubmatch(line); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func (a *App) fetchHLSSegment(ctx context.Context, seg hlsSegment) ([]byte, string, error) {
	rng := ""
	if seg.ByteRange != "" {
		if i := strings.Index(seg.ByteRange, "@"); i > 0 {
			length, e1 := strconv.ParseInt(seg.ByteRange[:i], 10, 64)
			offset, e2 := strconv.ParseInt(seg.ByteRange[i+1:], 10, 64)
			if e1 == nil && e2 == nil && length > 0 && offset >= 0 {
				rng = fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
			}
		}
	}
	resp, err := a.openUpstream(ctx, seg.URL, rng)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 24<<20))
	if err != nil {
		return nil, "", err
	}
	if len(data) >= 24<<20 {
		return nil, "", errors.New("HLS 分片超过 24MB 限制")
	}
	return data, normalizeContentType(resp.Header.Get("Content-Type")), nil
}

func detectSegmentFormat(data []byte) string {
	if len(data) >= 188 && data[0] == 0x47 {
		return "ts"
	}
	if len(data) > 12 && (string(data[4:8]) == "ftyp" || string(data[4:8]) == "styp" || string(data[4:8]) == "moof") {
		return "mp4"
	}
	if bytes.HasPrefix(data, []byte("ID3")) {
		return "mp3"
	}
	if len(data) > 2 && data[0] == 0xFF && (data[1]&0xE0) == 0xE0 {
		if data[1]&0x06 == 0x00 {
			return "aac"
		}
		return "mp3"
	}
	return "unknown"
}

func (a *App) serveHLS(ctx context.Context, w http.ResponseWriter, r *http.Request, entry *MediaEntry, playlistURL string) error {
	pl, err := a.loadMediaPlaylist(ctx, playlistURL, 0)
	if err != nil {
		return err
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", mimeForExt(entry.Ext))
		w.Header().Set("Cache-Control", "no-cache, no-store")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
		return nil
	}

	start := 0
	if !pl.EndList && len(pl.Segments) > 1 {
		start = len(pl.Segments) - 1
	}
	demux := &tsDemux{}
	var first bytes.Buffer
	var firstFormat string
	var nextSeq int64
	started := false

	for attempt := 0; attempt < 3; attempt++ {
		idx := start - attempt
		if idx < 0 || idx >= len(pl.Segments) {
			continue
		}
		seg := pl.Segments[idx]
		data, _, err := a.fetchHLSSegment(ctx, seg)
		if err != nil {
			a.log.Warnf("HLS 首个分片失败 (%s): %v", seg.URL, err)
			continue
		}
		firstFormat = detectSegmentFormat(data)
		if firstFormat == "ts" {
			_, _ = demux.feed(data, &first)
		} else {
			if pl.MapURL != "" && firstFormat == "mp4" {
				if initData, _, e := a.fetchBytes(ctx, pl.MapURL, 8<<20); e == nil {
					first.Write(initData)
				}
			}
			first.Write(data)
		}
		if first.Len() > 0 {
			nextSeq = seg.Seq + 1
			started = true
			break
		}
	}
	if !started {
		return errors.New("HLS 首个分片没有解析出音频")
	}

	ext := entry.Ext
	ct := normalizeContentType(entry.ContentType)
	if firstFormat == "ts" {
		if demux.audioFormat == "mp3" {
			ext, ct = "mp3", "audio/mpeg"
		} else {
			ext, ct = "aac", "audio/aac"
		}
	} else if firstFormat == "mp4" {
		ext, ct = "m4a", "audio/mp4"
	} else if ct == "" {
		ct = mimeForExt(ext)
	}
	_ = ext
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(first.Bytes()); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	a.log.Infof("HLS 代理启动: %s 首片 %d 字节 格式=%s 输出=%s", entry.Title, first.Len(), firstFormat, ct)

	consecutiveErrors := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		if pl.EndList {
			found := false
			for _, seg := range pl.Segments {
				if seg.Seq < nextSeq {
					continue
				}
				found = true
				if err := a.writeHLSSegment(ctx, w, pl, demux, seg); err != nil {
					return err
				}
				nextSeq = seg.Seq + 1
			}
			if !found {
				return nil
			}
			continue
		}

		// Process any newly published segments in this playlist snapshot.
		progress := false
		for _, seg := range pl.Segments {
			if seg.Seq < nextSeq {
				continue
			}
			if err := a.writeHLSSegment(ctx, w, pl, demux, seg); err != nil {
				consecutiveErrors++
				a.log.Warnf("HLS 分片写入失败 (%d): %v", consecutiveErrors, err)
				if consecutiveErrors >= 8 {
					return err
				}
				break
			}
			consecutiveErrors = 0
			nextSeq = seg.Seq + 1
			progress = true
		}
		if progress {
			continue
		}

		wait := time.Duration(pl.TargetDuration * 500 * float64(time.Millisecond))
		if wait < 500*time.Millisecond {
			wait = 500 * time.Millisecond
		}
		if wait > 2500*time.Millisecond {
			wait = 2500 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}

		nextPL, err := a.loadMediaPlaylist(ctx, pl.URL, 0)
		if err != nil {
			consecutiveErrors++
			a.log.Warnf("刷新 HLS 播放列表失败 (%d): %v", consecutiveErrors, err)
			if consecutiveErrors >= 8 {
				return err
			}
			continue
		}
		if nextPL.MapURL == "" {
			nextPL.MapURL = pl.MapURL
		}
		if len(nextPL.Segments) > 0 {
			lastSeq := nextPL.Segments[len(nextPL.Segments)-1].Seq
			if nextSeq > lastSeq+1 {
				a.log.Warnf("HLS 序列回退或跳变，重新对齐: next=%d latest=%d", nextSeq, lastSeq)
				nextSeq = nextPL.Segments[len(nextPL.Segments)-1].Seq
			}
			firstSeq := nextPL.Segments[0].Seq
			if nextSeq < firstSeq {
				a.log.Warnf("HLS 序列落后，跳过 %d 个过期分片", firstSeq-nextSeq)
				nextSeq = firstSeq
			}
		}
		pl = nextPL
	}
}

func (a *App) writeHLSSegment(ctx context.Context, w http.ResponseWriter, pl *hlsPlaylist, demux *tsDemux, seg hlsSegment) error {
	data, _, err := a.fetchHLSSegment(ctx, seg)
	if err != nil {
		return err
	}
	format := detectSegmentFormat(data)
	if format == "ts" {
		var out bytes.Buffer
		if _, err := demux.feed(data, &out); err != nil {
			return err
		}
		if out.Len() > 0 {
			if _, err := w.Write(out.Bytes()); err != nil {
				return err
			}
		}
	} else {
		if _, err := w.Write(data); err != nil {
			return err
		}
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// --- MPEG-TS audio demux ---

type tsDemux struct {
	pmtPID      int
	audioPID    int
	audioFormat string
	logged      bool

	pesPending bool
	esActive   bool
	pesHdr     []byte
}

func (d *tsDemux) feed(data []byte, out io.Writer) (int, error) {
	if d.pmtPID == 0 {
		d.pmtPID = -1
	}
	if d.audioPID == 0 {
		d.audioPID = -1
	}
	offset := 0
	if len(data) >= 188 && data[0] != 0x47 {
		for i := 0; i+188 <= len(data) && i < 4096; i++ {
			if data[i] == 0x47 && (i+188 >= len(data) || data[i+188] == 0x47) {
				offset = i
				break
			}
		}
	}
	written := 0
	for i := offset; i+188 <= len(data); i += 188 {
		pkt := data[i : i+188]
		if pkt[0] != 0x47 {
			continue
		}
		pid := int(pkt[1]&0x1F)<<8 | int(pkt[2])
		pusi := pkt[1]&0x40 != 0
		afc := (pkt[3] >> 4) & 0x03
		off := 4
		if afc&0x02 != 0 {
			if off >= len(pkt) {
				continue
			}
			off += 1 + int(pkt[4])
		}
		if afc&0x01 == 0 || off >= len(pkt) {
			continue
		}
		payload := pkt[off:]

		switch {
		case pid == 0 && pusi:
			if p := parsePAT(payload); p > 0 {
				d.pmtPID = p
			}
		case pid == d.pmtPID && pusi:
			audioPID, format := parsePMT(payload)
			if audioPID > 0 {
				if d.audioPID != audioPID {
					d.audioPID = audioPID
					d.pesPending = false
					d.esActive = false
					d.pesHdr = nil
				}
				d.audioFormat = format
				d.logged = true
			}
		case pid == d.audioPID:
			n, err := d.feedPES(payload, pusi, out)
			written += n
			if err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

func (d *tsDemux) feedPES(payload []byte, pusi bool, out io.Writer) (int, error) {
	if pusi {
		d.pesPending = true
		d.esActive = false
		d.pesHdr = d.pesHdr[:0]
	}
	if d.pesPending {
		if len(d.pesHdr) < 128<<10 {
			d.pesHdr = append(d.pesHdr, payload...)
		}
		if len(d.pesHdr) >= 9 && d.pesHdr[0] == 0x00 && d.pesHdr[1] == 0x00 && d.pesHdr[2] == 0x01 {
			total := 9 + int(d.pesHdr[8])
			if len(d.pesHdr) >= total {
				n, err := out.Write(d.pesHdr[total:])
				d.pesHdr = d.pesHdr[:0]
				d.pesPending = false
				d.esActive = true
				return n, err
			}
			return 0, nil
		}
		if len(d.pesHdr) > 256<<10 {
			n, err := out.Write(d.pesHdr)
			d.pesHdr = d.pesHdr[:0]
			d.pesPending = false
			d.esActive = true
			return n, err
		}
		return 0, nil
	}
	if d.esActive {
		return out.Write(payload)
	}
	return 0, nil
}

func parsePAT(payload []byte) int {
	if len(payload) < 8 {
		return 0
	}
	ptr := int(payload[0])
	sec := payload[1+ptr:]
	if len(sec) < 8 || sec[0] != 0x00 {
		return 0
	}
	secLen := int(sec[1]&0x0F)<<8 | int(sec[2])
	end := 3 + secLen - 4
	if end > len(sec) {
		end = len(sec)
	}
	for pos := 8; pos+4 <= end; pos += 4 {
		program := int(sec[pos])<<8 | int(sec[pos+1])
		pid := int(sec[pos+2]&0x1F)<<8 | int(sec[pos+3])
		if program != 0 {
			return pid
		}
	}
	return 0
}

func parsePMT(payload []byte) (int, string) {
	if len(payload) < 12 {
		return 0, ""
	}
	ptr := int(payload[0])
	sec := payload[1+ptr:]
	if len(sec) < 12 || sec[0] != 0x02 {
		return 0, ""
	}
	secLen := int(sec[1]&0x0F)<<8 | int(sec[2])
	infoLen := int(sec[10]&0x0F)<<8 | int(sec[11])
	pos := 12 + infoLen
	end := 3 + secLen - 4
	if end > len(sec) {
		end = len(sec)
	}
	bestPID, bestFormat, bestRank := 0, "", 99
	for pos+5 <= end {
		streamType := sec[pos]
		pid := int(sec[pos+1]&0x1F)<<8 | int(sec[pos+2])
		esLen := int(sec[pos+3]&0x0F)<<8 | int(sec[pos+4])
		format, rank := "", 99
		switch streamType {
		case 0x0F:
			format, rank = "aac", 1
		case 0x03, 0x04:
			format, rank = "mp3", 2
		case 0x11:
			format, rank = "aac-latm", 3
		case 0x81, 0x87:
			format, rank = "ac3", 4
		}
		if format != "" && rank < bestRank {
			bestPID, bestFormat, bestRank = pid, format, rank
		}
		pos += 5 + esLen
	}
	return bestPID, bestFormat
}

// --- Sniffing ---

func sniffContentType(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if bytes.HasPrefix(b, []byte("ID3")) {
		return "audio/mpeg"
	}
	if len(b) > 2 && b[0] == 0xFF && (b[1]&0xE0) == 0xE0 {
		if b[1]&0x06 == 0x00 {
			return "audio/aac"
		}
		return "audio/mpeg"
	}
	if bytes.HasPrefix(b, []byte("fLaC")) {
		return "audio/flac"
	}
	if bytes.HasPrefix(b, []byte("OggS")) {
		return "audio/ogg"
	}
	if bytes.HasPrefix(b, []byte("RIFF")) {
		return "audio/wav"
	}
	if bytes.HasPrefix(b, []byte("MAC ")) {
		return "audio/ape"
	}
	if len(b) > 12 && (string(b[4:8]) == "ftyp" || string(b[4:8]) == "styp") {
		return "audio/mp4"
	}
	if len(b) > 188 && b[0] == 0x47 && b[188] == 0x47 {
		return "video/mp2t"
	}
	t := bytes.TrimSpace(b)
	if bytes.HasPrefix(t, []byte("#EXTM3U")) {
		return "application/vnd.apple.mpegurl"
	}
	lower := strings.ToLower(string(t))
	if strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html") {
		return "text/html"
	}
	return ""
}
