package main

import (
	"context"
	"crypto/sha1"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	appVersion = "1.0.4"
	appName    = "DISC Podcast & Radio"
	userAgent  = "Mozilla/5.0 (Linux; FiiO DISC) DISC-Podcast-Radio/" + appVersion
)

//go:embed web/*
var webFS embed.FS

// --- Data Models ---

type RadioStation struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Category string `json:"category"`
	Favicon  string `json:"favicon,omitempty"`
	Bitrate  int    `json:"bitrate,omitempty"`
	Codec    string `json:"codec,omitempty"`
}

type PodcastFeed struct {
	ID          string        `json:"id"`
	Title       string        `json:"title"`
	URL         string        `json:"url"`
	Description string        `json:"description"`
	Cover       string        `json:"cover"`
	Author      string        `json:"author,omitempty"`
	Episodes    []PodcastItem `json:"episodes"`
	LastUpdated string        `json:"last_updated"`
}

type PodcastSummary struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	URL          string `json:"url"`
	Description  string `json:"description"`
	Cover        string `json:"cover"`
	Author       string `json:"author,omitempty"`
	EpisodeCount int    `json:"episode_count"`
	LastUpdated  string `json:"last_updated"`
}

type PodcastItem struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	AudioURL    string `json:"audio_url"`
	PubDate     string `json:"pub_date"`
	Duration    string `json:"duration"`
	Description string `json:"description"`
	Downloaded  bool   `json:"downloaded"`
	FileSize    int64  `json:"file_size,omitempty"`
}

type PlayerStatus struct {
	Playing       bool   `json:"playing"`
	Type          string `json:"type"`
	Title         string `json:"title"`
	SubTitle      string `json:"subtitle"`
	Cover         string `json:"cover"`
	StreamURL     string `json:"stream_url"`
	ProxyURL      string `json:"proxy_url,omitempty"`
	ContentType   string `json:"content_type,omitempty"`
	Volume        int    `json:"volume"`
	UPnPPort      int    `json:"upnp_port"`
	RendererState string `json:"renderer_state,omitempty"`
	RendererURI   string `json:"renderer_uri,omitempty"`
	Error         string `json:"error,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
}

type DownloadTask struct {
	EpisodeID  string  `json:"episode_id"`
	Title      string  `json:"title"`
	Progress   float64 `json:"progress"`
	TotalBytes int64   `json:"total_bytes"`
	Written    int64   `json:"written"`
	Status     string  `json:"status"`
	Error      string  `json:"error,omitempty"`
	Path       string  `json:"path,omitempty"`
}

type MediaEntry struct {
	Token           string `json:"token"`
	Kind            string `json:"kind"`
	ID              string `json:"id"`
	URL             string `json:"url"`
	ResolvedURL     string `json:"resolved_url,omitempty"`
	Title           string `json:"title"`
	Subtitle        string `json:"subtitle,omitempty"`
	Cover           string `json:"cover,omitempty"`
	Ext             string `json:"ext"`
	ContentType     string `json:"content_type,omitempty"`
	Format          string `json:"format,omitempty"`
	PlaceholderPath string `json:"placeholder_path,omitempty"`
	Created         int64  `json:"created"`
}

type cachedFeed struct {
	feed *PodcastFeed
	at   time.Time
}

type playlistManifest struct {
	Built string   `json:"built"`
	Root  string   `json:"root"`
	Files []string `json:"files"`
}

// --- App State ---

type App struct {
	mu      sync.RWMutex
	dataDir string
	port    int
	log     *Logger

	stations  []RadioStation
	podcasts  []PodcastFeed
	downloads map[string]*DownloadTask
	status    PlayerStatus

	media map[string]*MediaEntry

	feedMu    sync.Mutex
	feedCache map[string]cachedFeed

	rendererMu      sync.RWMutex
	renderer        *RendererInfo
	rendererErr     string
	rendererChecked time.Time
	discoverMu      sync.Mutex
	settings        appSettings

	placeholderSwitching int32
	placeholderLastMu    sync.Mutex
	placeholderLastPath  string
	placeholderLastAt    time.Time
	playerPIDMu          sync.Mutex
	playerPIDs           []int
	playerPIDsAt         time.Time

	playMu          sync.Mutex
	playlistMu      sync.Mutex
	playlistRoot    string
	playlistFiles   []string
	playlistsBuilt  string
	playlistLastErr string

	client         *http.Client
	insecureClient *http.Client
	soapClient     *http.Client

	lastProbe       string
	lastStreamError string
	activeStreams   int64

	ctx    context.Context
	cancel context.CancelFunc
	start  time.Time
}

// Default radio presets. These are deliberately direct MP3 streams where
// possible: they start instantly and avoid HLS edge cases on the player.
const defaultsVersion = 2

// 1.0.0 shipped several feeds/streams that later disappeared. These URL maps
// let an upgrade replace only the old built-ins while keeping user-added
// stations and subscriptions untouched.
var legacyStationDefaults = map[string]string{
	"http://ngcdn001.cnr.cn/live/zgzs/index.m3u8":             "cnr1",
	"http://ngcdn002.cnr.cn/live/jjzs/index.m3u8":             "cnr2",
	"http://ngcdn003.cnr.cn/live/yyzs/index.m3u8":             "cnr3",
	"http://stream.live.vc.bbcmedia.co.uk/bbc_world_service":  "bbcworld",
	"https://stream.live.vc.bbcmedia.co.uk/bbc_world_service": "bbcworld",
	"http://stream.zeno.fm/f3wvbbqmdg8uv":                     "lofi",
	"https://stream.zeno.fm/f3wvbbqmdg8uv":                    "lofi",
}

var legacyPodcastURLs = map[string]bool{
	"https://feeds.fireside.fm/storyfm/rss": true,
	"https://feed.xyzfm.space/42y25528z392": true,
	"https://feed.xyzfm.space/8y7m5n04y8w9": true,
	"https://feed.xyzfm.space/6q9x5z4y1m89": true,
}

var legacyPodcastURLDefaults = map[string]string{
	"https://feeds.fireside.fm/storyfm/rss": "storyfm",
	"https://feed.xyzfm.space/42y25528z392": "leftright",
	"https://feed.xyzfm.space/8y7m5n04y8w9": "gcores",
	"https://feed.xyzfm.space/6q9x5z4y1m89": "ritan",
}

var podcastTitleAliases = map[string]string{
	"故事fm":       "storyfm",
	"机核游戏频道":     "gcores",
	"机核网 gcores": "gcores",
	"忽左忽右":       "leftright",
	"日谈公园":       "ritan",
}

type installState struct {
	Version         string `json:"version"`
	DefaultsVersion int    `json:"defaults_version"`
	UpdatedAt       string `json:"updated_at"`
}

type appSettings struct {
	RendererLocation string `json:"renderer_location,omitempty"`
}

var defaultStations = []RadioStation{
	{ID: "cnr1", Name: "中国之声 CNR-1", URL: "https://lhttp.qtfm.cn/live/15318317/64k.mp3", Category: "国家台", Bitrate: 64, Codec: "MP3"},
	{ID: "cnr2", Name: "经济之声 CNR-2", URL: "http://ngcdn002.cnr.cn/live/jjzs/index.m3u8", Category: "国家台", Codec: "HLS/AAC"},
	{ID: "cnr3", Name: "音乐之声 CNR-3", URL: "http://ngcdn003.cnr.cn/live/yyzs/index.m3u8", Category: "国家台", Codec: "HLS/AAC"},
	{ID: "gdmusic", Name: "广东音乐之声", URL: "https://lhttp.qtfm.cn/live/1260/64k.mp3", Category: "流行音乐", Bitrate: 64, Codec: "MP3"},
	{ID: "gdnews", Name: "广东新闻广播", URL: "https://lhttp.qtfm.cn/live/1254/64k.mp3", Category: "新闻资讯", Bitrate: 64, Codec: "MP3"},
	{ID: "gdprd", Name: "广东珠江经济台", URL: "https://lhttp.qtfm.cn/live/1259/64k.mp3", Category: "新闻资讯", Bitrate: 64, Codec: "MP3"},
	{ID: "bjnews", Name: "北京新闻广播", URL: "https://lhttp.qtfm.cn/live/339/64k.mp3", Category: "新闻资讯", Bitrate: 64, Codec: "MP3"},
	{ID: "shnews", Name: "上海新闻广播", URL: "http://lhttp.qingting.fm/live/270/64k.mp3", Category: "新闻资讯", Bitrate: 64, Codec: "MP3"},
	{ID: "finance1", Name: "第一财经广播", URL: "http://lhttp.qingting.fm/live/276/64k.mp3", Category: "财经", Bitrate: 64, Codec: "MP3"},
	{ID: "morning", Name: "清晨音乐台", URL: "http://lhttp.qingting.fm/live/4915/64k.mp3", Category: "轻音乐/爵士", Bitrate: 64, Codec: "MP3"},
	{ID: "lofi", Name: "Lofi Chill Beats", URL: "https://stream.zeno.fm/f3wvbbqmdg8uv", Category: "轻音乐/爵士", Codec: "MP3"},
	{ID: "radioparadise", Name: "Radio Paradise", URL: "https://stream.radioparadise.com/mp3-128", Category: "轻音乐/爵士", Bitrate: 128, Codec: "MP3"},
	{ID: "somafm", Name: "SomaFM Groove Salad", URL: "https://ice1.somafm.com/groovesalad-128-mp3", Category: "轻音乐/爵士", Bitrate: 128, Codec: "MP3"},
	{ID: "radio4", Name: "NPO Radio 4 古典", URL: "https://icecast.omroep.nl/radio4-bb-mp3", Category: "古典世界", Codec: "MP3"},
	{ID: "bbcworld", Name: "BBC World Service", URL: "https://stream.live.vc.bbcmedia.co.uk/bbc_world_service", Category: "国际新闻", Codec: "MP3"},
	{ID: "cri", Name: "CRI 中文环球广播", URL: "http://sk.cri.cn/hyhq.m3u8", Category: "国际新闻", Codec: "HLS/AAC"},
}

// Default podcasts use current, tested feed URLs. The previous 1.0.0 defaults
// all returned HTTP 404, which is why the episode screen crashed.
var defaultPodcasts = []PodcastFeed{
	{
		ID:          "storyfm",
		Title:       "故事FM",
		URL:         "https://feeds.storyfm.cn/storyfm.xml",
		Description: "用你的声音，讲述你的故事。一个亲历者口述的声音纪录片节目。",
		Author:      "爱哲",
	},
	{
		ID:          "gcores",
		Title:       "机核网 GCORES",
		URL:         "https://alioss.gcores.com/feeds/apple_podcast_v1.xml",
		Description: "机核 GCORES 游戏与流行文化电台节目。",
		Author:      "机核网",
	},
	{
		ID:          "leftright",
		Title:       "忽左忽右",
		URL:         "https://feed.xyzfm.space/cv4bkgpuglwp",
		Description: "忽左忽右，沙龙与知识播客，文化、政治、商业与历史。",
		Author:      "JustPod",
	},
	{
		ID:          "ritan",
		Title:       "日谈公园",
		URL:         "https://www.ximalaya.com/album/5574153.xml",
		Description: "畅聊电影、音乐、生活与八卦的知名脱口秀播客。",
		Author:      "李叔 & 小伙子",
	},
}

func NewApp(dataDir string, port int, logger *Logger) *App {
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{
		dataDir:   dataDir,
		port:      port,
		log:       logger,
		downloads: make(map[string]*DownloadTask),
		media:     make(map[string]*MediaEntry),
		feedCache: make(map[string]cachedFeed),
		ctx:       ctx,
		cancel:    cancel,
		start:     time.Now(),
	}
	a.status.Volume = 100
	a.client, a.insecureClient = newHTTPClients()
	a.soapClient = &http.Client{
		Timeout: 7 * time.Second,
		Transport: &http.Transport{
			DialContext:           dialIPv4First,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   7 * time.Second,
			ResponseHeaderTimeout: 7 * time.Second,
		},
	}
	return a
}

func (a *App) Init() error {
	if err := os.MkdirAll(a.dataDir, 0755); err != nil {
		return err
	}

	// Stations
	if err := loadJSON(filepath.Join(a.dataDir, "stations.json"), &a.stations); err != nil {
		a.log.Warnf("读取电台列表失败，将使用内置列表: %v", err)
	}
	if len(a.stations) == 0 {
		a.stations = cloneStations(defaultStations)
		if err := a.saveStations(); err != nil {
			a.log.Errorf("保存内置电台失败: %v", err)
		}
	}
	for i := range a.stations {
		if a.stations[i].ID == "" {
			a.stations[i].ID = "radio_" + stableHash(a.stations[i].URL, 12)
		}
		if a.stations[i].Category == "" {
			a.stations[i].Category = "未分类"
		}
	}

	// Podcasts
	if err := loadJSON(filepath.Join(a.dataDir, "podcasts.json"), &a.podcasts); err != nil {
		a.log.Warnf("读取播客列表失败，将使用内置列表: %v", err)
	}
	if len(a.podcasts) == 0 {
		a.podcasts = clonePodcasts(defaultPodcasts)
		if err := a.savePodcasts(); err != nil {
			a.log.Errorf("保存内置播客失败: %v", err)
		}
	}
	for i := range a.podcasts {
		if a.podcasts[i].ID == "" {
			a.podcasts[i].ID = "pod_" + stableHash(a.podcasts[i].URL, 12)
		}
		if a.podcasts[i].Episodes == nil {
			a.podcasts[i].Episodes = []PodcastItem{}
		}
	}

	// One-time replacement of dead 1.0.0 defaults. It only touches known old
	// built-in URLs, so user-added stations and feeds survive the upgrade.
	a.migrateDefaults()

	// Media token registry
	var stored []*MediaEntry
	if err := loadJSON(filepath.Join(a.dataDir, "media.json"), &stored); err == nil {
		for _, m := range stored {
			if m != nil && validToken(m.Token) {
				a.media[m.Token] = m
			}
		}
	}

	a.loadSettings()
	a.log.Infof("%s %s 启动，端口 %d，数据目录 %s", appName, appVersion, a.port, a.dataDir)
	a.log.Infof("运行模式: 原生流媒体屏幕UI交互模式 (免TF卡占位符I/O)，媒体登记 %d 条，电台 %d 个，播客 %d 个", len(a.media), len(a.stations), len(a.podcasts))

	// 异步安全清理历史版本可能残留在 TF 卡上的 .disc-placeholders 和历史 .m3u
	go a.cleanupLegacyPlaceholders()

	go a.discoveryLoop()
	go a.statusPollLoop()
	// 已改用流媒体屏幕UI原生点播，不再需要高频扫描fd寻找打开的静音占位符
	// go a.localPlaybackLoop()
	go a.maintenanceLoop()
	go a.startupTasks()
	go a.startOnlineIPC()
	go a.commandListenerLoop()
	return nil
}

func (a *App) migrateDefaults() {
	statePath := filepath.Join(a.dataDir, "install.json")
	var state installState
	if err := loadJSON(statePath, &state); err == nil && state.DefaultsVersion >= defaultsVersion {
		return
	}

	stationsChanged := a.mergeDefaultStations()
	podcastsChanged := a.mergeDefaultPodcasts()
	if stationsChanged {
		if err := a.saveStations(); err != nil {
			a.log.Warnf("保存升级后的电台列表失败: %v", err)
		}
	}
	if podcastsChanged {
		if err := a.savePodcasts(); err != nil {
			a.log.Warnf("保存升级后的播客列表失败: %v", err)
		}
	}
	if stationsChanged || podcastsChanged {
		a.log.Infof("已更新内置电台/播客默认源（保留自定义订阅）")
	}

	state.Version = appVersion
	state.DefaultsVersion = defaultsVersion
	state.UpdatedAt = time.Now().Format(time.RFC3339)
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	if err := writeFileAtomic(statePath, b, 0644); err != nil {
		a.log.Warnf("保存升级标记失败: %v", err)
	}
}

func (a *App) mergeDefaultStations() bool {
	byID := make(map[string]RadioStation, len(defaultStations))
	byURL := make(map[string]RadioStation, len(defaultStations))
	for _, def := range defaultStations {
		byID[def.ID] = def
		byURL[def.URL] = def
	}

	out := make([]RadioStation, 0, len(a.stations)+len(defaultStations))
	used := make(map[string]bool, len(defaultStations))
	changed := false
	for _, cur := range a.stations {
		key := strings.TrimSpace(cur.URL)
		def, ok := byID[cur.ID]
		if !ok {
			def, ok = byURL[key]
		}
		if !ok {
			if id, legacy := legacyStationDefaults[key]; legacy {
				def, ok = byID[id]
			}
		}
		if !ok {
			out = append(out, cur)
			used[cur.ID] = true
			continue
		}
		if cur.ID == def.ID && key == def.URL {
			out = append(out, cur)
			used[def.ID] = true
			continue
		}
		// A built-in ID with a custom URL is a user override; keep it.
		if _, isLegacy := legacyStationDefaults[key]; cur.ID == def.ID && key != "" && key != def.URL && !isLegacy {
			out = append(out, cur)
			used[cur.ID] = true
			continue
		}
		if used[def.ID] {
			changed = true
			continue
		}
		out = append(out, def)
		used[def.ID] = true
		changed = true
	}
	for _, def := range defaultStations {
		if !used[def.ID] {
			out = append(out, def)
			used[def.ID] = true
			changed = true
		}
	}
	a.stations = out
	return changed
}

func (a *App) mergeDefaultPodcasts() bool {
	byID := make(map[string]PodcastFeed, len(defaultPodcasts))
	byURL := make(map[string]PodcastFeed, len(defaultPodcasts))
	for _, def := range defaultPodcasts {
		byID[def.ID] = def
		byURL[def.URL] = def
	}

	out := make([]PodcastFeed, 0, len(a.podcasts)+len(defaultPodcasts))
	used := make(map[string]bool, len(defaultPodcasts))
	changed := false
	for _, cur := range a.podcasts {
		key := strings.TrimSpace(cur.URL)
		def, ok := byID[cur.ID]
		if !ok {
			def, ok = byURL[key]
		}
		if !ok {
			if id, legacy := legacyPodcastURLDefaults[key]; legacy {
				def, ok = byID[id]
			}
		}
		if ok {
			// Keep current feed data, but only replace a known old built-in or
			// an empty URL. A custom URL under a built-in ID is preserved.
			if cur.ID == def.ID && key == def.URL {
				out = append(out, cur)
				used[def.ID] = true
				continue
			}
			if cur.ID == def.ID && key != "" && !legacyPodcastURLs[key] {
				out = append(out, cur)
				used[cur.ID] = true
				continue
			}
			if used[def.ID] {
				changed = true
				continue
			}
			def.Episodes = []PodcastItem{}
			out = append(out, def)
			used[def.ID] = true
			changed = true
			continue
		}
		if legacyPodcastURLs[key] {
			alias := strings.ToLower(strings.TrimSpace(cur.Title))
			if id, found := podcastTitleAliases[alias]; found {
				if def, found = byID[id]; found {
					if !used[def.ID] {
						def.Episodes = []PodcastItem{}
						out = append(out, def)
						used[def.ID] = true
						changed = true
						continue
					}
					changed = true
					continue
				}
			}
		}
		out = append(out, cur)
		used[cur.ID] = true
	}
	for _, def := range defaultPodcasts {
		if !used[def.ID] {
			out = append(out, def)
			used[def.ID] = true
			changed = true
		}
	}
	a.podcasts = out
	return changed
}

func (a *App) startupTasks() {
	select {
	case <-a.ctx.Done():
		return
	case <-time.After(4 * time.Second):
	}

	// Capture the device-side facts that make renderer discovery debuggable
	// without SSH: listeners, process ownership, the control socket, and direct
	// UPnP probes on localhost/LAN addresses. This runs in parallel with feed
	// refresh so a slow SSDP scan does not delay the on-device playlists.
	go a.logStartupDiagnostics()

	// Populate empty default feeds in the background. This is what makes the
	// on-device M3U folders useful even if the phone UI is never opened.
	a.mu.RLock()
	feeds := append([]PodcastFeed(nil), a.podcasts...)
	a.mu.RUnlock()
	for _, f := range feeds {
		if len(f.Episodes) > 0 {
			continue
		}
		select {
		case <-a.ctx.Done():
			return
		case <-time.After(1 * time.Second):
		}
		ctx, cancel := context.WithTimeout(a.ctx, 45*time.Second)
		feed, err := a.FetchPodcastEpisodes(ctx, f.URL, true)
		cancel()
		if err != nil {
			a.log.Warnf("后台刷新内置播客《%s》失败: %v", f.Title, err)
			continue
		}
		a.log.Infof("后台刷新内置播客《%s》完成，%d 集", feed.Title, len(feed.Episodes))
	}
	a.exportEpisodesText()
	a.log.Infof("内置播客后台刷新完毕，已通过流媒体屏幕UI及Web就绪。")
}

func (a *App) commandListenerLoop() {
	cmdDir := "/tmp/disc-podcast-radio"
	cmdFile := filepath.Join(cmdDir, "play_cmd")
	_ = os.MkdirAll(cmdDir, 0755)

	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	var lastCmd string

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			data, err := os.ReadFile(cmdFile)
			if err != nil || len(data) == 0 {
				continue
			}
			text := strings.TrimSpace(string(data))
			if text == "" || text == lastCmd {
				continue
			}
			lastCmd = text
			_ = os.Remove(cmdFile)

			parts := strings.Split(text, "\t")
			if len(parts) >= 3 {
				playType := parts[0]
				title := parts[1]
				url := parts[2]
				a.log.Infof("[ScreenUI] 收到屏幕纯文字点击播放指令: type=%s title=%s url=%s", playType, title, url)
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					if err := a.PlayStream(ctx, url, title, "正在播放", "", playType); err != nil {
						a.log.Errorf("[ScreenUI] 播放失败: %v", err)
					}
				}()
			}
		}
	}
}

func (a *App) exportEpisodesText() {
	cmdDir := "/tmp/disc-podcast-radio"
	_ = os.MkdirAll(cmdDir, 0755)
	txtFile := filepath.Join(cmdDir, "episodes.txt")

	a.mu.RLock()
	podcasts := append([]PodcastFeed(nil), a.podcasts...)
	a.mu.RUnlock()

	var sb strings.Builder
	for _, f := range podcasts {
		sb.WriteString(fmt.Sprintf("[%s]\n", f.Title))
		for i, ep := range f.Episodes {
			if i >= 15 {
				break
			}
			sb.WriteString(fmt.Sprintf("%s | %s\n", ep.Title, ep.AudioURL))
		}
	}

	_ = os.WriteFile(txtFile+".next", []byte(sb.String()), 0644)
	_ = os.Rename(txtFile+".next", txtFile)
}

func (a *App) Close() {
	if a.cancel != nil {
		a.cancel()
	}
	if a.log != nil {
		a.log.Close()
	}
}

// --- Persistence ---

func (a *App) loadSettings() {
	var s appSettings
	if err := loadJSON(filepath.Join(a.dataDir, "settings.json"), &s); err != nil {
		if !os.IsNotExist(err) {
			a.log.Warnf("读取设置失败，将使用自动发现: %v", err)
		}
		return
	}
	s.RendererLocation = strings.TrimSpace(s.RendererLocation)
	a.mu.Lock()
	a.settings = s
	a.mu.Unlock()
	if s.RendererLocation != "" {
		a.log.Infof("已加载手动播放器地址: %s", s.RendererLocation)
	}
}

func (a *App) saveSettings() error {
	a.mu.RLock()
	s := a.settings
	a.mu.RUnlock()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(a.dataDir, "settings.json"), b, 0644)
}

func (a *App) rendererOverride() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return strings.TrimSpace(a.settings.RendererLocation)
}

func (a *App) setRendererOverride(location string) error {
	location = strings.TrimSpace(location)
	a.mu.Lock()
	a.settings.RendererLocation = location
	b, err := json.MarshalIndent(a.settings, "", "  ")
	a.mu.Unlock()
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(a.dataDir, "settings.json"), b, 0644)
}

func (a *App) saveStations() error {
	a.mu.RLock()
	b, err := json.MarshalIndent(a.stations, "", "  ")
	a.mu.RUnlock()
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(a.dataDir, "stations.json"), b, 0644)
}

func (a *App) persistStationsLocked() error {
	b, err := json.MarshalIndent(a.stations, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(a.dataDir, "stations.json"), b, 0644)
}

func (a *App) savePodcasts() error {
	a.mu.RLock()
	b, err := json.MarshalIndent(a.podcasts, "", "  ")
	a.mu.RUnlock()
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(a.dataDir, "podcasts.json"), b, 0644)
}

func (a *App) persistPodcastsLocked() error {
	b, err := json.MarshalIndent(a.podcasts, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(a.dataDir, "podcasts.json"), b, 0644)
}

func (a *App) saveMedia() error {
	a.mu.RLock()
	list := make([]*MediaEntry, 0, len(a.media))
	for _, m := range a.media {
		list = append(list, m)
	}
	a.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Created > list[j].Created })
	if len(list) > 5000 {
		list = list[:5000]
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(a.dataDir, "media.json"), b, 0644)
}

func (a *App) maintenanceLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			if err := a.saveMedia(); err != nil {
				a.log.Warnf("保存媒体登记失败: %v", err)
			}
		}
	}
}

// --- Helpers ---

func tfCardMounted() bool {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "/tmp/sdcard" {
			return true
		}
	}
	return false
}

func loadJSON(path string, dst interface{}) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return io.EOF
	}
	return json.Unmarshal(b, dst)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func stableHash(s string, n int) string {
	sum := sha1.Sum([]byte(s))
	h := hex.EncodeToString(sum[:])
	if n > len(h) {
		n = len(h)
	}
	return h[:n]
}

func validToken(s string) bool {
	if len(s) < 8 || len(s) > 32 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func cloneStations(in []RadioStation) []RadioStation {
	out := make([]RadioStation, len(in))
	copy(out, in)
	return out
}

func clonePodcasts(in []PodcastFeed) []PodcastFeed {
	out := make([]PodcastFeed, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Episodes = append([]PodcastItem(nil), in[i].Episodes...)
		if out[i].Episodes == nil {
			out[i].Episodes = []PodcastItem{}
		}
	}
	return out
}

func cloneFeed(in *PodcastFeed) *PodcastFeed {
	if in == nil {
		return nil
	}
	out := *in
	out.Episodes = append([]PodcastItem(nil), in.Episodes...)
	if out.Episodes == nil {
		out.Episodes = []PodcastItem{}
	}
	return &out
}

func (a *App) podcastSummaries() []PodcastSummary {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]PodcastSummary, 0, len(a.podcasts))
	for _, p := range a.podcasts {
		out = append(out, PodcastSummary{
			ID:           p.ID,
			Title:        p.Title,
			URL:          p.URL,
			Description:  p.Description,
			Cover:        p.Cover,
			Author:       p.Author,
			EpisodeCount: len(p.Episodes),
			LastUpdated:  p.LastUpdated,
		})
	}
	return out
}

func (a *App) setStatusError(msg string) {
	a.mu.Lock()
	a.status.Playing = false
	a.status.Error = msg
	a.status.RendererState = "ERROR"
	a.status.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
	a.mu.Unlock()
}

func (a *App) setLastStreamError(msg string) {
	a.mu.Lock()
	a.lastStreamError = msg
	a.mu.Unlock()
}

func (a *App) getStatus() PlayerStatus {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.status
}

// --- HTTP server ---

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (a *App) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			if v := recover(); v != nil {
				a.log.Errorf("HTTP panic %s %s: %v", r.Method, r.URL.Path, v)
				if rec.status == 0 {
					writeJSON(rec, http.StatusInternalServerError, map[string]interface{}{"success": false, "error": "服务器内部错误"})
				}
			}
			if !strings.HasPrefix(r.URL.Path, "/audio/") {
				a.log.Debugf("HTTP %s %s -> %d (%d bytes, %s)", r.Method, r.URL.Path, rec.status, rec.bytes, time.Since(start).Round(time.Millisecond))
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

func main() {
	port := flag.Int("port", 8888, "HTTP Web UI server port")
	dataDir := flag.String("data", "/usr/data/disc-runtime/module-data/disc-podcast-radio", "Data directory")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("%s %s (%s/%s)\n", appName, appVersion, runtime.GOOS, runtime.GOARCH)
		return
	}

	if err := os.MkdirAll(*dataDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "创建数据目录失败: %v\n", err)
		os.Exit(1)
	}
	logger := NewLogger(*dataDir)
	app := NewApp(*dataDir, *port, logger)
	if err := app.Init(); err != nil {
		logger.Errorf("初始化失败: %v", err)
		os.Exit(1)
	}
	defer app.Close()

	mux := http.NewServeMux()
	app.RegisterRoutes(mux)

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", *port),
		Handler:           app.middleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Streaming responses must never be cut off by a write timeout.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Infof("%s 已就绪: http://<DISC-IP>:%d  播放列表: %s", appName, *port, app.playlistRoot)
		errCh <- server.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		logger.Infof("收到信号 %v，正在退出…", sig)
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			logger.Errorf("HTTP 服务失败: %v", err)
			os.Exit(1)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

// dialIPv4First avoids waiting for broken IPv6 routes, which is common on
// embedded Wi-Fi stacks. It falls back to the system resolver if IPv4 fails.
var v4Dialer = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}

func dialIPv4First(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := v4Dialer.DialContext(ctx, "tcp4", addr)
	if err == nil {
		return c, nil
	}
	return v4Dialer.DialContext(ctx, "tcp", addr)
}
