package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// buildPlaylists creates a browsable on-device library out of M3U files:
//
//	<root>/网络电台/<分类>.m3u       one playlist per category
//	<root>/网络电台.m3u              all stations
//	<root>/播客/<订阅>.m3u           one playlist per podcast feed
//	<root>/DISC播客.m3u              latest episodes from all feeds
//
// Each M3U points at a local silent MP3 placeholder because the stock player's
// playlist importer rejects HTTP URLs. Opening a category or feed playlist on
// the player shows its stations/episodes, and the placeholder watcher switches
// AVTransport to the real /audio/<token> proxy after the user selects one.
func (a *App) detectPlaylistRoot() string {
	base := "/tmp/sdcard/流媒体下载"
	if tfCardMounted() {
		if st, err := os.Stat("/tmp/sdcard"); err == nil && st.IsDir() {
			if err := os.MkdirAll(base, 0755); err == nil {
				test := filepath.Join(base, ".disc-playlist-write-test")
				if err := os.WriteFile(test, []byte("ok"), 0644); err == nil {
					_ = os.Remove(test)
					return base
				}
			}
		}
	}
	fallback := filepath.Join(a.dataDir, "playlists")
	_ = os.MkdirAll(fallback, 0755)
	return fallback
}

func (a *App) buildPlaylists(reason string) error {
	a.playlistMu.Lock()
	defer a.playlistMu.Unlock()

	start := time.Now()
	if a.playlistRoot == "" {
		a.playlistRoot = a.detectPlaylistRoot()
	}
	if strings.HasPrefix(a.playlistRoot, "/tmp/sdcard") && !tfCardMounted() {
		err := errors.New("TF 卡未挂载，保留现有播放列表")
		a.setPlaylistError(err)
		return err
	}
	// If the service started before the card was mounted, move the generated
	// library onto the card as soon as it becomes available.
	if strings.HasPrefix(a.playlistRoot, a.dataDir) && tfCardMounted() {
		a.playlistRoot = a.detectPlaylistRoot()
	}
	root := a.playlistRoot
	if err := os.MkdirAll(root, 0755); err != nil {
		a.setPlaylistError(err)
		return err
	}

	oldManifest := a.loadPlaylistManifest()
	oldFiles := make(map[string]bool)
	for _, f := range oldManifest.Files {
		oldFiles[f] = true
	}
	newFiles := make(map[string]bool)
	used := make(map[string]bool)
	usedPlaceholders := make(map[string]bool)
	placeholderErrors := 0

	a.mu.RLock()
	stations := append([]RadioStation(nil), a.stations...)
	podcasts := clonePodcasts(a.podcasts)
	a.mu.RUnlock()

	radioRoot := filepath.Join(root, "网络电台")
	podcastRoot := filepath.Join(root, "播客")
	_ = os.MkdirAll(radioRoot, 0755)
	_ = os.MkdirAll(podcastRoot, 0755)

	var allRadio strings.Builder
	allRadio.WriteString("#EXTM3U\n")
	radioCount := 0
	byCategory := make(map[string][]RadioStation)
	for _, st := range stations {
		if strings.TrimSpace(st.URL) == "" {
			continue
		}
		cat := strings.TrimSpace(st.Category)
		if cat == "" {
			cat = "未分类"
		}
		byCategory[cat] = append(byCategory[cat], st)
	}

	categories := make([]string, 0, len(byCategory))
	for cat := range byCategory {
		categories = append(categories, cat)
	}
	sort.Strings(categories)
	for _, cat := range categories {
		var categoryPlaylist strings.Builder
		categoryPlaylist.WriteString("#EXTM3U\n")
		for _, st := range byCategory[cat] {
			entry := a.stationMedia(st)
			if entry == nil {
				continue
			}
			placeholder, err := a.ensurePlaceholder(entry, "radio", cat, st.Name)
			if err != nil {
				a.log.Warnf("生成电台占位音频失败 %s: %v", st.Name, err)
				placeholderErrors++
				continue
			}
			usedPlaceholders[filepath.Clean(placeholder)] = true
			appendM3UEntry(&categoryPlaylist, st.Name, placeholder)
			appendM3UEntry(&allRadio, st.Name, placeholder)
			radioCount++
		}
		if categoryPlaylist.Len() <= len("#EXTM3U\n") {
			continue
		}
		path := uniquePlaylistPath(radioRoot, sanitizeFilename(cat), ".m3u", used, oldFiles)
		if err := writeFileAtomic(path, []byte(categoryPlaylist.String()), 0644); err != nil {
			a.log.Warnf("写入电台分类播放列表失败 %s: %v", path, err)
			continue
		}
		newFiles[path] = true
	}

	if radioCount > 0 {
		rootRadioPath := uniquePlaylistPath(root, "网络电台", ".m3u", used, oldFiles)
		if err := writeFileAtomic(rootRadioPath, []byte(allRadio.String()), 0644); err != nil {
			a.log.Warnf("写入网络电台总列表失败: %v", err)
		} else {
			newFiles[rootRadioPath] = true
		}
	}

	// Podcasts: one M3U per subscription. Opening the feed's playlist on the
	// player shows exactly that feed's episode list.
	var latest strings.Builder
	latest.WriteString("#EXTM3U\n")
	podcastCount := 0
	for _, feed := range podcasts {
		if strings.TrimSpace(feed.Title) == "" || len(feed.Episodes) == 0 {
			continue
		}
		var feedPlaylist strings.Builder
		feedPlaylist.WriteString("#EXTM3U\n")
		limit := len(feed.Episodes)
		if limit > 60 {
			limit = 60
		}
		for _, ep := range feed.Episodes[:limit] {
			if strings.TrimSpace(ep.AudioURL) == "" {
				continue
			}
			entry := a.episodeMedia(feed, ep)
			if entry == nil {
				continue
			}
			title := strings.TrimSpace(ep.Title)
			if title == "" {
				title = "未命名单集"
			}
			if ep.PubDate != "" {
				title = ep.PubDate + " " + title
			}
			placeholder, err := a.ensurePlaceholder(entry, "podcast", feed.Title, title)
			if err != nil {
				a.log.Warnf("生成播客占位音频失败 %s/%s: %v", feed.Title, title, err)
				placeholderErrors++
				continue
			}
			usedPlaceholders[filepath.Clean(placeholder)] = true
			appendM3UEntry(&feedPlaylist, title, placeholder)
			podcastCount++
			if latest.Len() < 1<<20 {
				appendM3UEntry(&latest, feed.Title+" - "+title, placeholder)
			}
		}
		if feedPlaylist.Len() <= len("#EXTM3U\n") {
			continue
		}
		path := uniquePlaylistPath(podcastRoot, sanitizeFilename(feed.Title), ".m3u", used, oldFiles)
		if err := writeFileAtomic(path, []byte(feedPlaylist.String()), 0644); err != nil {
			a.log.Warnf("写入播客订阅播放列表失败 %s: %v", path, err)
			continue
		}
		newFiles[path] = true
	}

	if podcastCount > 0 {
		latestPath := uniquePlaylistPath(root, "DISC播客", ".m3u", used, oldFiles)
		if err := writeFileAtomic(latestPath, []byte(latest.String()), 0644); err != nil {
			a.log.Warnf("写入播客总列表失败: %v", err)
		} else {
			newFiles[latestPath] = true
		}
	}

	// Remove only files that we generated previously. Never touch user files.
	removed := 0
	for old := range oldFiles {
		if newFiles[old] {
			continue
		}
		if strings.HasSuffix(strings.ToLower(old), ".m3u") {
			if err := os.Remove(old); err == nil {
				removed++
			}
		}
	}
	removeEmptyDirs(podcastRoot, root)
	removeEmptyDirs(radioRoot, root)
	if placeholderErrors == 0 {
		a.cleanupPlaceholders(usedPlaceholders)
	}

	manifest := playlistManifest{
		Built: time.Now().Format("2006-01-02 15:04:05"),
		Root:  root,
		Files: make([]string, 0, len(newFiles)),
	}
	for f := range newFiles {
		manifest.Files = append(manifest.Files, f)
	}
	sort.Strings(manifest.Files)
	_ = a.savePlaylistManifest(manifest)
	_ = a.saveMedia()

	a.mu.Lock()
	a.playlistFiles = manifest.Files
	a.playlistsBuilt = manifest.Built
	a.playlistLastErr = ""
	a.mu.Unlock()

	a.log.Infof("播放列表已生成(%s): 电台 %d 个/播客单集 %d 个/文件 %d 个/占位音频 %d 个，删除旧文件 %d 个，耗时 %s，目录 %s",
		reason, radioCount, podcastCount, len(newFiles), len(usedPlaceholders), removed, time.Since(start).Round(time.Millisecond), root)
	return nil
}

func (a *App) setPlaylistError(err error) {
	a.mu.Lock()
	if err != nil {
		a.playlistLastErr = err.Error()
	} else {
		a.playlistLastErr = ""
	}
	a.mu.Unlock()
}

func (a *App) stationMedia(st RadioStation) *MediaEntry {
	if strings.TrimSpace(st.URL) == "" {
		return nil
	}
	if st.ID == "" {
		st.ID = "radio_" + stableHash(st.URL, 12)
	}
	ext := extFromURL(st.URL)
	ct := mimeForExt(ext)
	format := "direct"
	if looksLikeHLSURL(st.URL) {
		ext = "aac"
		ct = "audio/aac"
		format = "hls"
	}
	if ext == "" {
		ext = "mp3"
		ct = "audio/mpeg"
	}
	return a.registerMedia("radio", st.ID, st.URL, st.Name, st.Category, st.Favicon, ext, ct, format)
}

func (a *App) stationProxy(st RadioStation) string {
	return a.mediaProxyURL(a.stationMedia(st))
}

func (a *App) episodeMedia(feed PodcastFeed, ep PodcastItem) *MediaEntry {
	if strings.TrimSpace(ep.AudioURL) == "" {
		return nil
	}
	ext := extFromURL(ep.AudioURL)
	ct := mimeForExt(ext)
	format := "direct"
	if looksLikeHLSURL(ep.AudioURL) {
		ext = "aac"
		ct = "audio/aac"
		format = "hls"
	}
	if ext == "" {
		ext = "mp3"
		ct = "audio/mpeg"
	}
	return a.registerMedia("podcast", ep.ID, ep.AudioURL, ep.Title, feed.Title, feed.Cover, ext, ct, format)
}

func (a *App) episodeProxy(feed PodcastFeed, ep PodcastItem) string {
	return a.mediaProxyURL(a.episodeMedia(feed, ep))
}

func appendM3UEntry(b *strings.Builder, title, mediaURL string) {
	title = strings.ReplaceAll(title, "\r", " ")
	title = strings.ReplaceAll(title, "\n", " ")
	b.WriteString("#EXTINF:-1," + title + "\n")
	b.WriteString(mediaURL + "\n")
}

func uniquePlaylistPath(dir, base, ext string, used, oldFiles map[string]bool) string {
	if base == "" {
		base = "未命名"
	}
	candidate := filepath.Join(dir, base+ext)
	n := 2
	for used[candidate] || (fileExists(candidate) && !oldFiles[candidate]) {
		candidate = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, n, ext))
		n++
	}
	used[candidate] = true
	return candidate
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func (a *App) loadPlaylistManifest() playlistManifest {
	var m playlistManifest
	if err := loadJSON(filepath.Join(a.dataDir, "playlists.json"), &m); err != nil {
		return playlistManifest{}
	}
	return m
}

func (a *App) savePlaylistManifest(m playlistManifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(a.dataDir, "playlists.json"), b, 0644)
}

func removeEmptyDirs(dir, stop string) {
	if dir == "" || dir == stop || !strings.HasPrefix(filepath.Clean(dir), filepath.Clean(stop)) {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			removeEmptyDirs(filepath.Join(dir, e.Name()), stop)
		}
	}
	if remaining, err := os.ReadDir(dir); err == nil && len(remaining) == 0 {
		_ = os.Remove(dir)
	}
}

// cleanupLegacyPlaceholders 安全清理旧版本遗留在 TF 卡上的占位符 MP3 和历史 M3U 播放列表。
// 仅清理 .disc-placeholders 目录与 .m3u 文件，绝不触碰用户下载的真实播客音频（位于 播客/ 目录）。
func (a *App) cleanupLegacyPlaceholders() {
	if !tfCardMounted() {
		return
	}
	base := "/tmp/sdcard/流媒体下载"
	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		return
	}

	phDir := filepath.Join(base, ".disc-placeholders")
	if st, err := os.Stat(phDir); err == nil && st.IsDir() {
		a.log.Infof("[LegacyCleanup] 检测到旧版本占位音频目录，正在安全清理: %s", phDir)
		_ = os.RemoveAll(phDir)
	}

	entries, err := os.ReadDir(base)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".m3u") {
				_ = os.Remove(filepath.Join(base, e.Name()))
			}
		}
	}

	// 清理仅用于 M3U 的网络电台子目录
	radioDir := filepath.Join(base, "网络电台")
	if st, err := os.Stat(radioDir); err == nil && st.IsDir() {
		_ = os.RemoveAll(radioDir)
	}

	a.log.Infof("[LegacyCleanup] 历史 M3U 播放列表与占位符已清理完毕，已完全运行在原生流媒体屏幕UI模式。")
}
