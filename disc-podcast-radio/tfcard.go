package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	lastPodcastsMTime time.Time
	lastRadiosMTime   time.Time
)

func findTFCardMount() string {
	candidates := []string{"/tmp/sdcard", "/mnt/sdcard"}
	for _, c := range candidates {
		fi, err := os.Stat(c)
		if err == nil && fi.IsDir() {
			return c
		}
	}
	return ""
}

func (a *App) initTFCardSync() {
	mount := findTFCardMount()
	if mount == "" {
		a.log.Infof("[TFCard] 当前未检测到 TF 卡挂载目录 (/tmp/sdcard)，暂未同步 TF 卡文件")
		a.exportRadiosText()
		a.exportEpisodesText()
		return
	}

	podcastsTxt := filepath.Join(mount, "podcasts.txt")
	radiosTxt := filepath.Join(mount, "radios.txt")

	// 1. 如果 TF 卡上尚未存在订阅或电台文件，自动生成带中文说明的模板文件
	if _, err := os.Stat(podcastsTxt); os.IsNotExist(err) {
		a.generatePodcastsTemplate(podcastsTxt)
	}
	if _, err := os.Stat(radiosTxt); os.IsNotExist(err) {
		a.generateRadiosTemplate(radiosTxt)
	}

	// 2. 从 TF 卡读取并加载
	a.loadPodcastsFromTF(podcastsTxt)
	a.loadRadiosFromTF(radiosTxt)

	// 3. 导出供屏幕 UI 实时读取的纯文本菜单
	a.exportRadiosText()
	a.exportEpisodesText()
}

func (a *App) generatePodcastsTemplate(dest string) {
	var sb strings.Builder
	sb.WriteString("# ====================================================================\n")
	sb.WriteString("# FiiO DISC 播客订阅列表 (TF 卡自由编辑)\n")
	sb.WriteString("#\n")
	sb.WriteString("# 使用说明：\n")
	sb.WriteString("# 1. 每一行输入一个播客订阅，格式为：播客名称,RSS订阅地址\n")
	sb.WriteString("# 2. 也可以使用标准换行格式：\n")
	sb.WriteString("#    [播客名称]\n")
	sb.WriteString("#    https://example.com/feed.xml\n")
	sb.WriteString("# 3. 以井号(#)开头的行是注释，将被系统忽略。\n")
	sb.WriteString("# 4. 保存文件后无需重启，系统将在几秒内自动加载更新。\n")
	sb.WriteString("# ====================================================================\n\n")

	a.mu.RLock()
	current := append([]PodcastFeed(nil), a.podcasts...)
	a.mu.RUnlock()

	if len(current) == 0 {
		current = defaultPodcasts
	}
	for _, p := range current {
		title := strings.TrimSpace(p.Title)
		u := strings.TrimSpace(p.URL)
		if title != "" && u != "" {
			sb.WriteString(fmt.Sprintf("%s,%s\n", title, u))
		}
	}

	_ = os.WriteFile(dest, []byte(sb.String()), 0644)
	a.log.Infof("[TFCard] 已在 TF 卡自动生成播客订阅文件模板: %s", dest)
}

func (a *App) generateRadiosTemplate(dest string) {
	var sb strings.Builder
	sb.WriteString("# ====================================================================\n")
	sb.WriteString("# FiiO DISC 网络收音机电台列表 (TF 卡自由编辑)\n")
	sb.WriteString("#\n")
	sb.WriteString("# 使用说明：\n")
	sb.WriteString("# 1. 每一行输入一个电台，格式为：分类,电台名称,流媒体播放地址\n")
	sb.WriteString("# 2. 也可以按分类分组编写：\n")
	sb.WriteString("#    [分类名称]\n")
	sb.WriteString("#    电台名称,流媒体播放地址\n")
	sb.WriteString("# 3. 分隔符支持英文逗号(,)、中文逗号(，)或制表符(Tab)。\n")
	sb.WriteString("# 4. 以井号(#)开头的行是注释，将被系统忽略。\n")
	sb.WriteString("# 5. 保存文件后无需重启，系统将在几秒内自动加载更新。\n")
	sb.WriteString("# ====================================================================\n\n")

	a.mu.RLock()
	current := append([]RadioStation(nil), a.stations...)
	a.mu.RUnlock()

	if len(current) == 0 {
		current = defaultStations
	}
	for _, s := range current {
		cat := strings.TrimSpace(s.Category)
		if cat == "" {
			cat = "网络电台"
		}
		name := strings.TrimSpace(s.Name)
		u := strings.TrimSpace(s.URL)
		if name != "" && u != "" {
			sb.WriteString(fmt.Sprintf("%s,%s,%s\n", cat, name, u))
		}
	}

	_ = os.WriteFile(dest, []byte(sb.String()), 0644)
	a.log.Infof("[TFCard] 已在 TF 卡自动生成网络电台文件模板: %s", dest)
}

func (a *App) loadPodcastsFromTF(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	lastPodcastsMTime = fi.ModTime()

	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	var newFeeds []PodcastFeed
	scanner := bufio.NewScanner(f)
	currentSection := ""

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			currentSection = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}

		var title, rawURL string
		sep := ","
		if strings.Contains(line, "，") {
			sep = "，"
		} else if strings.Contains(line, "\t") {
			sep = "\t"
		} else if strings.Contains(line, "|") {
			sep = "|"
		}

		parts := strings.SplitN(line, sep, 2)
		if len(parts) == 2 {
			title = strings.TrimSpace(parts[0])
			rawURL = strings.TrimSpace(parts[1])
		} else if currentSection != "" && strings.HasPrefix(line, "http") {
			title = currentSection
			rawURL = line
		} else if strings.HasPrefix(line, "http") {
			rawURL = line
			title = "自定义播客"
		}

		if rawURL != "" {
			id := "tf_" + stableHash(rawURL, 12)
			newFeeds = append(newFeeds, PodcastFeed{
				ID:       id,
				Title:    title,
				URL:      rawURL,
				Episodes: []PodcastItem{},
			})
		}
	}

	if len(newFeeds) == 0 {
		return false
	}

	a.mu.Lock()
	// 保留已有单集数据，合并新增订阅
	existingMap := make(map[string]PodcastFeed, len(a.podcasts))
	for _, p := range a.podcasts {
		existingMap[p.URL] = p
	}

	var merged []PodcastFeed
	for _, nf := range newFeeds {
		if ex, ok := existingMap[nf.URL]; ok {
			if nf.Title != "" && nf.Title != "自定义播客" {
				ex.Title = nf.Title
			}
			merged = append(merged, ex)
			delete(existingMap, nf.URL)
		} else {
			merged = append(merged, nf)
			// 新增播客异步拉取单集
			go func(feedURL string) {
				ctx, cancel := context.WithTimeout(a.ctx, 45*time.Second)
				defer cancel()
				feed, err := a.FetchPodcastEpisodes(ctx, feedURL, true)
				if err == nil && feed != nil {
					a.log.Infof("[TFCard] 成功解析新增播客《%s》，共 %d 集", feed.Title, len(feed.Episodes))
					a.exportEpisodesText()
				}
			}(nf.URL)
		}
	}
	a.podcasts = merged
	a.mu.Unlock()
	_ = a.savePodcasts()
	a.log.Infof("[TFCard] 已从 TF 卡加载 %d 个播客订阅", len(merged))
	return true
}

func (a *App) loadRadiosFromTF(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	lastRadiosMTime = fi.ModTime()

	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	var newStations []RadioStation
	scanner := bufio.NewScanner(f)
	currentCat := "网络电台"

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			currentCat = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}

		sep := ","
		if strings.Contains(line, "，") {
			sep = "，"
		} else if strings.Contains(line, "\t") {
			sep = "\t"
		} else if strings.Contains(line, "|") {
			sep = "|"
		}

		parts := strings.Split(line, sep)
		var cat, name, rawURL string
		if len(parts) >= 3 {
			cat = strings.TrimSpace(parts[0])
			name = strings.TrimSpace(parts[1])
			rawURL = strings.TrimSpace(parts[2])
		} else if len(parts) == 2 {
			cat = currentCat
			name = strings.TrimSpace(parts[0])
			rawURL = strings.TrimSpace(parts[1])
		}

		if name != "" && rawURL != "" {
			if cat == "" {
				cat = currentCat
			}
			newStations = append(newStations, RadioStation{
				ID:       "tf_" + stableHash(rawURL, 12),
				Name:     name,
				URL:      rawURL,
				Category: cat,
				Codec:    "MP3",
			})
		}
	}

	if len(newStations) == 0 {
		return false
	}

	a.mu.Lock()
	a.stations = newStations
	a.mu.Unlock()
	_ = a.saveStations()
	a.log.Infof("[TFCard] 已从 TF 卡加载 %d 个网络电台", len(newStations))
	return true
}

func (a *App) exportRadiosText() {
	cmdDir := "/tmp/disc-podcast-radio"
	_ = os.MkdirAll(cmdDir, 0755)
	txtFile := filepath.Join(cmdDir, "radios.txt")

	a.mu.RLock()
	stations := append([]RadioStation(nil), a.stations...)
	a.mu.RUnlock()

	// 分类聚合
	catsOrder := make([]string, 0)
	catStations := make(map[string][]RadioStation)
	for _, s := range stations {
		cat := strings.TrimSpace(s.Category)
		if cat == "" {
			cat = "网络电台"
		}
		if _, exists := catStations[cat]; !exists {
			catsOrder = append(catsOrder, cat)
		}
		catStations[cat] = append(catStations[cat], s)
	}

	var sb strings.Builder
	for _, cat := range catsOrder {
		sb.WriteString(fmt.Sprintf("[%s]\n", cat))
		for _, s := range catStations[cat] {
			cleanName := strings.ReplaceAll(s.Name, "|", "-")
			cleanName = strings.ReplaceAll(cleanName, "\t", " ")
			cleanName = strings.ReplaceAll(cleanName, "\n", " ")
			cleanName = strings.TrimSpace(cleanName)
			cleanURL := strings.TrimSpace(s.URL)
			if cleanName != "" && cleanURL != "" {
				sb.WriteString(fmt.Sprintf("%s | %s\n", cleanName, cleanURL))
			}
		}
	}

	_ = os.WriteFile(txtFile+".next", []byte(sb.String()), 0644)
	_ = os.Rename(txtFile+".next", txtFile)
}

func (a *App) tfCardWatcherLoop() {
	ticker := time.NewTicker(8 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			mount := findTFCardMount()
			if mount == "" {
				continue
			}

			podcastsTxt := filepath.Join(mount, "podcasts.txt")
			if fi, err := os.Stat(podcastsTxt); err == nil {
				if fi.ModTime().After(lastPodcastsMTime) {
					a.log.Infof("[TFCard] 检测到 TF 卡播客订阅文件修改 (%v)，正在重新加载…", fi.ModTime())
					if a.loadPodcastsFromTF(podcastsTxt) {
						a.exportEpisodesText()
					}
				}
			}

			radiosTxt := filepath.Join(mount, "radios.txt")
			if fi, err := os.Stat(radiosTxt); err == nil {
				if fi.ModTime().After(lastRadiosMTime) {
					a.log.Infof("[TFCard] 检测到 TF 卡网络电台文件修改 (%v)，正在重新加载…", fi.ModTime())
					if a.loadRadiosFromTF(radiosTxt) {
						a.exportRadiosText()
					}
				}
			}
		}
	}
}
