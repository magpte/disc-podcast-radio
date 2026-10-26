package main

import (
	"context"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf16"
)

// The stock player's local playlist importer only accepts files that exist on
// disk ("playlist: only support local_play!"). To make a generated playlist
// browsable on the device we therefore point every entry at a tiny local silent
// MP3 carrying the station/episode title, then use the player's open file
// descriptor to notice which entry was selected and switch AVTransport to the
// real stream proxy.
//
//go:embed assets/silence.mp3
var silenceMP3 []byte

const placeholderDirName = ".disc-placeholders"

func (a *App) placeholderRoot() string {
	if strings.TrimSpace(a.playlistRoot) == "" {
		return ""
	}
	return filepath.Join(a.playlistRoot, placeholderDirName)
}

func (a *App) ensurePlaceholder(entry *MediaEntry, kind, group, displayTitle string) (string, error) {
	if entry == nil {
		return "", errors.New("媒体登记为空")
	}
	root := a.placeholderRoot()
	if root == "" {
		return "", errors.New("播放列表目录为空")
	}
	kind = strings.TrimSpace(kind)
	if kind != "podcast" {
		kind = "radio"
	}
	group = sanitizeFilename(group)
	if group == "" {
		group = "未分类"
	}
	dir := filepath.Join(root, kind, group)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}

	// Reuse the path assigned on a previous build when the file is still there.
	// This keeps playlist imports stable across restarts and avoids creating a
	// new "(2)" file every time the player rescans the TF card.
	if old := filepath.Clean(strings.TrimSpace(entry.PlaceholderPath)); old != "" && old != "." {
		if strings.HasPrefix(old, filepath.Clean(root)+string(os.PathSeparator)) && fileExists(old) &&
			!strings.ContainsAny(filepath.Base(old), "#%") {
			entry.PlaceholderPath = old
			a.setPlaceholderPath(entry.Token, old)
			return old, nil
		}
	}

	base := sanitizeFilename(displayTitle)
	if base == "" {
		base = sanitizeFilename(entry.Title)
	}
	if base == "" {
		base = "未命名"
	}
	candidate := filepath.Join(dir, base+".mp3")
	for n := 2; a.placeholderPathTaken(candidate, entry.Token); n++ {
		candidate = filepath.Join(dir, fmt.Sprintf("%s (%d).mp3", base, n))
	}

	album := "DISC 网络电台"
	if kind == "podcast" {
		album = "DISC 播客"
	}
	data := buildPlaceholderMP3(displayTitle, entry.Subtitle, album, entry.Token)
	if st, err := os.Stat(candidate); err != nil || st.Size() != int64(len(data)) {
		if err := writeFileAtomic(candidate, data, 0644); err != nil {
			return "", err
		}
	}
	entry.PlaceholderPath = candidate
	a.setPlaceholderPath(entry.Token, candidate)
	return candidate, nil
}

func (a *App) setPlaceholderPath(token, path string) {
	if token == "" || path == "" {
		return
	}
	a.mu.Lock()
	if entry := a.media[token]; entry != nil {
		entry.PlaceholderPath = path
	}
	a.mu.Unlock()
}

func (a *App) placeholderPathTaken(path, token string) bool {
	clean := filepath.Clean(path)
	a.mu.RLock()
	defer a.mu.RUnlock()
	for otherToken, entry := range a.media {
		if otherToken == token || entry == nil || entry.PlaceholderPath == "" {
			continue
		}
		if filepath.Clean(entry.PlaceholderPath) == clean {
			return true
		}
	}
	return false
}

func buildPlaceholderMP3(title, artist, album, token string) []byte {
	return append(id3v23Tag(title, artist, album, token), silenceMP3...)
}

func id3v23Tag(title, artist, album, token string) []byte {
	var frames []byte
	frames = append(frames, id3TextFrame("TIT2", title)...)
	frames = append(frames, id3TextFrame("TPE1", artist)...)
	frames = append(frames, id3TextFrame("TALB", album)...)
	if token != "" {
		frames = append(frames, id3TXXXFrame("DISC_PLACEHOLDER", token)...)
	}
	size := len(frames)
	header := []byte{
		'I', 'D', '3', 3, 0, 0,
		byte((size >> 21) & 0x7f),
		byte((size >> 14) & 0x7f),
		byte((size >> 7) & 0x7f),
		byte(size & 0x7f),
	}
	return append(header, frames...)
}

func id3TextFrame(id, text string) []byte {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\x00", ""))
	if text == "" {
		return nil
	}
	// ID3v2.3 does not define UTF-8. Use UTF-16LE with a BOM (encoding 1),
	// which is understood by the stock player's tag reader and preserves
	// Chinese titles.
	payload := append([]byte{1}, id3UTF16LE(text)...)
	frame := make([]byte, 10+len(payload))
	copy(frame[0:4], id)
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[10:], payload)
	return frame
}

func id3TXXXFrame(description, value string) []byte {
	description = strings.TrimSpace(strings.ReplaceAll(description, "\x00", ""))
	value = strings.TrimSpace(strings.ReplaceAll(value, "\x00", ""))
	if description == "" || value == "" {
		return nil
	}
	payload := []byte{1}
	payload = append(payload, id3UTF16LE(description)...)
	payload = append(payload, 0, 0)
	payload = append(payload, id3UTF16LE(value)...)
	frame := make([]byte, 10+len(payload))
	copy(frame[0:4], "TXXX")
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[10:], payload)
	return frame
}

func id3UTF16LE(text string) []byte {
	units := utf16.Encode([]rune(text))
	out := make([]byte, 2+len(units)*2)
	out[0], out[1] = 0xff, 0xfe
	for i, unit := range units {
		binary.LittleEndian.PutUint16(out[2+i*2:], unit)
	}
	return out
}

func (a *App) cleanupPlaceholders(used map[string]bool) {
	root := a.placeholderRoot()
	if root == "" {
		return
	}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".mp3") {
			return nil
		}
		if !used[filepath.Clean(path)] {
			_ = os.Remove(path)
		}
		return nil
	})
	removeEmptyDirs(filepath.Join(root, "radio"), root)
	removeEmptyDirs(filepath.Join(root, "podcast"), root)
}

func (a *App) placeholderCount() int {
	root := a.placeholderRoot()
	if root == "" {
		return 0
	}
	n := 0
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() && strings.EqualFold(filepath.Ext(path), ".mp3") {
			n++
		}
		return nil
	})
	return n
}

// --- Detecting a selected placeholder ---

func (a *App) localPlaybackLoop() {
	ticker := time.NewTicker(600 * time.Millisecond)
	defer ticker.Stop()
	tick := 0
	pendingPath := ""
	pendingCount := 0
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
		}
		tick++
		if entry, ok := a.findOpenPlaceholder(); ok {
			// Require the same file to stay open for two polls. Importing or
			// scanning a playlist opens files briefly to read ID3 tags; a real
			// playback keeps the descriptor open.
			if entry.PlaceholderPath == pendingPath {
				pendingCount++
			} else {
				pendingPath = entry.PlaceholderPath
				pendingCount = 1
			}
			if pendingCount >= 2 {
				a.maybeSwitchPlaceholder(entry, "proc-fd")
				pendingPath = ""
				pendingCount = 0
			}
			continue
		}
		pendingPath = ""
		pendingCount = 0
		if tick%4 == 0 {
			a.checkRendererPlaceholder()
		}
	}
}

func (a *App) cachedPlayerPIDs() []int {
	a.playerPIDMu.Lock()
	defer a.playerPIDMu.Unlock()
	if time.Since(a.playerPIDsAt) < 3*time.Second {
		return append([]int(nil), a.playerPIDs...)
	}
	a.playerPIDs = a.processIDsMatching("mq_player")
	a.playerPIDsAt = time.Now()
	return append([]int(nil), a.playerPIDs...)
}

func (a *App) findOpenPlaceholder() (*MediaEntry, bool) {
	pids := a.cachedPlayerPIDs()
	if len(pids) == 0 {
		return nil, false
	}
	var best *MediaEntry
	bestPos := int64(-1)
	for _, pid := range pids {
		fdDir := fmt.Sprintf("/proc/%d/fd", pid)
		entries, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range entries {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			entry, ok := a.lookupPlaceholderPath(target)
			if !ok {
				continue
			}
			pos := readFDPosition(pid, fd.Name())
			if pos > bestPos {
				best = entry
				bestPos = pos
			}
		}
	}
	if best != nil {
		return best, true
	}
	return nil, false
}

func readFDPosition(pid int, fd string) int64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/fdinfo/%s", pid, fd))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "pos:") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, "pos:"))
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

func (a *App) checkRendererPlaceholder() {
	r := a.currentRenderer()
	if r == nil {
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Second)
	vals, err := a.getPositionInfo(ctx, r)
	cancel()
	if err != nil {
		a.log.Debugf("读取播放器当前曲目失败: %v", err)
		return
	}
	if entry, ok := a.lookupPlaceholderPath(vals["TrackURI"]); ok {
		a.maybeSwitchPlaceholder(entry, "upnp-position")
		return
	}
	if title := didlTitle(vals["TrackMetaData"]); title != "" {
		if entry, ok := a.lookupPlaceholderTitle(title); ok {
			a.maybeSwitchPlaceholder(entry, "upnp-title")
		}
	}
}

var didlTitleRE = regexp.MustCompile(`(?is)<(?:dc:)?title[^>]*>(.*?)</(?:dc:)?title>`)

func didlTitle(metadata string) string {
	if strings.TrimSpace(metadata) == "" {
		return ""
	}
	decoded := xmlUnescape(metadata)
	if m := didlTitleRE.FindStringSubmatch(decoded); len(m) == 2 {
		return strings.TrimSpace(xmlUnescape(m[1]))
	}
	return ""
}

func xmlUnescape(s string) string {
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", "\"")
	s = strings.ReplaceAll(s, "&apos;", "'")
	s = strings.ReplaceAll(s, "&amp;", "&")
	return s
}

func (a *App) maybeSwitchPlaceholder(entry *MediaEntry, reason string) {
	if entry == nil || strings.TrimSpace(entry.PlaceholderPath) == "" {
		return
	}
	// Do not mark a selection as handled when another switch is in flight.
	// Otherwise selecting B while A is switching would suppress B for 15s.
	if !atomic.CompareAndSwapInt32(&a.placeholderSwitching, 0, 1) {
		return
	}
	defer atomic.StoreInt32(&a.placeholderSwitching, 0)

	a.placeholderLastMu.Lock()
	path := filepath.Clean(entry.PlaceholderPath)
	if a.placeholderLastPath == path && time.Since(a.placeholderLastAt) < 15*time.Second {
		a.placeholderLastMu.Unlock()
		return
	}
	a.placeholderLastPath = path
	a.placeholderLastAt = time.Now()
	a.placeholderLastMu.Unlock()

	a.mu.RLock()
	already := a.status.Playing && a.status.StreamURL == entry.URL
	a.mu.RUnlock()
	if already {
		return
	}

	a.log.Infof("检测到播放器打开占位文件(%s): %s -> 切换真实流 %s", reason, entry.PlaceholderPath, entry.URL)
	ctx, cancel := context.WithTimeout(a.ctx, 50*time.Second)
	defer cancel()
	if err := a.playMediaEntry(ctx, entry); err != nil {
		a.log.Warnf("占位文件切换播放失败: %s: %v", entry.Title, err)
	}
}

func (a *App) lookupPlaceholderPath(raw string) (*MediaEntry, bool) {
	clean := normalizePlayerPath(raw)
	if clean == "" {
		return nil, false
	}
	base := filepath.Base(clean)
	var found *MediaEntry
	baseMatches := 0
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, entry := range a.media {
		if entry == nil || entry.PlaceholderPath == "" {
			continue
		}
		if filepath.Clean(entry.PlaceholderPath) == clean {
			c := *entry
			return &c, true
		}
		if filepath.Base(entry.PlaceholderPath) == base {
			baseMatches++
			c := *entry
			found = &c
		}
	}
	if baseMatches == 1 {
		return found, true
	}
	return nil, false
}

func (a *App) lookupPlaceholderTitle(title string) (*MediaEntry, bool) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, false
	}
	var found *MediaEntry
	matches := 0
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, entry := range a.media {
		if entry == nil || entry.PlaceholderPath == "" {
			continue
		}
		if strings.TrimSpace(entry.Title) == title {
			matches++
			c := *entry
			found = &c
		}
	}
	if matches == 1 {
		return found, true
	}
	return nil, false
}

func normalizePlayerPath(raw string) string {
	raw = strings.TrimSpace(strings.Trim(raw, `"'`))
	if raw == "" || strings.HasPrefix(strings.ToLower(raw), "http://") || strings.HasPrefix(strings.ToLower(raw), "https://") {
		return ""
	}
	if strings.HasSuffix(raw, " (deleted)") {
		raw = strings.TrimSuffix(raw, " (deleted)")
	}
	if strings.HasPrefix(strings.ToLower(raw), "file://") {
		if u, err := url.Parse(raw); err == nil {
			raw = u.Path
		}
	}
	if strings.Contains(raw, "%") {
		if decoded, derr := url.PathUnescape(raw); derr == nil && decoded != raw && fileExists(decoded) {
			raw = decoded
		}
	}
	if idx := strings.IndexAny(raw, "?#"); idx >= 0 {
		raw = raw[:idx]
	}
	raw = filepath.Clean(raw)
	if raw == "." || raw == string(os.PathSeparator) {
		return ""
	}
	return raw
}

// playMediaEntry is the fast path used by the placeholder watcher. Unlike
// PlayStream it does not probe the remote source first: the media registry
// already knows the URL and format, and the local proxy resolves/redirects it
// when the renderer starts fetching.
func (a *App) playMediaEntry(ctx context.Context, entry *MediaEntry) error {
	if entry == nil || strings.TrimSpace(entry.URL) == "" {
		err := errors.New("媒体地址为空")
		a.setStatusError(err.Error())
		return err
	}
	title := strings.TrimSpace(entry.Title)
	if title == "" {
		title = "DISC 流媒体"
	}
	mediaType := entry.Kind
	if mediaType == "" {
		mediaType = "radio"
	}
	album := albumForType(mediaType)

	a.mu.Lock()
	a.status.Playing = false
	a.status.Type = mediaType
	a.status.Title = title
	a.status.SubTitle = entry.Subtitle
	a.status.Cover = entry.Cover
	a.status.StreamURL = entry.URL
	a.status.ProxyURL = ""
	a.status.ContentType = entry.ContentType
	a.status.RendererState = "CONNECTING"
	a.status.Error = ""
	a.status.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
	a.mu.Unlock()

	r, err := a.ensureRenderer(9 * time.Second)
	if err != nil {
		msg := "未找到播放器: " + err.Error()
		a.setStatusError(msg)
		a.setLastStreamError(msg)
		return errors.New(msg)
	}

	proxyURL := a.mediaProxyURL(entry)
	a.mu.Lock()
	a.status.ProxyURL = proxyURL
	a.mu.Unlock()

	stopCtx, stopCancel := context.WithTimeout(ctx, 3*time.Second)
	if err := a.stopRenderer(stopCtx, r); err != nil {
		a.log.Debugf("占位切换前 Stop 忽略错误: %v", err)
	}
	stopCancel()

	meta := didlMetadata(title, entry.Subtitle, album, entry.Cover, proxyURL, entry.ContentType)
	setCtx, setCancel := context.WithTimeout(ctx, 6*time.Second)
	err = a.setAVTransportURI(setCtx, r, proxyURL, meta)
	setCancel()
	if err != nil {
		retryCtx, retryCancel := context.WithTimeout(ctx, 6*time.Second)
		err = a.setAVTransportURI(retryCtx, r, proxyURL, "")
		retryCancel()
	}
	if err != nil {
		msg := "设置播放地址失败: " + err.Error()
		a.setStatusError(msg)
		a.setLastStreamError(msg)
		return errors.New(msg)
	}

	playCtx, playCancel := context.WithTimeout(ctx, 5*time.Second)
	err = a.rendererPlay(playCtx, r)
	playCancel()
	if err != nil {
		msg := "启动播放失败: " + err.Error()
		a.setStatusError(msg)
		a.setLastStreamError(msg)
		return errors.New(msg)
	}

	state, err := a.waitForPlayback(ctx, r, proxyURL)
	if err != nil {
		msg := "播放器未进入播放状态: " + err.Error()
		a.setStatusError(msg)
		a.setLastStreamError(msg)
		return errors.New(msg)
	}
	a.mu.Lock()
	a.status.Playing = true
	a.status.Error = ""
	a.status.RendererState = state
	a.status.RendererURI = proxyURL
	a.status.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
	a.mu.Unlock()
	a.log.Infof("占位文件已切换为真实流: %s / %s renderer=%s proxy=%s", title, entry.Subtitle, r.FriendlyName, proxyURL)
	return nil
}
