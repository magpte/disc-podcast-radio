package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

type statusResponse struct {
	PlayerStatus
	RendererFound bool          `json:"renderer_found"`
	RendererError string        `json:"renderer_error,omitempty"`
	Renderer      *RendererInfo `json:"renderer,omitempty"`
	Playlists     string        `json:"playlists_built,omitempty"`
}

func (a *App) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/status", a.handleStatus)
	mux.HandleFunc("/api/player/play", a.handlePlay)
	mux.HandleFunc("/api/player/stop", a.handleStop)
	mux.HandleFunc("/api/player/volume", a.handleVolume)
	mux.HandleFunc("/api/radio/list", a.handleRadioList)
	mux.HandleFunc("/api/radio/add", a.handleRadioAdd)
	mux.HandleFunc("/api/radio/delete", a.handleRadioDelete)
	mux.HandleFunc("/api/radio/search", a.handleRadioSearch)
	mux.HandleFunc("/api/podcast/list", a.handlePodcastList)
	mux.HandleFunc("/api/podcast/subscribe", a.handlePodcastSubscribe)
	mux.HandleFunc("/api/podcast/episodes", a.handlePodcastEpisodes)
	mux.HandleFunc("/api/podcast/delete", a.handlePodcastDelete)
	mux.HandleFunc("/api/podcast/download", a.handleDownload)
	mux.HandleFunc("/api/downloads", a.handleDownloads)
	mux.HandleFunc("/api/logs", a.handleLogs)
	mux.HandleFunc("/api/logs/clear", a.handleLogsClear)
	mux.HandleFunc("/api/debug", a.handleDebug)
	mux.HandleFunc("/api/renderer/discover", a.handleRendererDiscover)
	mux.HandleFunc("/api/renderer/wake", a.handleRendererWake)
	mux.HandleFunc("/api/renderer", a.handleRenderer)
	mux.HandleFunc("/api/renderer/probe", a.handleRendererProbe)
	mux.HandleFunc("/api/diagnostics/renderer", a.handleRendererProbe)
	mux.HandleFunc("/api/diagnostics/player-db", a.handlePlayerDB)
	mux.HandleFunc("/api/playlists", a.handlePlaylists)
	mux.HandleFunc("/api/playlists/build", a.handlePlaylistsBuild)
	mux.HandleFunc("/api/health", a.handleHealth)
	mux.HandleFunc("/api/version", a.handleVersion)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{"success": false, "error": "接口不存在: " + r.URL.Path})
	})
	mux.HandleFunc("/audio/", a.handleAudio)
	mux.HandleFunc("/", a.handleStatic)
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func readJSON(r *http.Request, dst interface{}) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	return dec.Decode(dst)
}

func onlyMethods(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, m := range methods {
		if r.Method == m {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"success": false, "error": "不支持的请求方法"})
	return false
}

// --- Status / player ---

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodGet) {
		return
	}
	st := a.getStatus()
	rd, rerr, _ := a.rendererSnapshot()
	a.mu.RLock()
	built := a.playlistsBuilt
	a.mu.RUnlock()
	writeJSON(w, http.StatusOK, statusResponse{
		PlayerStatus:  st,
		RendererFound: rd != nil,
		RendererError: rerr,
		Renderer:      rd,
		Playlists:     built,
	})
}

func (a *App) handlePlay(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodPost) {
		return
	}
	var body struct {
		URL       string `json:"url"`
		StreamURL string `json:"stream_url"`
		Title     string `json:"title"`
		Subtitle  string `json:"subtitle"`
		Cover     string `json:"cover"`
		Type      string `json:"type"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "请求 JSON 无效: " + err.Error()})
		return
	}
	streamURL := strings.TrimSpace(body.URL)
	if streamURL == "" {
		streamURL = strings.TrimSpace(body.StreamURL)
	}
	if streamURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "缺少音频地址 url"})
		return
	}

	a.playMu.Lock()
	defer a.playMu.Unlock()
	ctx, cancel := context.WithTimeout(a.ctx, 55*time.Second)
	defer cancel()
	if err := a.PlayStream(ctx, streamURL, body.Title, body.Subtitle, body.Cover, body.Type); err != nil {
		a.log.Warnf("播放请求失败: %s (%v)", streamURL, err)
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{"success": false, "error": err.Error(), "status": a.getStatus()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "status": a.getStatus()})
}

func (a *App) handleStop(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodPost) {
		return
	}
	a.playMu.Lock()
	defer a.playMu.Unlock()
	if err := a.StopPlay(); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "status": a.getStatus()})
}

func (a *App) handleVolume(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodPost) {
		return
	}
	var body struct {
		Volume int `json:"volume"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "请求 JSON 无效"})
		return
	}
	if err := a.SetVolume(body.Volume); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "volume": body.Volume})
}

// --- Radio ---

func (a *App) handleRadioList(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodGet) {
		return
	}
	a.mu.RLock()
	out := append([]RadioStation(nil), a.stations...)
	a.mu.RUnlock()
	if out == nil {
		out = []RadioStation{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleRadioAdd(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodPost) {
		return
	}
	var body struct {
		Name     string `json:"name"`
		URL      string `json:"url"`
		Category string `json:"category"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "请求 JSON 无效"})
		return
	}
	body.URL = strings.TrimSpace(body.URL)
	body.Name = strings.TrimSpace(body.Name)
	body.Category = strings.TrimSpace(body.Category)
	if err := validateHTTPURL(body.URL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	if body.Name == "" {
		body.Name = "自定义电台"
	}
	if body.Category == "" {
		body.Category = "自定义"
	}

	var station RadioStation
	a.mu.Lock()
	for i := range a.stations {
		if a.stations[i].URL == body.URL {
			a.stations[i].Name = body.Name
			a.stations[i].Category = body.Category
			station = a.stations[i]
			_ = a.persistStationsLocked()
			a.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "station": station, "updated": true})
			return
		}
	}
	station = RadioStation{
		ID:       "radio_" + stableHash(body.URL, 12),
		Name:     body.Name,
		URL:      body.URL,
		Category: body.Category,
	}
	a.stations = append(a.stations, station)
	_ = a.persistStationsLocked()
	a.mu.Unlock()
	a.log.Infof("添加电台: %s (%s) %s", station.Name, station.Category, station.URL)
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "station": station, "updated": false})
}

func (a *App) handleRadioDelete(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodPost) {
		return
	}
	var body struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "请求 JSON 无效"})
		return
	}
	removed := false
	a.mu.Lock()
	out := a.stations[:0]
	for _, st := range a.stations {
		if (body.ID != "" && st.ID == body.ID) || (body.URL != "" && st.URL == body.URL) {
			removed = true
			continue
		}
		out = append(out, st)
	}
	a.stations = out
	if removed {
		_ = a.persistStationsLocked()
	}
	a.mu.Unlock()
	if !removed {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{"success": false, "error": "没有找到该电台"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

func (a *App) handleRadioSearch(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodGet) {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, []RadioStation{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	stations, err := a.SearchRadioBrowser(ctx, q)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{"success": false, "error": err.Error(), "results": []RadioStation{}})
		return
	}
	if stations == nil {
		stations = []RadioStation{}
	}
	writeJSON(w, http.StatusOK, stations)
}

// --- Podcasts ---

func (a *App) handlePodcastList(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodGet) {
		return
	}
	if r.URL.Query().Get("full") == "1" {
		a.mu.RLock()
		out := clonePodcasts(a.podcasts)
		a.mu.RUnlock()
		writeJSON(w, http.StatusOK, out)
		return
	}
	out := a.podcastSummaries()
	if out == nil {
		out = []PodcastSummary{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handlePodcastSubscribe(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodPost) {
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "请求 JSON 无效"})
		return
	}
	body.URL = strings.TrimSpace(body.URL)
	if err := validateHTTPURL(body.URL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	feed, err := a.FetchPodcastEpisodes(ctx, body.URL, true)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	if feed.ID == "" {
		feed.ID = "pod_" + stableHash(feed.URL, 12)
	}
	a.mu.Lock()
	found := false
	for i := range a.podcasts {
		if a.podcasts[i].URL == body.URL {
			feed.ID = a.podcasts[i].ID
			if feed.ID == "" {
				feed.ID = "pod_" + stableHash(body.URL, 12)
			}
			a.podcasts[i] = *feed
			found = true
			break
		}
	}
	if !found {
		a.podcasts = append(a.podcasts, *feed)
	}
	_ = a.persistPodcastsLocked()
	a.mu.Unlock()
	a.log.Infof("订阅播客: 《%s》 %s (%d 集)", feed.Title, feed.URL, len(feed.Episodes))
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"podcast": PodcastSummary{
			ID: feed.ID, Title: feed.Title, URL: feed.URL, Description: feed.Description,
			Cover: feed.Cover, Author: feed.Author, EpisodeCount: len(feed.Episodes), LastUpdated: feed.LastUpdated,
		},
	})
}

func (a *App) handlePodcastEpisodes(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodGet) {
		return
	}
	feedURL := strings.TrimSpace(r.URL.Query().Get("url"))
	if feedURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "缺少订阅地址 url", "episodes": []PodcastItem{}})
		return
	}
	force := r.URL.Query().Get("refresh") == "1"
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	feed, err := a.FetchPodcastEpisodes(ctx, feedURL, force)
	if err != nil {
		a.log.Warnf("读取播客单集失败 %s: %v", feedURL, err)
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{
			"success":  false,
			"error":    err.Error(),
			"episodes": []PodcastItem{},
		})
		return
	}
	if feed.Episodes == nil {
		feed.Episodes = []PodcastItem{}
	}
	writeJSON(w, http.StatusOK, feed)
}

func (a *App) handlePodcastDelete(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodPost) {
		return
	}
	var body struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "请求 JSON 无效"})
		return
	}
	removed := false
	a.mu.Lock()
	out := a.podcasts[:0]
	for _, p := range a.podcasts {
		if (body.ID != "" && p.ID == body.ID) || (body.URL != "" && p.URL == body.URL) {
			removed = true
			continue
		}
		out = append(out, p)
	}
	a.podcasts = out
	if removed {
		_ = a.persistPodcastsLocked()
	}
	a.mu.Unlock()
	if !removed {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{"success": false, "error": "没有找到该订阅"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

func (a *App) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodPost) {
		return
	}
	var body struct {
		EpisodeID    string `json:"episode_id"`
		Title        string `json:"title"`
		AudioURL     string `json:"audio_url"`
		PodcastTitle string `json:"podcast_title"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "请求 JSON 无效"})
		return
	}
	if err := validateHTTPURL(body.AudioURL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	if body.PodcastTitle == "" {
		body.PodcastTitle = "未命名播客"
	}
	if body.Title == "" {
		body.Title = "未命名单集"
	}
	a.DownloadEpisode(body.PodcastTitle, body.Title, body.AudioURL, body.EpisodeID)
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

func (a *App) handleDownloads(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodGet) {
		return
	}
	a.mu.RLock()
	out := make(map[string]DownloadTask, len(a.downloads))
	for k, v := range a.downloads {
		out[k] = *v
	}
	a.mu.RUnlock()
	writeJSON(w, http.StatusOK, out)
}

// --- Logs / debug ---

func (a *App) handleLogs(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodGet) {
		return
	}
	limit := 300
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := parseInt(v); err == nil && n > 0 && n <= 2000 {
			limit = n
		}
	}
	level := r.URL.Query().Get("level")
	entries := a.log.Entries(limit, level)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"entries": entries,
		"count":   len(entries),
		"file":    a.log.File(),
	})
}

func (a *App) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodPost) {
		return
	}
	a.log.Clear()
	a.log.Infof("日志缓冲区已清空")
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

func (a *App) handleDebug(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodGet) {
		return
	}
	st := a.getStatus()
	renderer, rerr, checked := a.rendererSnapshot()
	a.mu.RLock()
	stationCount := len(a.stations)
	mediaCount := len(a.media)
	downloadCount := len(a.downloads)
	lastProbe := a.lastProbe
	lastStreamError := a.lastStreamError
	playlistFiles := append([]string(nil), a.playlistFiles...)
	playlistsBuilt := a.playlistsBuilt
	playlistError := a.playlistLastErr
	a.mu.RUnlock()

	feedCache := make(map[string]interface{})
	a.feedMu.Lock()
	for k, v := range a.feedCache {
		feedCache[k] = map[string]interface{}{
			"title":     v.feed.Title,
			"episodes":  len(v.feed.Episodes),
			"cached_at": v.at.Format("2006-01-02 15:04:05"),
		}
	}
	a.feedMu.Unlock()

	playlists := playlistFiles
	if len(playlists) > 120 {
		playlists = playlists[:120]
	}
	out := map[string]interface{}{
		"version":             appVersion,
		"go_version":          runtime.Version(),
		"started_at":          a.start.Format("2006-01-02 15:04:05"),
		"uptime_seconds":      int64(time.Since(a.start).Seconds()),
		"data_dir":            a.dataDir,
		"port":                a.port,
		"log_file":            a.log.File(),
		"goroutines":          runtime.NumGoroutine(),
		"status":              st,
		"renderer_found":      renderer != nil,
		"renderer":            renderer,
		"renderer_error":      rerr,
		"renderer_checked":    checked.Format("2006-01-02 15:04:05"),
		"renderer_override":   a.rendererOverride(),
		"placeholder_root":    a.placeholderRoot(),
		"placeholder_count":   a.placeholderCount(),
		"stations":            stationCount,
		"podcasts":            a.podcastSummaries(),
		"feed_cache":          feedCache,
		"media_tokens":        mediaCount,
		"downloads":           downloadCount,
		"active_streams":      atomic.LoadInt64(&a.activeStreams),
		"last_probe":          lastProbe,
		"last_stream_error":   lastStreamError,
		"playlist_root":       a.playlistRoot,
		"playlists_built":     playlistsBuilt,
		"playlist_error":      playlistError,
		"playlist_file_count": len(playlistFiles),
		"playlist_files":      playlists,
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleRenderer(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		renderer, rerr, checked := a.rendererSnapshot()
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"success":           renderer != nil,
			"renderer":          renderer,
			"renderer_error":    rerr,
			"renderer_checked":  checked.Format("2006-01-02 15:04:05"),
			"renderer_override": a.rendererOverride(),
		})
	case http.MethodPost:
		var body struct {
			Location string `json:"location"`
			URL      string `json:"url"`
		}
		if err := readJSON(r, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "请求 JSON 无效: " + err.Error()})
			return
		}
		location := strings.TrimSpace(body.Location)
		if location == "" {
			location = strings.TrimSpace(body.URL)
		}
		if location == "" {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "缺少播放器地址 location"})
			return
		}
		if !strings.Contains(location, "://") {
			location = "http://" + location
		}
		if err := validateHTTPURL(location); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": err.Error()})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		candidates := []rendererCandidate{{URL: location, Source: "manual", Timeout: 3 * time.Second, Priority: 0}}
		rd, attempts := a.probeRendererCandidates(ctx, candidates, false)
		cancel()
		if rd == nil {
			writeJSON(w, http.StatusBadGateway, map[string]interface{}{
				"success":  false,
				"error":    "无法连接该播放器地址: " + rendererAttemptSummary(attempts, location),
				"attempts": attempts,
			})
			return
		}
		if err := a.setRendererOverride(location); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "error": "保存设置失败: " + err.Error()})
			return
		}
		a.setRenderer(rd)
		a.log.Infof("已保存手动播放器地址: %s (%s)", location, rd.ControlURL)
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "renderer": rd, "renderer_override": location})
	case http.MethodDelete:
		if err := a.setRendererOverride(""); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "error": "保存设置失败: " + err.Error()})
			return
		}
		a.log.Infof("已清除手动播放器地址，恢复自动发现")
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "renderer_override": ""})
	default:
		onlyMethods(w, r, http.MethodGet, http.MethodPost, http.MethodDelete)
	}
}

func (a *App) handleRendererProbe(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodGet) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 16*time.Second)
	defer cancel()
	d := a.runRendererDiagnostics(ctx)
	a.saveRendererDiagnostics(d)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":        d.Renderer != nil,
		"renderer_found": d.Renderer != nil,
		"diagnostics":    d,
		"renderer":       d.Renderer,
		"renderer_error": d.RendererError,
	})
}

func (a *App) handlePlayerDB(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodGet) {
		return
	}
	info := a.exportPlayerDB()
	if r.URL.Query().Get("download") == "1" {
		if info.CopiedTo == "" {
			writeJSON(w, http.StatusNotFound, map[string]interface{}{"success": false, "error": info.Error, "database": info})
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="song.db"`)
		http.ServeFile(w, r, info.CopiedTo)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": info.Exists, "database": info})
}

func (a *App) handleRendererDiscover(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodPost) {
		return
	}
	// Attempt to wake mq_player into DLNA mode before scanning
	if err := a.wakePlayerDLNA(); err != nil {
		a.log.Warnf("handleRendererDiscover 唤醒 mq_player: %v", err)
	}
	time.Sleep(600 * time.Millisecond)

	ctx, cancel := context.WithTimeout(r.Context(), 14*time.Second)
	defer cancel()
	rd, err := a.discoverRenderer(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{"success": false, "error": err.Error(), "renderer": a.currentRenderer()})
		return
	}
	a.setRenderer(rd)
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "renderer": rd})
}

func (a *App) handleRendererWake(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodPost, http.MethodGet) {
		return
	}
	err := a.wakePlayerDLNA()
	time.Sleep(700 * time.Millisecond)
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	rd, discErr := a.discoverRenderer(ctx)
	if rd != nil {
		a.setRenderer(rd)
	}
	var discErrStr string
	if discErr != nil {
		discErrStr = discErr.Error()
	}
	var wakeErrStr string
	if err != nil {
		wakeErrStr = err.Error()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":        rd != nil || err == nil,
		"wake_sent":      err == nil,
		"wake_error":     wakeErrStr,
		"renderer_found": rd != nil,
		"renderer":       rd,
		"discover_error": discErrStr,
		"mqueue_player":  checkMQueuePlayer(),
	})
}

func (a *App) handlePlaylists(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodGet) {
		return
	}
	a.mu.RLock()
	files := append([]string(nil), a.playlistFiles...)
	built := a.playlistsBuilt
	lastErr := a.playlistLastErr
	a.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"root":              a.playlistRoot,
		"built":             built,
		"count":             len(files),
		"files":             files,
		"error":             lastErr,
		"download_dir":      a.downloadDir(),
		"placeholder_root":  a.placeholderRoot(),
		"placeholder_count": a.placeholderCount(),
	})
}

func (a *App) handlePlaylistsBuild(w http.ResponseWriter, r *http.Request) {
	if !onlyMethods(w, r, http.MethodPost) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "已完全切换为原生流媒体屏幕UI模式，无需向TF卡生成M3U播放列表与占位音频",
		"count":   0,
	})
}

func (a *App) rebuildPlaylistsSafe(reason string) {
	// 已切换为流媒体原生UI选择模式，避免向脆弱的 FAT32 TF 卡频繁写入占位符与播放列表
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "version": appVersion, "uptime_seconds": int64(time.Since(a.start).Seconds())})
}

func (a *App) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"name": appName, "version": appVersion, "go": runtime.Version()})
}

func parseInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n, err
}

// --- Static web UI ---

func (a *App) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	clean := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
	if strings.Contains(clean, "..") {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(clean, "/")
	if name == "" || name == "." {
		name = "index.html"
	}
	if name == "favicon.ico" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	data, err := webFS.ReadFile("web/" + name)
	if err != nil {
		if !strings.Contains(name, ".") {
			name = "index.html"
			data, err = webFS.ReadFile("web/index.html")
		}
		if err != nil {
			http.NotFound(w, r)
			return
		}
	}
	ext := strings.ToLower(path.Ext(name))
	ct := mime.TypeByExtension(ext)
	switch ext {
	case ".html":
		ct = "text/html; charset=utf-8"
	case ".js":
		ct = "application/javascript; charset=utf-8"
	case ".css":
		ct = "text/css; charset=utf-8"
	case ".svg":
		ct = "image/svg+xml"
	case ".json":
		ct = "application/json; charset=utf-8"
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	if ext == ".html" || ext == ".js" || ext == ".css" {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}
