package main

import (
	"context"
	"crypto/sha1"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	appVersion = "1.3.0"
	appName    = "DISC Podcast & Radio"
	userAgent  = "Mozilla/5.0 (Linux; FiiO DISC) DISC-Podcast-Radio/" + appVersion
)

// errPlaySuperseded is returned when a newer play request arrived before this
// one could reach the renderer. Callers treat it as "silently dropped".
var errPlaySuperseded = errors.New("播放请求已被更新的指令取代")

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
	Position      int    `json:"position"`
	Duration      int    `json:"duration"`
	PositionStr   string `json:"position_str,omitempty"`
	DurationStr   string `json:"duration_str,omitempty"`
	Error         string `json:"error,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
}

type Breakpoint struct {
	Key       string    `json:"key,omitempty"`
	StreamURL string    `json:"stream_url"`
	Title     string    `json:"title"`
	Subtitle  string    `json:"subtitle,omitempty"`
	Position  int       `json:"position"` // 秒数
	Duration  int       `json:"duration"` // 秒数
	UpdatedAt time.Time `json:"updated_at"`
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
	rendererPorts   []int
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
	playCancel      context.CancelFunc
	playCancelMu    sync.Mutex
	playSeq         int64
	playlistMu      sync.Mutex
	playlistRoot    string
	playlistFiles   []string
	playlistsBuilt  string
	playlistLastErr string

	client         *http.Client
	insecureClient *http.Client
	soapClient     *http.Client

	lastProbe         string
	lastStreamError   string
	activeStreams     int32
	playInFlight      int32
	mediaSaveMu       sync.Mutex
	currentCoverBytes []byte
	currentCoverMu    sync.RWMutex

	breakpointMu sync.RWMutex
	breakpoints  map[string]Breakpoint
	bpDirty      bool

	seekMu          sync.Mutex
	lastSeekSec     int
	lastSeekAt      time.Time
	lastFeedRefresh time.Time

	session playbackSession
	pcm     pcmClock

	seekCheck seekCheckState

	ctx    context.Context
	cancel context.CancelFunc
	start  time.Time
}

// Default radio presets. These are deliberately direct MP3 streams where
// possible: they start instantly and avoid HLS edge cases on the player.
const defaultsVersion = 3

// 1.0.0 shipped several feeds/streams that later disappeared. These URL maps
// let an upgrade replace only the old built-ins while keeping user-added
// stations and subscriptions untouched.
var legacyStationDefaults = map[string]string{
	"http://ngcdn001.cnr.cn/live/zgzs/index.m3u8":             "cnr1",
	"http://ngcdn002.cnr.cn/live/jjzs/index.m3u8":             "cnr2",
	"http://ngcdn003.cnr.cn/live/yyzs/index.m3u8":             "cnr3",
	"http://stream.live.vc.bbcmedia.co.uk/bbc_world_service":  "bbcworld",
	"https://stream.live.vc.bbcmedia.co.uk/bbc_world_service": "bbcworld",
	"http://stream.zeno.fm/f3wvbbqmdg8uv":                     "oldies",
	"https://stream.zeno.fm/f3wvbbqmdg8uv":                    "oldies",
}

var legacyPodcastURLs = map[string]bool{
	"https://feeds.fireside.fm/storyfm/rss": true,
	"https://feeds.storyfm.cn/storyfm.xml":  true,
	"https://feed.xyzfm.space/42y25528z392": true,
	"https://feed.xyzfm.space/8y7m5n04y8w9": true,
	"https://feed.xyzfm.space/6q9x5z4y1m89": true,
}

var legacyPodcastURLDefaults = map[string]string{
	"https://feeds.fireside.fm/storyfm/rss": "buhsy",
	"https://feeds.storyfm.cn/storyfm.xml":  "buhsy",
	"https://feed.xyzfm.space/42y25528z392": "leftright",
	"https://feed.xyzfm.space/8y7m5n04y8w9": "gcores",
	"https://feed.xyzfm.space/6q9x5z4y1m89": "ritan",
}

var podcastTitleAliases = map[string]string{
	"故事fm":       "buhsy",
	"不合时宜":       "buhsy",
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
	{ID: "oldies", Name: "怀旧金曲", URL: "http://lhttp.qingting.fm/live/4917/64k.mp3", Category: "轻音乐/爵士", Bitrate: 64, Codec: "MP3"},
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
		ID:          "buhsy",
		Title:       "不合时宜",
		URL:         "https://feed.xyzfm.space/ww7cqnybekty",
		Cover:       "https://image.xyzcdn.net/Fq-ccfNzhgTqQNzlG-1qA_jOow7l.png",
		Description: "以跨文化视野与敏锐触角，探讨当下青年关注的生活叙事与公共议题。",
		Author:      "若含 & 孟常 & 王磬",
	},
	{
		ID:          "gcores",
		Title:       "机核网 GCORES",
		URL:         "https://alioss.gcores.com/feeds/apple_podcast_v1.xml",
		Cover:       "https://alioss.gcores.com/download/podcast/gadio.png?v=1",
		Description: "机核 GCORES 游戏与流行文化电台节目。",
		Author:      "机核网",
	},
	{
		ID:          "leftright",
		Title:       "忽左忽右",
		URL:         "https://feed.xyzfm.space/cv4bkgpuglwp",
		Cover:       "https://image.xyzcdn.net/FrYMqb37Wg_LA8aenDJJ8SFGsjea",
		Description: "忽左忽右，沙龙与知识播客，文化、政治、商业与历史。",
		Author:      "JustPod",
	},
	{
		ID:          "ritan",
		Title:       "日谈公园",
		URL:         "https://www.ximalaya.com/album/5574153.xml",
		Cover:       "https://fdfs.xmcdn.com/group53/M08/04/7F/wKgLfFxFi1DyK_sOAAIVVH_yP2g776.jpg",
		Description: "畅聊电影、音乐、生活与八卦的知名脱口秀播客。",
		Author:      "李叔 & 小伙子",
	},
}

func NewApp(dataDir string, port int, logger *Logger) *App {
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{
		dataDir:     dataDir,
		port:        port,
		log:         logger,
		downloads:   make(map[string]*DownloadTask),
		media:       make(map[string]*MediaEntry),
		feedCache:   make(map[string]cachedFeed),
		breakpoints: make(map[string]Breakpoint),
		ctx:         ctx,
		cancel:      cancel,
		start:       time.Now(),
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

	// Podcasts. Remember whether this is a fresh install before the defaults
	// file is created: on a fresh install a TF-card list is authoritative,
	// while on an upgrade the already-persisted online list must win.
	podcastsPath := filepath.Join(a.dataDir, "podcasts.json")
	_, podcastsStatErr := os.Stat(podcastsPath)
	firstRun := os.IsNotExist(podcastsStatErr)
	if err := loadJSON(podcastsPath, &a.podcasts); err != nil {
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
	a.exportEpisodesText()

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
	a.loadBreakpoints()
	a.log.Infof("%s %s 启动，端口 %d，数据目录 %s", appName, appVersion, a.port, a.dataDir)
	a.log.Infof("运行模式: 原生流媒体屏幕UI交互模式 (免TF卡占位符I/O)，媒体登记 %d 条，电台 %d 个，播客 %d 个", len(a.media), len(a.stations), len(a.podcasts))

	// 异步安全清理历史版本可能残留在 TF 卡上的 .disc-placeholders 和历史 .m3u
	go a.cleanupLegacyPlaceholders()

	// 初始化 TF 卡订阅及电台文件同步 (podcasts.txt / radios.txt)
	a.initTFCardSync(firstRun)
	go a.tfCardWatcherLoop()

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
		if !ok && cur.ID == "lofi" {
			def, ok = byID["oldies"]
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
		if !ok && cur.ID == "storyfm" {
			def, ok = byID["buhsy"]
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

func (a *App) hasNetworkIP() bool {
	for _, ip := range a.localIPv4List() {
		if ip != "127.0.0.1" && !strings.HasPrefix(ip, "127.") {
			return true
		}
	}
	return false
}

func (a *App) startupTasks() {
	select {
	case <-a.ctx.Done():
		return
	case <-time.After(3 * time.Second):
	}

	go a.logStartupDiagnostics()

	// 阶段一：保底填充无单集的空白订阅（如首次启动或通过 TF 卡新添加但未拉取的播客）
	for attempt := 0; attempt < 3; attempt++ {
		a.mu.RLock()
		feeds := append([]PodcastFeed(nil), a.podcasts...)
		a.mu.RUnlock()

		hasMissing := false
		for _, f := range feeds {
			if len(f.Episodes) > 0 {
				continue
			}
			hasMissing = true
			select {
			case <-a.ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
			feed, err := a.FetchPodcastEpisodes(ctx, f.URL, true)
			cancel()
			if err != nil {
				a.log.Warnf("[Startup] 补充拉取空白播客《%s》失败: %v", f.Title, err)
				continue
			}
			a.log.Infof("[Startup] 补充拉取空白播客《%s》完成，共 %d 集", feed.Title, len(feed.Episodes))
		}
		if hasMissing {
			a.exportEpisodesText()
			a.syncPodcastsToTF()
			a.log.Infof("[Startup] 空白播客单集补充完毕，已实时导出屏幕选单与持久化快照")
		}
		if !hasMissing {
			break
		}
		select {
		case <-a.ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}

	// 阶段二：后台网络监控与常驻增量同步
	// 无论开机初期网络是否就绪，持续后台监测，在 Wi-Fi 建立连接后立即拉取，随后每20分钟定时增量同步
	go func() {
		for {
			if a.hasNetworkIP() {
				break
			}
			select {
			case <-a.ctx.Done():
				return
			case <-time.After(4 * time.Second):
			}
		}
		a.log.Infof("[StartupRefresh] 检测到 Wi-Fi 网络已连接，执行首次播客更新...")
		a.refreshSubscribedFeeds()

		ticker := time.NewTicker(20 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-a.ctx.Done():
				return
			case <-ticker.C:
				if a.hasNetworkIP() {
					a.refreshSubscribedFeeds()
				}
			}
		}
	}()
}

func (a *App) refreshSubscribedFeeds() {
	if !a.hasNetworkIP() {
		return
	}
	a.mu.RLock()
	feeds := append([]PodcastFeed(nil), a.podcasts...)
	a.mu.RUnlock()

	if len(feeds) == 0 {
		return
	}

	a.log.Infof("[FeedSync] 开始执行播客增量刷新（共 %d 个订阅源）...", len(feeds))
	var updatedCount int

	for _, f := range feeds {
		if strings.HasPrefix(f.URL, "http://127.0.0.1") {
			continue
		}
		select {
		case <-a.ctx.Done():
			return
		case <-time.After(1000 * time.Millisecond): // 节奏控制：每个订阅间隔 1 秒
		}

		oldLen := len(f.Episodes)
		var oldTopTitle string
		if oldLen > 0 {
			oldTopTitle = f.Episodes[0].Title
		}

		ctx, cancel := context.WithTimeout(a.ctx, 25*time.Second)
		feed, err := a.FetchPodcastEpisodes(ctx, f.URL, true)
		cancel()

		if err != nil {
			a.log.Warnf("[FeedSync] 播客《%s》增量刷新失败 (保留本地旧数据): %v", f.Title, err)
			continue
		}

		newLen := len(feed.Episodes)
		var newTopTitle string
		if newLen > 0 {
			newTopTitle = feed.Episodes[0].Title
		}

		if newLen != oldLen || newTopTitle != oldTopTitle {
			a.log.Infof("[FeedSync] 播客《%s》发现新节目更新: %s (原 %d 集 -> 现 %d 集)", feed.Title, newTopTitle, oldLen, newLen)
			updatedCount++
		} else {
			a.log.Infof("[FeedSync] 播客《%s》已是最新内容 (%d 集)", feed.Title, newLen)
		}
	}

	a.lastFeedRefresh = time.Now()
	if updatedCount > 0 {
		a.exportEpisodesText()
		a.log.Infof("[FeedSync] 播客增量更新完毕: %d 个订阅发现新节目，屏幕选单已同步更新。", updatedCount)

		// if the screen is currently showing a podcast list via IPC,
		// push fresh data immediately so the user sees up-to-date content
		ipcMgr.mu.Lock()
		activeSource := ipcMgr.activeSource
		activeView := ipcMgr.activeView
		activeFeedID := ipcMgr.activeFeedID
		ipcMgr.mu.Unlock()

		switch {
		case activeSource == "podcast" && activeView == "feeds":
			go ipcMgr.handlePodcastAccount("refresh")
		case activeSource == "podcast" && activeView == "episodes" && activeFeedID != "":
			go ipcMgr.handlePodcastTracks("refresh", activeFeedID)
		}
	} else {
		a.log.Infof("[FeedSync] 播客增量更新完毕: 所有订阅内容均已是最新状态。")
	}
}

func (a *App) commandListenerLoop() {
	cmdDir := "/tmp/disc-podcast-radio"
	cmdFile := filepath.Join(cmdDir, "play_cmd")
	seekFile := filepath.Join(cmdDir, "seek_cmd")
	_ = os.MkdirAll(cmdDir, 0755)

	// 250ms keeps tap latency imperceptible while cutting the stat() rate by
	// 40% versus the old 150ms loop. A partially written command is still
	// retried (maxParseRetries * 250ms = 2s) before it is dropped.
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	// The UI signals every tap by writing this file, including a second tap on
	// the same episode, so there is deliberately no "same content twice" filter
	// here: the file itself is the event. A partially written command is retried
	// for a few ticks before it is dropped.
	const maxParseRetries = 8
	retries := 0

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			// 检查屏幕 UI 触发的主动刷新播客单集指令
			refreshFile := filepath.Join(cmdDir, "refresh_requested")
			if _, rerr := os.Stat(refreshFile); rerr == nil {
				_ = os.Remove(refreshFile)
				a.log.Infof("[ScreenUI] 收到屏幕主动刷新请求")
				if a.hasNetworkIP() && time.Since(a.lastFeedRefresh) > 30*time.Second {
					go a.refreshSubscribedFeeds()
				}
			}

			// 优先检查屏幕 UI 点击跳转进度指令
			if seekData, serr := os.ReadFile(seekFile); serr == nil && len(seekData) > 0 {
				_ = os.Remove(seekFile)
				seekText := strings.TrimSpace(string(seekData))
				if seekText != "" {
					sec := parseTimeSeconds(seekText)
					if sec >= 0 {
						a.log.Infof("[ScreenUI] 收到进度跳转指令: %s (%d 秒)", seekText, sec)
						go func(targetSec int) {
							if err := a.Seek(targetSec); err != nil {
								a.log.Warnf("[ScreenUI] 执行跳转失败: %v", err)
							}
						}(sec)
					}
				}
			}

			data, err := os.ReadFile(cmdFile)
			if err != nil || len(data) == 0 {
				retries = 0
				continue
			}
			text := strings.TrimSpace(string(data))
			if text == "" {
				continue
			}
			parts := strings.Split(text, "\t")
			if len(parts) < 3 {
				retries++
				if retries < maxParseRetries {
					continue
				}
				a.log.Warnf("[ScreenUI] 播放指令格式无效，已丢弃: %q", text)
				_ = os.Remove(cmdFile)
				retries = 0
				continue
			}
			retries = 0
			if err := os.Remove(cmdFile); err != nil && !os.IsNotExist(err) {
				a.log.Debugf("[ScreenUI] 删除播放指令文件失败: %v", err)
			}

			playType := parts[0]
			title := parts[1]
			url := parts[2]
			artist := ""
			if len(parts) >= 4 {
				artist = parts[3]
			}
			if artist == "" {
				if playType == "podcast" {
					artist = "播客"
				} else {
					artist = "网络电台"
				}
			}
			cover := a.findCoverForPlay(playType, title, artist, url)
			a.log.Infof("[ScreenUI] 收到屏幕播放指令: type=%s title=%s artist=%s url=%s cover=%s", playType, title, artist, url, cover)

			ctx, cancel, gen := a.beginPlay(25 * time.Second)
			go func(ctx context.Context, gen int64, pType, pTitle, pArtist, pURL, pCover string, cancel context.CancelFunc) {
				defer cancel()
				err := a.playNow(ctx, gen, pURL, pTitle, pArtist, pCover, pType)
				if err == nil {
					return
				}
				if errors.Is(err, errPlaySuperseded) {
					a.log.Infof("[ScreenUI] 播放已由后续新指令取消: %s", pTitle)
					return
				}
				if ctx.Err() == nil {
					a.log.Errorf("[ScreenUI] 播放失败: %v", err)
				} else {
					a.log.Infof("[ScreenUI] 播放已由后续新指令取消: %s", pTitle)
				}
			}(ctx, gen, playType, title, artist, url, cover, cancel)
		}
	}
}

// beginPlay registers a new playback generation. Any in-flight playback is
// cancelled and the returned context is cancelled as soon as a newer request
// arrives, so a slow discovery cannot keep running after the user moved on.
func (a *App) beginPlay(timeout time.Duration) (context.Context, context.CancelFunc, int64) {
	a.playCancelMu.Lock()
	if a.playCancel != nil {
		a.playCancel()
	}
	ctx, cancel := context.WithTimeout(a.ctx, timeout)
	a.playSeq++
	gen := a.playSeq
	a.playCancel = cancel
	a.playCancelMu.Unlock()
	return ctx, cancel, gen
}

func (a *App) playIsCurrent(gen int64) bool {
	a.playCancelMu.Lock()
	defer a.playCancelMu.Unlock()
	return a.playSeq == gen
}

func (a *App) currentPlayGen() int64 {
	a.playCancelMu.Lock()
	defer a.playCancelMu.Unlock()
	return a.playSeq
}

// playNow serialises PlayStream behind playMu and drops the request when a
// newer command superseded it while it was queued.
func (a *App) playNow(ctx context.Context, gen int64, streamURL, title, subtitle, cover, mediaType string) error {
	a.playMu.Lock()
	defer a.playMu.Unlock()
	if !a.playIsCurrent(gen) {
		return errPlaySuperseded
	}
	if err := ctx.Err(); err != nil {
		return errPlaySuperseded
	}
	atomic.StoreInt32(&a.playInFlight, 1)
	defer atomic.StoreInt32(&a.playInFlight, 0)
	return a.PlayStream(ctx, streamURL, title, subtitle, cover, mediaType)
}

// playInBackground starts a playback request that participates in the same
// cancel/preempt chain as screen taps and HTTP calls, so two callers can never
// mutate renderer state at the same time.
func (a *App) playInBackground(streamURL, title, subtitle, cover, mediaType, tag string) {
	ctx, cancel, gen := a.beginPlay(30 * time.Second)
	go func() {
		defer cancel()
		err := a.playNow(ctx, gen, streamURL, title, subtitle, cover, mediaType)
		switch {
		case err == nil:
		case errors.Is(err, errPlaySuperseded):
			a.log.Infof("[%s] 播放已由更新的指令取代: %s", tag, title)
		default:
			a.log.Errorf("[%s] 播放失败: %s (%v)", tag, title, err)
		}
	}()
}

func (a *App) findCoverForPlay(playType, title, artist, streamURL string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if playType == "podcast" {
		lowerArtist := strings.ToLower(strings.TrimSpace(artist))
		for _, p := range a.podcasts {
			if strings.EqualFold(p.Title, artist) || strings.EqualFold(p.ID, artist) {
				if p.Cover != "" {
					return p.Cover
				}
			}
		}
		if id, ok := podcastTitleAliases[lowerArtist]; ok {
			for _, p := range a.podcasts {
				if p.ID == id && p.Cover != "" {
					return p.Cover
				}
			}
		}
		for _, p := range a.podcasts {
			if strings.Contains(strings.ToLower(p.Title), lowerArtist) || strings.Contains(lowerArtist, strings.ToLower(p.Title)) {
				if p.Cover != "" {
					return p.Cover
				}
			}
		}
		for _, p := range defaultPodcasts {
			if strings.Contains(strings.ToLower(p.Title), lowerArtist) || strings.Contains(lowerArtist, strings.ToLower(p.Title)) {
				if p.Cover != "" {
					return p.Cover
				}
			}
		}
	} else {
		for _, s := range a.stations {
			if strings.EqualFold(s.Name, title) || s.URL == streamURL {
				if s.Favicon != "" {
					return s.Favicon
				}
			}
		}
	}
	return ""
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
		if strings.TrimSpace(f.Title) == "" {
			continue
		}
		sb.WriteString(fmt.Sprintf("[%s]\n", strings.TrimSpace(f.Title)))
		for i, ep := range f.Episodes {
			if i >= 50 {
				break
			}
			cleanTitle := strings.ReplaceAll(ep.Title, "|", "-")
			cleanTitle = strings.ReplaceAll(cleanTitle, "\t", " ")
			cleanTitle = strings.ReplaceAll(cleanTitle, "\n", " ")
			cleanTitle = strings.TrimSpace(cleanTitle)
			cleanURL := strings.TrimSpace(ep.AudioURL)
			if cleanTitle != "" && cleanURL != "" {
				sb.WriteString(fmt.Sprintf("%s | %s\n", cleanTitle, cleanURL))
			}
		}
	}

	_ = os.WriteFile(txtFile+".next", []byte(sb.String()), 0644)
	_ = os.Rename(txtFile+".next", txtFile)
	a.persistUIMenuSnapshot("episodes.txt", []byte(sb.String()))
}

// persistUIMenuSnapshot stores a copy of a screen-UI text menu on NAND.
//
// /tmp is a tmpfs: every reboot wipes /tmp/disc-podcast-radio/episodes.txt and
// radios.txt. Until the on-demand daemon writes them again, module-ui.so falls
// back to the demo list compiled into it (忽左忽右 505-510, 客栈里的武林…),
// which users see as "the old list came back after a reboot". The guard module
// copies this snapshot back into /tmp at boot, before the first menu render.
func (a *App) persistUIMenuSnapshot(name string, data []byte) {
	if err := writeFileAtomic(filepath.Join(a.dataDir, name), data, 0644); err != nil && a.log != nil {
		a.log.Warnf("保存屏幕选单快照 %s 失败: %v", name, err)
	}
}

func (a *App) Close() {
	a.captureCurrentBreakpointSync("shutdown")
	a.session.stop()
	if a.cancel != nil {
		a.cancel()
	}
	_ = a.saveBreakpoints()
	_ = os.Remove("/tmp/disc-podcast-radio/playback_state")
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
	a.mediaSaveMu.Lock()
	defer a.mediaSaveMu.Unlock()

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
			if err := a.saveBreakpoints(); err != nil {
				a.log.Warnf("保存播客断点记忆失败: %v", err)
			}
		}
	}
}

// --- Breakpoints & Playback Progress ---

func (a *App) loadBreakpoints() {
	var m map[string]Breakpoint
	path := filepath.Join(a.dataDir, "breakpoints.json")
	if err := loadJSON(path, &m); err != nil {
		if !os.IsNotExist(err) {
			a.log.Warnf("读取断点记录失败: %v", err)
		} else {
			a.log.Infof("未找到播客断点文件，将在首次播放后创建: %s", path)
		}
		return
	}
	if m == nil {
		m = make(map[string]Breakpoint)
	}
	a.breakpointMu.Lock()
	a.breakpoints = m
	a.bpDirty = false
	a.breakpointMu.Unlock()
	a.log.Infof("已载入 %d 条播客断点记忆", len(m))
}

func (a *App) saveBreakpoints() error {
	a.breakpointMu.Lock()
	if !a.bpDirty {
		a.breakpointMu.Unlock()
		return nil
	}
	copyMap := make(map[string]Breakpoint, len(a.breakpoints))
	for k, v := range a.breakpoints {
		copyMap[k] = v
	}
	a.bpDirty = false
	a.breakpointMu.Unlock()

	b, err := json.MarshalIndent(copyMap, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(a.dataDir, "breakpoints.json"), b, 0644)
}

// getBreakpoint accepts optional title/subtitle hints. Old files were keyed by
// the exact stream URL; new files also carry a stable episode key and a
// canonical URL without expiring CDN signature parameters.
func (a *App) getBreakpoint(streamURL string, hints ...string) (Breakpoint, bool) {
	title, subtitle := "", ""
	if len(hints) > 0 {
		title = hints[0]
	}
	if len(hints) > 1 {
		subtitle = hints[1]
	}
	canonical := canonicalMediaKey(streamURL)
	episode := episodeBreakpointKey(title, subtitle)

	a.breakpointMu.RLock()
	defer a.breakpointMu.RUnlock()
	for _, key := range []string{streamURL, canonical, episode} {
		if key == "" {
			continue
		}
		if bp, ok := a.breakpoints[key]; ok {
			return bp, true
		}
	}
	for _, bp := range a.breakpoints {
		if canonical != "" && canonicalMediaKey(bp.StreamURL) == canonical {
			return bp, true
		}
		if episode != "" && episodeBreakpointKey(bp.Title, bp.Subtitle) == episode {
			return bp, true
		}
	}
	return Breakpoint{}, false
}

func (a *App) setBreakpoint(streamURL, title string, pos, dur int, hints ...string) {
	if streamURL == "" || pos < 5 {
		return
	}
	subtitle := ""
	if len(hints) > 0 {
		subtitle = hints[0]
	}
	key := episodeBreakpointKey(title, subtitle)
	if key == "" {
		key = canonicalMediaKey(streamURL)
	}
	if key == "" {
		return
	}
	bp := Breakpoint{
		Key:       key,
		StreamURL: streamURL,
		Title:     title,
		Subtitle:  subtitle,
		Position:  pos,
		Duration:  dur,
		UpdatedAt: time.Now(),
	}
	a.breakpointMu.Lock()
	a.breakpoints[key] = bp
	if canonical := canonicalMediaKey(streamURL); canonical != "" && canonical != key {
		a.breakpoints[canonical] = bp
	}
	a.bpDirty = true
	a.breakpointMu.Unlock()
}

func (a *App) clearBreakpoint(streamURL string, hints ...string) {
	title, subtitle := "", ""
	if len(hints) > 0 {
		title = hints[0]
	}
	if len(hints) > 1 {
		subtitle = hints[1]
	}
	canonical := canonicalMediaKey(streamURL)
	episode := episodeBreakpointKey(title, subtitle)

	a.breakpointMu.Lock()
	changed := false
	for key, bp := range a.breakpoints {
		if key == streamURL || key == canonical || key == episode ||
			(canonical != "" && canonicalMediaKey(bp.StreamURL) == canonical) ||
			(episode != "" && episodeBreakpointKey(bp.Title, bp.Subtitle) == episode) {
			delete(a.breakpoints, key)
			changed = true
		}
	}
	if changed {
		a.bpDirty = true
	}
	a.breakpointMu.Unlock()
}

// captureCurrentBreakpoint persists the in-memory position before a stream is
// replaced, stopped, or the process exits. Without this, switching episodes
// loses everything recorded since the last 15-second poll.
func (a *App) storeCurrentBreakpoint(reason string) bool {
	sess := a.session.snapshot()
	if !sess.Active || sess.MediaType != "podcast" || sess.StreamURL == "" {
		return false
	}
	pos := a.session.estimate(time.Now(), !sess.Paused)
	if pos < 5 {
		return false
	}
	if durationReachedEnd(pos, sess.Duration) {
		a.clearBreakpoint(sess.StreamURL, sess.Title, sess.Subtitle)
		return true
	}
	a.setBreakpoint(sess.StreamURL, sess.Title, pos, sess.Duration, sess.Subtitle)
	a.log.Infof("[Breakpoint] 保存断点 (%s): %s / %s -> %s", reason, sess.Title, sess.Subtitle, formatSeconds(pos))
	return true
}

func (a *App) captureCurrentBreakpoint(reason string) {
	if a.storeCurrentBreakpoint(reason) {
		go func() {
			if err := a.saveBreakpoints(); err != nil {
				a.log.Warnf("保存播客断点记忆失败: %v", err)
			}
		}()
	}
}

func (a *App) captureCurrentBreakpointSync(reason string) {
	if a.storeCurrentBreakpoint(reason) {
		if err := a.saveBreakpoints(); err != nil {
			a.log.Warnf("保存播客断点记忆失败: %v", err)
		}
	}
}

func parseTimeSeconds(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	parts := strings.Split(s, ":")
	if len(parts) == 3 {
		h, _ := strconv.Atoi(parts[0])
		m, _ := strconv.Atoi(parts[1])
		sec, _ := strconv.Atoi(strings.Split(parts[2], ".")[0])
		return h*3600 + m*60 + sec
	}
	if len(parts) == 2 {
		m, _ := strconv.Atoi(parts[0])
		sec, _ := strconv.Atoi(strings.Split(parts[1], ".")[0])
		return m*60 + sec
	}
	return 0
}

func formatSeconds(sec int) string {
	if sec < 0 {
		sec = 0
	}
	h := sec / 3600
	m := (sec % 3600) / 60
	s := sec % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

// formatRendererSeekTarget formats seconds into a Target string for mq_player's UPnP Seek.
// mq_player's UPnP Seek handler parses "%d:%02d:%02d" into total_sec = (h*3600 + m*60 + s),
// and its internal seek thread (0x5e1f14) ALREADY multiplies total_sec by 1000
// (sll 5, subu, sll 2, addu, sll 3 == * 125 * 8 == * 1000) before passing milliseconds
// to AudioCodecSeek in libfiio_decoder.so.
// Therefore, standard UPnP REL_TIME "HH:MM:SS" (in seconds) must be sent!
func formatRendererSeekTarget(sec int) string {
	if sec <= 0 {
		return "00:00:00"
	}
	h := sec / 3600
	m := (sec % 3600) / 60
	s := sec % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

func formatDisplayTime(sec int) string {
	if sec <= 0 {
		return "00:00"
	}
	h := sec / 3600
	m := (sec % 3600) / 60
	s := sec % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

func (a *App) broadcastPlaybackState(pType string, pos, dur int, playing bool) {
	dir := "/tmp/disc-podcast-radio"
	_ = os.MkdirAll(dir, 0755)
	stateFile := filepath.Join(dir, "playback_state")
	tmpFile := stateFile + ".tmp"

	posStr := formatDisplayTime(pos)
	durStr := formatDisplayTime(dur)
	if dur <= 0 {
		durStr = "--:--"
	}
	playFlag := 0
	if playing {
		playFlag = 1
	}

	line := fmt.Sprintf("%s\t%d\t%d\t%s\t%s\t%d\n", pType, pos, dur, posStr, durStr, playFlag)
	if err := os.WriteFile(tmpFile, []byte(line), 0644); err == nil {
		_ = os.Rename(tmpFile, stateFile)
	}
}

func (a *App) Seek(targetSeconds int) error {
	if targetSeconds < 0 {
		targetSeconds = 0
	}
	a.seekMu.Lock()
	defer a.seekMu.Unlock()

	now := time.Now()
	if a.lastSeekSec == targetSeconds && now.Sub(a.lastSeekAt) < 400*time.Millisecond {
		a.log.Debugf("[Seek] 忽略 400ms 内重复跳转目标: %d 秒", targetSeconds)
		return nil
	}
	a.lastSeekSec = targetSeconds
	a.lastSeekAt = now

	r := a.currentRenderer()
	if r == nil {
		ctx, cancel := context.WithTimeout(a.ctx, 4*time.Second)
		defer cancel()
		var err error
		r, err = a.ensureRenderer(ctx, 3*time.Second)
		if err != nil {
			return fmt.Errorf("播放器未就绪: %w", err)
		}
	}
	targetStr := formatRendererSeekTarget(targetSeconds)
	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
	defer cancel()
	a.log.Infof("执行播放进度跳转: %s (%d 秒, UPnP REL_TIME: %s) -> %s", formatDisplayTime(targetSeconds), targetSeconds, targetStr, r.ControlURL)
	// Arm the byte-offset verifier before the SOAP call: mq_player reopens the
	// proxied stream about 0.5s after a seek, and that Range start is the only
	// reliable proof of where it actually landed.
	a.seekCheck.begin(targetSeconds)
	if err := a.rendererSeek(ctx, r, targetStr); err != nil {
		a.seekCheck.cancel()
		return fmt.Errorf("跳转进度失败: %w", err)
	}
	a.pcm.setSeekTarget(targetSeconds)
	a.mu.Lock()
	a.status.Position = targetSeconds
	a.status.PositionStr = formatDisplayTime(targetSeconds)
	pType := a.status.Type
	dur := a.status.Duration
	playing := a.status.Playing
	a.mu.Unlock()
	a.session.update(targetSeconds, dur, time.Now())
	a.broadcastPlaybackState(pType, targetSeconds, dur, playing)
	go a.confirmSeek()
	return nil
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

	readyFile := "/tmp/disc-podcast-radio/ready"
	_ = os.WriteFile(readyFile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0644)
	defer os.Remove(readyFile)

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

func init() {
	// Embedded Linux on FiiO DISC has IPv6 completely disabled in the kernel.
	// Force Go resolver to query over IPv4, and fallback to 223.5.5.5:53 / 119.29.29.29:53 directly
	// when local dnsmasq ([::1]:53 or 127.0.0.1:53) is not yet forwarding upstream DNS.
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 4 * time.Second}
			if strings.HasPrefix(address, "[::1]") || strings.Contains(address, "::") || strings.HasPrefix(address, "127.0.0.1") {
				conn, err := d.DialContext(ctx, "udp4", "223.5.5.5:53")
				if err == nil {
					return conn, nil
				}
				conn, err = d.DialContext(ctx, "udp4", "119.29.29.29:53")
				if err == nil {
					return conn, nil
				}
				address = "127.0.0.1:53"
			}
			host, _, err := net.SplitHostPort(address)
			if err == nil {
				if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
					address = "223.5.5.5:53"
				}
			}
			return d.DialContext(ctx, "udp4", address)
		},
	}
}

// dialIPv4First strictly forces IPv4 connections on the embedded system.
var v4Dialer = &net.Dialer{Timeout: 4 * time.Second, KeepAlive: 30 * time.Second}

func dialIPv4First(ctx context.Context, network, addr string) (net.Conn, error) {
	return v4Dialer.DialContext(ctx, "tcp4", addr)
}
