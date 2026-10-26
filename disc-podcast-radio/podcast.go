package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/transform"
)

// --- RSS / Atom models ---

type rssFeed struct {
	XMLName xml.Name   `xml:"rss"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title        string     `xml:"title"`
	Description  string     `xml:"description"`
	Link         string     `xml:"link"`
	Language     string     `xml:"language"`
	Author       string     `xml:"author"`
	ItunesAuthor string     `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd author"`
	Images       []rssImage `xml:"image"`
	Items        []rssItem  `xml:"item"`
}

type rssImage struct {
	URL  string `xml:"url"`
	Href string `xml:"href,attr"`
}

type rssLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
	Text string `xml:",chardata"`
}

type rssMedia struct {
	URL      string `xml:"url,attr"`
	Type     string `xml:"type,attr"`
	Medium   string `xml:"medium,attr"`
	FileSize int64  `xml:"fileSize,attr"`
}

type rssEnclosure struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

type rssItem struct {
	Title       string
	Link        string
	Links       []rssLink
	GUID        string
	Description string
	Summary     string
	Encoded     string
	PubDate     string
	Duration    string
	Enclosure   rssEnclosure
	Media       []rssMedia
	Images      []rssImage
}

func (it *rssItem) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "title":
				_ = d.DecodeElement(&it.Title, &t)
			case "link":
				var l rssLink
				if err := d.DecodeElement(&l, &t); err == nil {
					if strings.TrimSpace(l.Text) != "" {
						it.Link = strings.TrimSpace(l.Text)
					}
					if l.Href != "" {
						it.Links = append(it.Links, l)
					}
				}
			case "guid", "id":
				_ = d.DecodeElement(&it.GUID, &t)
			case "description":
				_ = d.DecodeElement(&it.Description, &t)
			case "summary":
				_ = d.DecodeElement(&it.Summary, &t)
			case "encoded":
				_ = d.DecodeElement(&it.Encoded, &t)
			case "pubDate", "published", "updated":
				var v string
				if err := d.DecodeElement(&v, &t); err == nil {
					it.PubDate = v
				}
			case "duration":
				_ = d.DecodeElement(&it.Duration, &t)
			case "enclosure":
				_ = d.DecodeElement(&it.Enclosure, &t)
			case "content":
				var m rssMedia
				if err := d.DecodeElement(&m, &t); err == nil && m.URL != "" {
					it.Media = append(it.Media, m)
				}
			case "group":
				var g struct {
					Media      []rssMedia `xml:"content"`
					Thumbnails []rssImage `xml:"thumbnail"`
				}
				if err := d.DecodeElement(&g, &t); err == nil {
					it.Media = append(it.Media, g.Media...)
					it.Images = append(it.Images, g.Thumbnails...)
				}
			case "thumbnail", "image":
				var img rssImage
				if err := d.DecodeElement(&img, &t); err == nil {
					it.Images = append(it.Images, img)
				}
			default:
				_ = d.Skip()
			}
		case xml.EndElement:
			if t.Name.Local == start.Name.Local {
				return nil
			}
		}
	}
}

type atomFeed struct {
	XMLName  xml.Name    `xml:"feed"`
	Title    string      `xml:"title"`
	Subtitle string      `xml:"subtitle"`
	Icon     string      `xml:"icon"`
	Logo     string      `xml:"logo"`
	Author   atomAuthor  `xml:"author"`
	Entries  []atomEntry `xml:"entry"`
}

type atomAuthor struct {
	Name string `xml:"name"`
}

type atomEntry struct {
	Title     string     `xml:"title"`
	ID        string     `xml:"id"`
	Summary   string     `xml:"summary"`
	Content   string     `xml:"content"`
	Published string     `xml:"published"`
	Updated   string     `xml:"updated"`
	Links     []rssLink  `xml:"link"`
	Image     []rssImage `xml:"image"`
}

// --- Feed parsing ---

func parseFeedDocument(body []byte) (*PodcastFeed, error) {
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errors.New("订阅地址返回了空内容")
	}
	lower := strings.ToLower(string(trimmed[:minInt(len(trimmed), 512)]))
	if strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html") {
		return nil, errors.New("订阅地址返回网页而不是 RSS/Atom，请填写播客 XML 订阅链接")
	}

	dec := xml.NewDecoder(bytes.NewReader(trimmed))
	dec.Strict = false
	dec.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		enc, err := htmlindex.Get(charset)
		if err != nil {
			return input, nil
		}
		return transform.NewReader(input, enc.NewDecoder()), nil
	}

	var rss rssFeed
	if err := dec.Decode(&rss); err == nil && rss.XMLName.Local == "rss" {
		return convertRSS(&rss)
	}

	// Retry as Atom. Some feeds start with processing instructions, which is
	// why the decoder is recreated instead of rewinding a half-read decoder.
	dec2 := xml.NewDecoder(bytes.NewReader(trimmed))
	dec2.Strict = false
	dec2.CharsetReader = dec.CharsetReader
	var atom atomFeed
	if err := dec2.Decode(&atom); err == nil && (atom.XMLName.Local == "feed" || len(atom.Entries) > 0) {
		return convertAtom(&atom)
	}

	sample := strings.TrimSpace(string(trimmed))
	if len(sample) > 160 {
		sample = sample[:160] + "…"
	}
	return nil, fmt.Errorf("不是有效的 RSS/Atom 订阅源（内容开头: %s）", sample)
}

func convertRSS(rss *rssFeed) (*PodcastFeed, error) {
	ch := rss.Channel
	if strings.TrimSpace(ch.Title) == "" && len(ch.Items) == 0 {
		return nil, errors.New("RSS 中没有 channel/title 或 item")
	}
	cover := ""
	for _, img := range ch.Images {
		if img.Href != "" {
			cover = img.Href
			break
		}
		if img.URL != "" {
			cover = img.URL
			break
		}
	}
	author := strings.TrimSpace(ch.Author)
	if author == "" {
		author = strings.TrimSpace(ch.ItunesAuthor)
	}
	feed := &PodcastFeed{
		Title:       strings.TrimSpace(ch.Title),
		Description: cleanHTML(ch.Description),
		Cover:       cover,
		Author:      author,
		Episodes:    []PodcastItem{},
		LastUpdated: time.Now().Format("2006-01-02 15:04"),
	}
	if feed.Title == "" {
		feed.Title = "未命名播客"
	}
	for _, item := range ch.Items {
		audio := itemAudioURL(item)
		if audio == "" {
			continue
		}
		guid := strings.TrimSpace(item.GUID)
		if guid == "" {
			guid = audio
		}
		desc := item.Description
		if strings.TrimSpace(desc) == "" {
			desc = item.Summary
		}
		if strings.TrimSpace(desc) == "" {
			desc = item.Encoded
		}
		if feed.Cover == "" {
			for _, img := range item.Images {
				if img.Href != "" {
					feed.Cover = img.Href
					break
				}
				if img.URL != "" {
					feed.Cover = img.URL
					break
				}
			}
		}
		feed.Episodes = append(feed.Episodes, PodcastItem{
			ID:          "ep_" + stableHash(guid, 16),
			Title:       strings.TrimSpace(item.Title),
			AudioURL:    audio,
			PubDate:     formatDate(item.PubDate),
			Duration:    formatDuration(item.Duration),
			Description: cleanHTML(desc),
			FileSize:    item.Enclosure.Length,
		})
		if len(feed.Episodes) >= 100 {
			break
		}
	}
	if len(feed.Episodes) == 0 {
		return nil, errors.New("订阅源中没有找到音频单集（enclosure/media:content）")
	}
	return feed, nil
}

func convertAtom(atom *atomFeed) (*PodcastFeed, error) {
	feed := &PodcastFeed{
		Title:       strings.TrimSpace(atom.Title),
		Description: cleanHTML(atom.Subtitle),
		Cover:       strings.TrimSpace(atom.Logo),
		Author:      strings.TrimSpace(atom.Author.Name),
		Episodes:    []PodcastItem{},
		LastUpdated: time.Now().Format("2006-01-02 15:04"),
	}
	if feed.Title == "" {
		feed.Title = "未命名播客"
	}
	if feed.Cover == "" {
		feed.Cover = strings.TrimSpace(atom.Icon)
	}
	for _, item := range atom.Entries {
		var audio string
		for _, l := range item.Links {
			if strings.EqualFold(l.Rel, "enclosure") && looksAudioURL(l.Href, l.Type) {
				audio = strings.TrimSpace(l.Href)
				break
			}
		}
		if audio == "" {
			for _, l := range item.Links {
				if looksAudioURL(l.Href, l.Type) {
					audio = strings.TrimSpace(l.Href)
					break
				}
			}
		}
		if audio == "" {
			continue
		}
		desc := item.Content
		if strings.TrimSpace(desc) == "" {
			desc = item.Summary
		}
		id := item.ID
		if id == "" {
			id = audio
		}
		pub := item.Published
		if pub == "" {
			pub = item.Updated
		}
		feed.Episodes = append(feed.Episodes, PodcastItem{
			ID:          "ep_" + stableHash(id, 16),
			Title:       strings.TrimSpace(item.Title),
			AudioURL:    audio,
			PubDate:     formatDate(pub),
			Description: cleanHTML(desc),
		})
		if feed.Cover == "" && len(item.Image) > 0 {
			feed.Cover = item.Image[0].Href
			if feed.Cover == "" {
				feed.Cover = item.Image[0].URL
			}
		}
		if len(feed.Episodes) >= 100 {
			break
		}
	}
	if len(feed.Episodes) == 0 {
		return nil, errors.New("Atom 订阅源中没有找到音频 enclosure")
	}
	return feed, nil
}

func itemAudioURL(item rssItem) string {
	if item.Enclosure.URL != "" && looksAudioURL(item.Enclosure.URL, item.Enclosure.Type) {
		return strings.TrimSpace(item.Enclosure.URL)
	}
	for _, m := range item.Media {
		if m.URL == "" {
			continue
		}
		if strings.EqualFold(m.Medium, "audio") || looksAudioURL(m.URL, m.Type) {
			return strings.TrimSpace(m.URL)
		}
	}
	for _, l := range item.Links {
		if strings.EqualFold(l.Rel, "enclosure") && looksAudioURL(l.Href, l.Type) {
			return strings.TrimSpace(l.Href)
		}
	}
	if looksAudioURL(item.Link, "") {
		return strings.TrimSpace(item.Link)
	}
	// Last resort: some feeds omit type but put the only media URL in
	// media:content; accept it if it is not obviously an image.
	for _, m := range item.Media {
		if m.URL != "" && !strings.HasPrefix(strings.ToLower(m.Type), "image/") {
			return strings.TrimSpace(m.URL)
		}
	}
	return ""
}

func looksAudioURL(raw, contentType string) bool {
	if raw == "" {
		return false
	}
	ct := normalizeContentType(contentType)
	if strings.HasPrefix(ct, "audio/") {
		return true
	}
	if ct == "video/mp4" || ct == "application/ogg" {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch extFromURL(u.Path) {
	case "mp3", "aac", "m4a", "flac", "ogg", "wav", "ape", "wma":
		return true
	}
	return false
}

// --- Fetching / cache ---

func (a *App) FetchPodcastEpisodes(ctx context.Context, feedURL string, force bool) (*PodcastFeed, error) {
	feedURL = strings.TrimSpace(feedURL)
	if err := validateHTTPURL(feedURL); err != nil {
		return nil, err
	}
	if !force {
		a.feedMu.Lock()
		if c, ok := a.feedCache[feedURL]; ok && time.Since(c.at) < 2*time.Minute {
			cp := cloneFeed(c.feed)
			a.feedMu.Unlock()
			return cp, nil
		}
		a.feedMu.Unlock()
	}

	a.log.Infof("开始解析播客订阅: %s", feedURL)
	start := time.Now()
	body, ct, err := a.fetchBytes(ctx, feedURL, 16<<20)
	if err != nil {
		return nil, fmt.Errorf("下载订阅源失败: %w", err)
	}
	if strings.Contains(strings.ToLower(ct), "text/html") {
		trim := bytes.TrimSpace(body)
		if !bytes.HasPrefix(trim, []byte("<?xml")) && !bytes.HasPrefix(trim, []byte("<rss")) && !bytes.HasPrefix(trim, []byte("<feed")) {
			return nil, errors.New("订阅地址返回网页而不是 RSS/Atom，请填写播客 XML 链接")
		}
	}
	feed, err := parseFeedDocument(body)
	if err != nil {
		return nil, err
	}
	feed.URL = feedURL
	feed.ID = "pod_" + stableHash(feedURL, 12)
	feed.Title = strings.TrimSpace(feed.Title)
	if feed.Title == "" {
		feed.Title = "未命名播客"
	}
	if feed.Cover != "" {
		feed.Cover = resolveURL(feedURL, feed.Cover)
		if !strings.HasPrefix(feed.Cover, "http://") && !strings.HasPrefix(feed.Cover, "https://") {
			feed.Cover = ""
		}
	}
	for i := range feed.Episodes {
		feed.Episodes[i].AudioURL = resolveURL(feedURL, feed.Episodes[i].AudioURL)
		if feed.Episodes[i].Title == "" {
			feed.Episodes[i].Title = "未命名单集"
		}
		feed.Episodes[i].Downloaded = a.episodeDownloaded(feed.Title, feed.Episodes[i].Title, feed.Episodes[i].AudioURL)
	}
	a.log.Infof("播客解析成功: 《%s》 %d 集，耗时 %s，封面=%q", feed.Title, len(feed.Episodes), time.Since(start).Round(time.Millisecond), feed.Cover)

	a.feedMu.Lock()
	a.feedCache[feedURL] = cachedFeed{feed: cloneFeed(feed), at: time.Now()}
	a.feedMu.Unlock()

	// If this feed is subscribed, persist the new episode list and rebuild
	// the on-device M3U playlists.
	updated := false
	a.mu.Lock()
	for i := range a.podcasts {
		if a.podcasts[i].URL == feedURL {
			feed.ID = a.podcasts[i].ID
			if feed.ID == "" {
				feed.ID = "pod_" + stableHash(feedURL, 12)
			}
			a.podcasts[i] = *feed
			updated = true
			break
		}
	}
	if updated {
		if err := a.persistPodcastsLocked(); err != nil {
			a.log.Warnf("保存播客缓存失败: %v", err)
		}
	}
	a.mu.Unlock()
	if updated {
		go func() {
			if err := a.buildPlaylists("播客刷新: " + feed.Title); err != nil {
				a.log.Warnf("更新播客播放列表失败: %v", err)
			}
		}()
	}
	return cloneFeed(feed), nil
}

func (a *App) episodeDownloaded(podcastTitle, episodeTitle, audioURL string) bool {
	base := filepath.Join(a.downloadDir(), sanitizeFilename(podcastTitle))
	stem := sanitizeFilename(episodeTitle)
	for _, ext := range []string{downloadExt(audioURL), "mp3", "m4a", "aac", "flac", "ogg"} {
		if ext == "" {
			continue
		}
		if st, err := os.Stat(filepath.Join(base, stem+"."+ext)); err == nil && st.Size() > 0 {
			return true
		}
	}
	return false
}

func (a *App) downloadDir() string {
	if tfCardMounted() {
		if st, err := os.Stat("/tmp/sdcard"); err == nil && st.IsDir() {
			return "/tmp/sdcard/流媒体下载/播客"
		}
	}
	return filepath.Join(a.dataDir, "downloads", "播客")
}

// --- Download ---

func downloadExt(raw string) string {
	ext := extFromURL(raw)
	if ext == "" {
		ext = "mp3"
	}
	return ext
}

func (a *App) DownloadEpisode(podcastTitle, epTitle, audioURL, epID string) {
	if err := validateHTTPURL(audioURL); err != nil {
		a.log.Warnf("拒绝下载非法地址: %v", err)
		return
	}
	if looksLikeHLSURL(audioURL) {
		a.log.Warnf("暂不支持直接下载 HLS 单集: %s", epTitle)
		return
	}
	if epID == "" {
		epID = "ep_" + stableHash(audioURL, 16)
	}
	a.mu.Lock()
	if _, exists := a.downloads[epID]; exists {
		a.mu.Unlock()
		return
	}
	task := &DownloadTask{EpisodeID: epID, Title: epTitle, Status: "downloading"}
	a.downloads[epID] = task
	a.mu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				a.updateDownload(epID, func(t *DownloadTask) {
					t.Status = "failed"
					t.Error = fmt.Sprintf("panic: %v", r)
				})
				a.log.Errorf("下载 panic: %v", r)
			}
		}()

		targetDir := filepath.Join(a.downloadDir(), sanitizeFilename(podcastTitle))
		if err := os.MkdirAll(targetDir, 0755); err != nil {
			a.updateDownload(epID, func(t *DownloadTask) { t.Status = "failed"; t.Error = err.Error() })
			return
		}
		ext := downloadExt(audioURL)
		destFile := filepath.Join(targetDir, sanitizeFilename(epTitle)+"."+ext)
		if st, err := os.Stat(destFile); err == nil && st.Size() > 0 {
			a.updateDownload(epID, func(t *DownloadTask) {
				t.Status = "completed"
				t.Progress = 100
				t.Written = st.Size()
				t.TotalBytes = st.Size()
				t.Path = destFile
			})
			return
		}
		tempFile := destFile + ".part"

		ctx, cancel := context.WithTimeout(a.ctx, 30*time.Minute)
		defer cancel()
		resp, err := a.openUpstream(ctx, audioURL, "")
		if err != nil {
			a.failDownload(epID, err)
			return
		}
		defer resp.Body.Close()
		ct := normalizeContentType(resp.Header.Get("Content-Type"))
		if ct == "text/html" {
			a.failDownload(epID, errors.New("下载地址返回网页而不是音频"))
			return
		}
		out, err := os.Create(tempFile)
		if err != nil {
			a.failDownload(epID, err)
			return
		}
		total := resp.ContentLength
		a.updateDownload(epID, func(t *DownloadTask) { t.TotalBytes = total })
		buf := make([]byte, 32<<10)
		var written int64
		lastUpdate := time.Now()
		for {
			n, er := resp.Body.Read(buf)
			if n > 0 {
				nw, ew := out.Write(buf[:n])
				written += int64(nw)
				if ew != nil {
					err = ew
					break
				}
				if time.Since(lastUpdate) > 500*time.Millisecond {
					a.updateDownload(epID, func(t *DownloadTask) {
						t.Written = written
						if total > 0 {
							t.Progress = float64(written) / float64(total) * 100
						}
					})
					lastUpdate = time.Now()
				}
			}
			if er != nil {
				if er != io.EOF {
					err = er
				}
				break
			}
		}
		closeErr := out.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(tempFile)
			a.failDownload(epID, err)
			return
		}
		if err := os.Rename(tempFile, destFile); err != nil {
			_ = os.Remove(tempFile)
			a.failDownload(epID, err)
			return
		}
		a.updateDownload(epID, func(t *DownloadTask) {
			t.Progress = 100
			t.Written = written
			t.Status = "completed"
			t.Path = destFile
			if t.TotalBytes <= 0 {
				t.TotalBytes = written
			}
		})
		a.log.Infof("下载完成: %s -> %s (%d 字节)", epTitle, destFile, written)

		a.mu.Lock()
		for i := range a.podcasts {
			for j := range a.podcasts[i].Episodes {
				if a.podcasts[i].Episodes[j].ID == epID {
					a.podcasts[i].Episodes[j].Downloaded = true
				}
			}
		}
		_ = a.persistPodcastsLocked()
		a.mu.Unlock()
	}()
}

func (a *App) updateDownload(id string, fn func(*DownloadTask)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.downloads[id]; ok {
		fn(t)
	}
}

func (a *App) failDownload(id string, err error) {
	a.updateDownload(id, func(t *DownloadTask) {
		t.Status = "failed"
		t.Error = err.Error()
	})
	a.log.Warnf("下载失败: %v", err)
}

// --- Radio Browser search ---

func (a *App) SearchRadioBrowser(ctx context.Context, query string) ([]RadioStation, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []RadioStation{}, nil
	}
	servers := []string{
		"https://de1.api.radio-browser.info",
		"https://de2.api.radio-browser.info",
	}
	var lastErr error
	for _, server := range servers {
		u := fmt.Sprintf("%s/json/stations/search?name=%s&limit=30&hidebroken=true&order=votes&reverse=true", server, url.QueryEscape(query))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "application/json")
		resp, err := a.doWithFallback(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if rerr != nil {
			lastErr = rerr
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("%s 返回 HTTP %d", server, resp.StatusCode)
			continue
		}
		var raw []struct {
			StationUUID string `json:"stationuuid"`
			Name        string `json:"name"`
			URL         string `json:"url"`
			URLResolved string `json:"url_resolved"`
			Favicon     string `json:"favicon"`
			CountryCode string `json:"countrycode"`
			Bitrate     int    `json:"bitrate"`
			Codec       string `json:"codec"`
			Tags        string `json:"tags"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			lastErr = err
			continue
		}
		seen := make(map[string]bool)
		out := make([]RadioStation, 0, len(raw))
		for _, r := range raw {
			rawURL := strings.TrimSpace(r.URLResolved)
			if rawURL == "" {
				rawURL = strings.TrimSpace(r.URL)
			}
			if rawURL == "" || seen[rawURL] {
				continue
			}
			seen[rawURL] = true
			cat := "搜索结果"
			if tags := strings.Split(r.Tags, ","); len(tags) > 0 && strings.TrimSpace(tags[0]) != "" {
				cat = strings.TrimSpace(tags[0])
			} else if r.CountryCode != "" {
				cat = strings.ToUpper(r.CountryCode)
			}
			out = append(out, RadioStation{
				ID:       "rb_" + stableHash(r.StationUUID+rawURL, 12),
				Name:     strings.TrimSpace(r.Name),
				URL:      rawURL,
				Category: cat,
				Favicon:  r.Favicon,
				Bitrate:  r.Bitrate,
				Codec:    r.Codec,
			})
		}
		a.log.Infof("Radio Browser 搜索 %q 返回 %d 个结果", query, len(out))
		return out, nil
	}
	if lastErr == nil {
		lastErr = errors.New("没有可用的 Radio Browser 服务器")
	}
	return nil, lastErr
}

// --- Utilities ---

var (
	htmlScriptRE = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	htmlTagRE    = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRE      = regexp.MustCompile(`\s+`)
)

func cleanHTML(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	s = htmlScriptRE.ReplaceAllString(s, " ")
	s = htmlTagRE.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\u00a0", " ")
	s = spaceRE.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	if len([]rune(s)) > 400 {
		r := []rune(s)
		s = string(r[:400]) + "…"
	}
	return s
}

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '_'
		}
		return r
	}, name)
	r := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_", "#", "_", "%", "_", "\x00", "_")
	name = r.Replace(name)
	name = strings.Trim(name, " .")
	if name == "" {
		name = "未命名"
	}
	// Keep filenames comfortably below ext4's 255-byte component limit.
	for len([]byte(name)) > 150 {
		r := []rune(name)
		if len(r) <= 1 {
			break
		}
		name = string(r[:len(r)-1])
	}
	return name
}

func formatDate(d string) string {
	d = strings.TrimSpace(d)
	if d == "" {
		return ""
	}
	formats := []string{
		time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822,
		time.RFC3339, "2006-01-02T15:04:05Z07:00", "2006-01-02 15:04:05",
		"Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, d); err == nil {
			return t.Format("2006-01-02")
		}
	}
	if len([]rune(d)) > 30 {
		r := []rune(d)
		return string(r[:30])
	}
	return d
}

func formatDuration(d string) string {
	d = strings.TrimSpace(d)
	if d == "" {
		return ""
	}
	if strings.Contains(d, ":") {
		return d
	}
	if f, err := strconv.ParseFloat(d, 64); err == nil && f >= 0 {
		sec := int(f)
		h := sec / 3600
		m := (sec % 3600) / 60
		s := sec % 60
		if h > 0 {
			return fmt.Sprintf("%d:%02d:%02d", h, m, s)
		}
		return fmt.Sprintf("%d:%02d", m, s)
	}
	return d
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
