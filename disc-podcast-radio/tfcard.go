package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	lastPodcastsMTime time.Time
	lastRadiosMTime   time.Time
	tfSyncMu          sync.Mutex
)

// tfSyncState records the content hash of the files this program last wrote to
// the TF card. A different hash therefore means the user edited the file
// externally and the edit should be imported; an identical hash means the file
// is our own mirror and must never overwrite newer online changes.
type tfSyncState struct {
	PodcastsHash string `json:"podcasts_hash,omitempty"`
	RadiosHash   string `json:"radios_hash,omitempty"`
}

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

func tfHash(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

func tfFileHash(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return tfHash(b), nil
}

// tfFileNewerThan reports whether path was modified after reference. A missing
// reference counts as older, so a user-provided file still wins on a fresh
// data directory.
func tfFileNewerThan(path, reference string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	ri, err := os.Stat(reference)
	if err != nil {
		return true
	}
	return fi.ModTime().After(ri.ModTime())
}

func (a *App) tfSyncStatePath() string {
	return filepath.Join(a.dataDir, "tf_sync.json")
}

func (a *App) readTFSyncStateLocked() tfSyncState {
	var st tfSyncState
	_ = loadJSON(a.tfSyncStatePath(), &st)
	return st
}

func (a *App) writeTFSyncStateLocked(st tfSyncState) {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	if err := writeFileAtomic(a.tfSyncStatePath(), b, 0644); err != nil {
		a.log.Warnf("[TFCard] 保存 TF 同步状态失败: %v", err)
	}
}

func (a *App) initTFCardSync(firstRun bool) {
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

	tfSyncMu.Lock()
	state := a.readTFSyncStateLocked()
	tfSyncMu.Unlock()

	// 2. 只在 TF 卡文件确实被用户修改过时才导入，避免重启时用旧文件
	//    覆盖网页端新添加的订阅（这正是订阅列表重启后“恢复”的根因）。
	a.importPodcastsOnStartup(podcastsTxt, firstRun, state.PodcastsHash)
	a.importRadiosOnStartup(radiosTxt, firstRun, state.RadiosHash)

	// 3. 将当前内存列表镜像回 TF 卡，并记录内容哈希，供下次启动判定
	a.syncPodcastsToTF()
	a.syncRadiosToTF()

	// 4. 导出供屏幕 UI 实时读取的纯文本菜单
	a.exportRadiosText()
	a.exportEpisodesText()
}

type tfImportMode int

const (
	tfImportSkip tfImportMode = iota
	tfImportAuthoritative
)

// decideTFImport decides whether a TF-card file should replace the persisted
// list. The hash baseline distinguishes our own mirror from a user edit; when
// no baseline exists yet (upgrading from an older release) the file mtime is
// used so a stale file cannot overwrite newer online changes.
func decideTFImport(firstRun bool, lastHash, fileHash string, tfFileNewer bool) tfImportMode {
	switch {
	case lastHash == "" && firstRun:
		// 全新安装：TF 卡上的文件是用户主动提供的列表
		return tfImportAuthoritative
	case lastHash == "":
		if tfFileNewer {
			return tfImportAuthoritative
		}
		return tfImportSkip
	case fileHash != lastHash:
		// 文件在设备关机期间被用户修改过
		return tfImportAuthoritative
	default:
		// 文件仍是本程序上次写入的内容：以本地 JSON 为准
		return tfImportSkip
	}
}

func (a *App) importPodcastsOnStartup(path string, firstRun bool, lastHash string) {
	hash, err := tfFileHash(path)
	if err != nil {
		a.log.Warnf("[TFCard] 读取 TF 卡播客订阅文件失败: %v", err)
		return
	}
	newer := tfFileNewerThan(path, filepath.Join(a.dataDir, "podcasts.json"))
	if decideTFImport(firstRun, lastHash, hash, newer) == tfImportAuthoritative {
		a.log.Infof("[TFCard] 按 TF 卡内容加载播客订阅列表")
		a.loadPodcastsFromTF(path, true)
	} else if lastHash == "" {
		a.log.Infof("[TFCard] 忽略早于本地数据的旧版 TF 播客订阅文件，保留联网后的最新列表")
	}
}

func (a *App) importRadiosOnStartup(path string, firstRun bool, lastHash string) {
	hash, err := tfFileHash(path)
	if err != nil {
		a.log.Warnf("[TFCard] 读取 TF 卡网络电台文件失败: %v", err)
		return
	}
	newer := tfFileNewerThan(path, filepath.Join(a.dataDir, "stations.json"))
	if decideTFImport(firstRun, lastHash, hash, newer) == tfImportAuthoritative {
		a.log.Infof("[TFCard] 按 TF 卡内容加载网络电台列表")
		a.loadRadiosFromTF(path, true)
	} else if lastHash == "" {
		a.log.Infof("[TFCard] 忽略早于本地数据的旧版 TF 网络电台文件，保留联网后的最新列表")
	}
}

func tfCommentHeader(existing, fallback string) string {
	var sb strings.Builder
	for _, line := range strings.Split(strings.ReplaceAll(existing, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			sb.WriteString(line)
			sb.WriteString("\n")
		}
	}
	if sb.Len() == 0 {
		return fallback + "\n"
	}
	sb.WriteString("\n")
	return sb.String()
}

func podcastTFHeader() string {
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
	sb.WriteString("# 5. 网页端新增/删除订阅后，本文件会自动同步更新。\n")
	sb.WriteString("# ====================================================================\n")
	return sb.String()
}

func radioTFHeader() string {
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
	sb.WriteString("# 6. 网页端新增/删除电台后，本文件会自动同步更新。\n")
	sb.WriteString("# ====================================================================\n")
	return sb.String()
}

func (a *App) podcastsTFContent(existing string) string {
	a.mu.RLock()
	current := append([]PodcastFeed(nil), a.podcasts...)
	a.mu.RUnlock()

	if len(current) == 0 {
		current = defaultPodcasts
	}

	var sb strings.Builder
	sb.WriteString(tfCommentHeader(existing, podcastTFHeader()))
	for _, p := range current {
		title := strings.TrimSpace(p.Title)
		u := strings.TrimSpace(p.URL)
		if title != "" && u != "" {
			sb.WriteString(fmt.Sprintf("%s,%s\n", title, u))
		}
	}
	return sb.String()
}

func (a *App) radiosTFContent(existing string) string {
	a.mu.RLock()
	current := append([]RadioStation(nil), a.stations...)
	a.mu.RUnlock()

	if len(current) == 0 {
		current = defaultStations
	}

	var sb strings.Builder
	sb.WriteString(tfCommentHeader(existing, radioTFHeader()))
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
	return sb.String()
}

func (a *App) generatePodcastsTemplate(dest string) {
	_ = writeFileAtomic(dest, []byte(a.podcastsTFContent("")), 0644)
	a.log.Infof("[TFCard] 已在 TF 卡自动生成播客订阅文件模板: %s", dest)
}

func (a *App) generateRadiosTemplate(dest string) {
	_ = writeFileAtomic(dest, []byte(a.radiosTFContent("")), 0644)
	a.log.Infof("[TFCard] 已在 TF 卡自动生成网络电台文件模板: %s", dest)
}

// syncPodcastsToTF mirrors the authoritative in-memory subscription list back
// to the TF card. It is called after every online subscribe/delete and at
// startup, so a stale TF file can no longer resurrect an outdated list.
func (a *App) syncPodcastsToTF() {
	mount := findTFCardMount()
	if mount == "" {
		return
	}

	tfSyncMu.Lock()
	defer tfSyncMu.Unlock()

	path := filepath.Join(mount, "podcasts.txt")
	existing, _ := os.ReadFile(path)
	content := []byte(a.podcastsTFContent(string(existing)))

	if string(existing) != string(content) {
		if err := writeFileAtomic(path, content, 0644); err != nil {
			a.log.Warnf("[TFCard] 导出播客订阅列表失败: %v", err)
			return
		}
	}
	if fi, err := os.Stat(path); err == nil {
		lastPodcastsMTime = fi.ModTime()
	}

	h := tfHash(content)
	st := a.readTFSyncStateLocked()
	if st.PodcastsHash != h {
		st.PodcastsHash = h
		a.writeTFSyncStateLocked(st)
	}
}

func (a *App) syncRadiosToTF() {
	mount := findTFCardMount()
	if mount == "" {
		return
	}

	tfSyncMu.Lock()
	defer tfSyncMu.Unlock()

	path := filepath.Join(mount, "radios.txt")
	existing, _ := os.ReadFile(path)
	content := []byte(a.radiosTFContent(string(existing)))

	if string(existing) != string(content) {
		if err := writeFileAtomic(path, content, 0644); err != nil {
			a.log.Warnf("[TFCard] 导出网络电台列表失败: %v", err)
			return
		}
	}
	if fi, err := os.Stat(path); err == nil {
		lastRadiosMTime = fi.ModTime()
	}

	h := tfHash(content)
	st := a.readTFSyncStateLocked()
	if st.RadiosHash != h {
		st.RadiosHash = h
		a.writeTFSyncStateLocked(st)
	}
}

// loadPodcastsFromTF parses the TF card file. authoritative=true makes the file
// the complete list (normal user edit); authoritative=false merges it with the
// current list so an upgrade from an older release cannot drop online-added
// subscriptions that are missing from the stale file.
func (a *App) loadPodcastsFromTF(path string, authoritative bool) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}

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

	tfSyncMu.Lock()
	state := a.readTFSyncStateLocked()
	fileHash, hashErr := tfFileHash(path)
	lastPodcastsMTime = fi.ModTime()
	unchanged := state.PodcastsHash != "" && hashErr == nil && state.PodcastsHash == fileHash
	tfSyncMu.Unlock()
	if unchanged {
		// 文件正是本程序上次写入的内容，不需要重新导入。
		return false
	}

	a.mu.Lock()
	// 保留已有单集数据，合并新增订阅
	existingMap := make(map[string]PodcastFeed, len(a.podcasts))
	for _, p := range a.podcasts {
		existingMap[p.URL] = p
	}

	var merged []PodcastFeed
	seen := make(map[string]bool, len(newFeeds))
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
					a.syncPodcastsToTF()
				}
			}(nf.URL)
		}
		seen[nf.URL] = true
	}

	if !authoritative {
		// 旧版 TF 文件可能缺少网页端新增的订阅，补齐这些订阅而不删除它们。
		for _, p := range a.podcasts {
			if !seen[p.URL] {
				merged = append(merged, p)
				seen[p.URL] = true
			}
		}
	}

	a.podcasts = merged
	a.mu.Unlock()
	_ = a.savePodcasts()
	a.log.Infof("[TFCard] 已从 TF 卡加载 %d 个播客订阅", len(merged))
	return true
}

func (a *App) loadRadiosFromTF(path string, authoritative bool) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}

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

	tfSyncMu.Lock()
	state := a.readTFSyncStateLocked()
	fileHash, hashErr := tfFileHash(path)
	lastRadiosMTime = fi.ModTime()
	unchanged := state.RadiosHash != "" && hashErr == nil && state.RadiosHash == fileHash
	tfSyncMu.Unlock()
	if unchanged {
		return false
	}

	a.mu.Lock()
	if authoritative {
		a.stations = newStations
	} else {
		seen := make(map[string]bool, len(newStations))
		for _, st := range newStations {
			seen[st.URL] = true
		}
		merged := append([]RadioStation(nil), newStations...)
		for _, st := range a.stations {
			if !seen[st.URL] {
				merged = append(merged, st)
				seen[st.URL] = true
			}
		}
		a.stations = merged
	}
	_ = a.persistStationsLocked()
	count := len(a.stations)
	a.mu.Unlock()

	a.log.Infof("[TFCard] 已从 TF 卡加载 %d 个网络电台", count)
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
	a.persistUIMenuSnapshot("radios.txt", []byte(sb.String()))
}

func (a *App) tfPodcastsChangedSince(fi os.FileInfo) bool {
	tfSyncMu.Lock()
	defer tfSyncMu.Unlock()
	return fi.ModTime().After(lastPodcastsMTime)
}

func (a *App) tfRadiosChangedSince(fi os.FileInfo) bool {
	tfSyncMu.Lock()
	defer tfSyncMu.Unlock()
	return fi.ModTime().After(lastRadiosMTime)
}

func (a *App) tfPodcastsHaveBaseline() bool {
	tfSyncMu.Lock()
	defer tfSyncMu.Unlock()
	return a.readTFSyncStateLocked().PodcastsHash != ""
}

func (a *App) tfRadiosHaveBaseline() bool {
	tfSyncMu.Lock()
	defer tfSyncMu.Unlock()
	return a.readTFSyncStateLocked().RadiosHash != ""
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
				if a.tfPodcastsChangedSince(fi) {
					a.log.Infof("[TFCard] 检测到 TF 卡播客订阅文件修改 (%v)，正在重新加载…", fi.ModTime())
					if a.loadPodcastsFromTF(podcastsTxt, a.tfPodcastsHaveBaseline()) {
						a.syncPodcastsToTF()
						a.exportEpisodesText()
					}
				}
			}

			radiosTxt := filepath.Join(mount, "radios.txt")
			if fi, err := os.Stat(radiosTxt); err == nil {
				if a.tfRadiosChangedSince(fi) {
					a.log.Infof("[TFCard] 检测到 TF 卡网络电台文件修改 (%v)，正在重新加载…", fi.ModTime())
					if a.loadRadiosFromTF(radiosTxt, a.tfRadiosHaveBaseline()) {
						a.syncRadiosToTF()
						a.exportRadiosText()
					}
				}
			}
		}
	}
}
