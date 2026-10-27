package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	onlineMusicDir = "/tmp/disc-online-music"
	onlineReqFile  = "/tmp/disc-online-music/request"
	onlineRespFile = "/tmp/disc-online-music/response"
	onlineNowFile  = "/tmp/disc-online-music/nowplaying"
)

type OnlineItem struct {
	ID       string
	Title    string
	Subtitle string
	Type     string // "feed", "episode", "station"
	URL      string
	Cover    string
	FeedID   string
}

type OnlineIPCManager struct {
	app          *App
	mu           sync.Mutex
	activeSource string // "podcast", "radio"
	activeView   string // "feeds", "episodes", "stations"
	activeFeedID string
	currentItems []OnlineItem
	lastReqText  string
	revision     int
}

var ipcMgr = &OnlineIPCManager{}

func (a *App) startOnlineIPC() {
	ipcMgr.app = a
	_ = os.MkdirAll(onlineMusicDir, 0755)

	// Keep nowPlayingLoop for updating nowplaying info, but avoid stealing /tmp/disc-online-music/request
	// as pure text menu uses /tmp/disc-podcast-radio/play_cmd directly.
	go ipcMgr.nowPlayingLoop()
}

func (mgr *OnlineIPCManager) requestLoop() {
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-mgr.app.ctx.Done():
			return
		case <-ticker.C:
			mgr.pollRequest()
		}
	}
}

func (mgr *OnlineIPCManager) pollRequest() {
	data, err := os.ReadFile(onlineReqFile)
	if err != nil || len(data) == 0 {
		return
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return
	}

	mgr.mu.Lock()
	if text == mgr.lastReqText {
		mgr.mu.Unlock()
		return
	}
	mgr.lastReqText = text
	mgr.mu.Unlock()

	mgr.handleRequest(text)
}

func (mgr *OnlineIPCManager) handleRequest(req string) {
	// Format: <seq>\t<action>\t<source_or_param>[\t<param2>...]
	parts := strings.Split(req, "\t")
	if len(parts) < 2 {
		return
	}

	seq := parts[0]
	action := parts[1]
	sourceOrParam := ""
	if len(parts) > 2 {
		sourceOrParam = parts[2]
	}

	mgr.app.log.Infof("[OnlineIPC] 收到屏幕指令: seq=%s action=%s param=%s (全量: %q)", seq, action, sourceOrParam, req)

	switch action {
	case "account":
		// Initial enter on clicking a provider
		if sourceOrParam == "podcast" {
			mgr.handlePodcastAccount(seq)
		} else if sourceOrParam == "radio" {
			mgr.handleRadioAccount(seq)
		}

	case "tracks":
		// User requested a list
		if len(parts) > 3 {
			source := parts[2]
			id := parts[3]
			if source == "podcast" {
				mgr.handlePodcastTracks(seq, id)
			} else if source == "radio" {
				mgr.handleRadioAccount(seq)
			}
		} else if mgr.activeSource == "podcast" {
			mgr.handlePodcastTracks(seq, sourceOrParam)
		} else if mgr.activeSource == "radio" {
			mgr.handleRadioAccount(seq)
		}

	case "search":
		// User searched via onscreen keyboard
		if len(parts) > 3 {
			source := parts[2]
			query := parts[3]
			if source == "podcast" || mgr.activeSource == "podcast" {
				mgr.handlePodcastSearch(seq, query)
			} else if source == "radio" || mgr.activeSource == "radio" {
				mgr.handleRadioSearch(seq, query)
			}
		}

	case "play":
		// Format: <seq>\tplay\t<page>\t<index>
		if len(parts) >= 4 {
			idxStr := parts[3]
			idx, err := strconv.Atoi(idxStr)
			if err == nil {
				mgr.handlePlayIndex(idx)
			}
		}
	}
}

func (mgr *OnlineIPCManager) handleRadioAccount(seq string) {
	mgr.mu.Lock()
	mgr.activeSource = "radio"
	mgr.activeView = "stations"
	mgr.currentItems = nil

	mgr.app.mu.RLock()
	stations := cloneStations(mgr.app.stations)
	mgr.app.mu.RUnlock()

	for _, st := range stations {
		mgr.currentItems = append(mgr.currentItems, OnlineItem{
			ID:       st.ID,
			Title:    st.Name,
			Subtitle: fmt.Sprintf("%s · %s", st.Category, st.Codec),
			Type:     "station",
			URL:      st.URL,
			Cover:    st.Favicon,
		})
	}
	items := make([]OnlineItem, len(mgr.currentItems))
	copy(items, mgr.currentItems)
	mgr.mu.Unlock()

	mgr.publishList(seq, fmt.Sprintf("%d 个电台", len(items)), items)
}

func (mgr *OnlineIPCManager) handlePodcastAccount(seq string) {
	mgr.mu.Lock()
	mgr.activeSource = "podcast"
	mgr.activeView = "feeds"
	mgr.currentItems = nil

	mgr.app.mu.RLock()
	feeds := clonePodcasts(mgr.app.podcasts)
	mgr.app.mu.RUnlock()

	for _, f := range feeds {
		epCount := len(f.Episodes)
		sub := f.Author
		if sub == "" {
			sub = f.Description
		}
		if epCount > 0 {
			sub = fmt.Sprintf("%s (%d集)", sub, epCount)
		}
		mgr.currentItems = append(mgr.currentItems, OnlineItem{
			ID:       f.ID,
			Title:    f.Title,
			Subtitle: sub,
			Type:     "feed",
			URL:      f.URL,
			Cover:    f.Cover,
			FeedID:   f.ID,
		})
	}
	items := make([]OnlineItem, len(mgr.currentItems))
	copy(items, mgr.currentItems)
	mgr.mu.Unlock()

	mgr.publishList(seq, fmt.Sprintf("%d 个播客", len(items)), items)
}

func (mgr *OnlineIPCManager) handlePodcastTracks(seq string, feedID string) {
	mgr.mu.Lock()
	mgr.activeSource = "podcast"
	mgr.activeView = "episodes"
	mgr.activeFeedID = feedID

	var targetFeed *PodcastFeed
	mgr.app.mu.RLock()
	for i := range mgr.app.podcasts {
		if mgr.app.podcasts[i].ID == feedID || mgr.app.podcasts[i].Title == feedID {
			f := mgr.app.podcasts[i]
			targetFeed = &f
			break
		}
	}
	mgr.app.mu.RUnlock()

	if targetFeed == nil {
		mgr.mu.Unlock()
		mgr.handlePodcastAccount(seq)
		return
	}

	mgr.currentItems = nil
	for _, ep := range targetFeed.Episodes {
		mgr.currentItems = append(mgr.currentItems, OnlineItem{
			ID:       ep.ID,
			Title:    ep.Title,
			Subtitle: fmt.Sprintf("%s · %s", ep.PubDate, ep.Duration),
			Type:     "episode",
			URL:      ep.AudioURL,
			Cover:    targetFeed.Cover,
			FeedID:   targetFeed.ID,
		})
	}
	items := make([]OnlineItem, len(mgr.currentItems))
	copy(items, mgr.currentItems)
	mgr.mu.Unlock()

	title := targetFeed.Title
	if len(items) > 0 {
		title = fmt.Sprintf("%s (%d集)", targetFeed.Title, len(items))
	}
	mgr.publishList(seq, title, items)
}

func (mgr *OnlineIPCManager) handlePodcastSearch(seq string, query string) {
	mgr.mu.Lock()
	mgr.activeSource = "podcast"
	mgr.activeView = "search"
	mgr.currentItems = nil

	queryLower := strings.ToLower(query)
	mgr.app.mu.RLock()
	for _, f := range mgr.app.podcasts {
		if strings.Contains(strings.ToLower(f.Title), queryLower) || strings.Contains(strings.ToLower(f.Description), queryLower) {
			mgr.currentItems = append(mgr.currentItems, OnlineItem{
				ID:       f.ID,
				Title:    f.Title,
				Subtitle: f.Author,
				Type:     "feed",
				URL:      f.URL,
				Cover:    f.Cover,
				FeedID:   f.ID,
			})
		}
		for _, ep := range f.Episodes {
			if strings.Contains(strings.ToLower(ep.Title), queryLower) {
				mgr.currentItems = append(mgr.currentItems, OnlineItem{
					ID:       ep.ID,
					Title:    ep.Title,
					Subtitle: fmt.Sprintf("%s · %s", f.Title, ep.PubDate),
					Type:     "episode",
					URL:      ep.AudioURL,
					Cover:    f.Cover,
					FeedID:   f.ID,
				})
			}
		}
	}
	mgr.app.mu.RUnlock()

	items := make([]OnlineItem, len(mgr.currentItems))
	copy(items, mgr.currentItems)
	mgr.mu.Unlock()

	mgr.publishList(seq, fmt.Sprintf("搜索: %s (%d条)", query, len(items)), items)
}

func (mgr *OnlineIPCManager) handleRadioSearch(seq string, query string) {
	mgr.mu.Lock()
	mgr.activeSource = "radio"
	mgr.activeView = "search"
	mgr.currentItems = nil

	queryLower := strings.ToLower(query)
	mgr.app.mu.RLock()
	for _, st := range mgr.app.stations {
		if strings.Contains(strings.ToLower(st.Name), queryLower) || strings.Contains(strings.ToLower(st.Category), queryLower) {
			mgr.currentItems = append(mgr.currentItems, OnlineItem{
				ID:       st.ID,
				Title:    st.Name,
				Subtitle: fmt.Sprintf("%s · %s", st.Category, st.Codec),
				Type:     "station",
				URL:      st.URL,
				Cover:    st.Favicon,
			})
		}
	}
	mgr.app.mu.RUnlock()

	items := make([]OnlineItem, len(mgr.currentItems))
	copy(items, mgr.currentItems)
	mgr.mu.Unlock()

	mgr.publishList(seq, fmt.Sprintf("电台搜索: %s (%d条)", query, len(items)), items)
}

func (mgr *OnlineIPCManager) handlePlayIndex(idx int) {
	mgr.mu.Lock()
	if idx < 0 || idx >= len(mgr.currentItems) {
		mgr.mu.Unlock()
		return
	}
	item := mgr.currentItems[idx]
	view := mgr.activeView
	mgr.mu.Unlock()

	mgr.app.log.Infof("[OnlineIPC] 用户在屏幕点击序号 %d: Title=%s Type=%s", idx, item.Title, item.Type)

	if item.Type == "station" {
		go func() {
			if err := mgr.app.PlayStream(context.Background(), item.URL, item.Title, item.Subtitle, item.Cover, "radio"); err != nil {
				mgr.app.log.Errorf("[OnlineIPC] 播放网络电台失败: %v", err)
			}
		}()
	} else if item.Type == "feed" {
		// User tapped a podcast feed. If in feed list, load its episodes and start playing latest episode
		mgr.app.mu.RLock()
		var feed *PodcastFeed
		for i := range mgr.app.podcasts {
			if mgr.app.podcasts[i].ID == item.FeedID {
				f := mgr.app.podcasts[i]
				feed = &f
				break
			}
		}
		mgr.app.mu.RUnlock()

		if feed != nil && len(feed.Episodes) > 0 {
			ep := feed.Episodes[0]
			go func() {
				if err := mgr.app.PlayStream(context.Background(), ep.AudioURL, ep.Title, feed.Title, feed.Cover, "podcast"); err != nil {
					mgr.app.log.Errorf("[OnlineIPC] 播放播客首集失败: %v", err)
				}
			}()
			// Switch screen view to episode list
			mgr.handlePodcastTracks("0", feed.ID)
		}
	} else if item.Type == "episode" {
		go func() {
			if err := mgr.app.PlayStream(context.Background(), item.URL, item.Title, item.Subtitle, item.Cover, "podcast"); err != nil {
				mgr.app.log.Errorf("[OnlineIPC] 播放播客单集失败: %v", err)
			}
		}()
	}
	_ = view
}

func (mgr *OnlineIPCManager) publishList(seq string, title string, items []OnlineItem) {
	var buf bytes.Buffer
	// Header: <seq>\ttracks\t<title>\n
	buf.WriteString(fmt.Sprintf("%s\ttracks\t%s\n", seq, title))
	// Pagination / Meta: @\t<total>\tfalse\t0\t0\t0\n
	buf.WriteString(fmt.Sprintf("@\t%d\tfalse\t0\t0\t0\n", len(items)))

	for _, it := range items {
		// Clean tabs and newlines from titles to avoid breaking parser
		cleanTitle := strings.ReplaceAll(strings.ReplaceAll(it.Title, "\t", " "), "\n", " ")
		cleanSub := strings.ReplaceAll(strings.ReplaceAll(it.Subtitle, "\t", " "), "\n", " ")
		buf.WriteString(fmt.Sprintf("%s\t%s\n", cleanTitle, cleanSub))
	}

	nextFile := onlineRespFile + ".next"
	if err := os.WriteFile(nextFile, buf.Bytes(), 0644); err != nil {
		mgr.app.log.Errorf("[OnlineIPC] 写入 response.next 失败: %v", err)
		return
	}
	if err := os.Rename(nextFile, onlineRespFile); err != nil {
		mgr.app.log.Errorf("[OnlineIPC] 重命名为 response 失败: %v", err)
		return
	}
	mgr.app.log.Infof("[OnlineIPC] 成功发布列表到屏幕 (条目数: %d, 标题: %s)", len(items), title)
}

func (mgr *OnlineIPCManager) nowPlayingLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-mgr.app.ctx.Done():
			return
		case <-ticker.C:
			mgr.updateNowPlaying()
		}
	}
}

func (mgr *OnlineIPCManager) updateNowPlaying() {
	st := mgr.app.getStatus()
	if !st.Playing && st.RendererState == "" {
		return
	}

	mgr.mu.Lock()
	mgr.revision++
	rev := mgr.revision
	mgr.mu.Unlock()

	stateStr := "STOPPED"
	if st.Playing {
		stateStr = "PLAYING"
	} else if st.RendererState == "PAUSED_PLAYBACK" {
		stateStr = "PAUSED"
	}

	title := st.Title
	if title == "" {
		title = "DISC 播客与收音机"
	}
	subtitle := st.SubTitle
	if subtitle == "" {
		subtitle = "正在播放"
	}

	var buf bytes.Buffer
	// Format: <rev>\t<PLAYING/PAUSED/STOPPED>\tmp3\t<elapsed>\t<duration>\t<volume>\t0\t0\t0\t0\n
	buf.WriteString(fmt.Sprintf("%d\t%s\tmp3\t0\t0\t%d\t0\t0\t0\t0\n", rev, stateStr, st.Volume))
	buf.WriteString(fmt.Sprintf("%s\n", strings.ReplaceAll(title, "\n", " ")))
	buf.WriteString(fmt.Sprintf("%s\n", strings.ReplaceAll(subtitle, "\n", " ")))

	nextFile := onlineNowFile + ".next"
	if err := os.WriteFile(nextFile, buf.Bytes(), 0644); err == nil {
		_ = os.Rename(nextFile, onlineNowFile)
	}
}
