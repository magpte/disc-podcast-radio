package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Renderer diagnostics are intentionally low-level: the device's stock player
// exposes a UPnP server, but its bind address and HTTP paths differ between
// firmware builds. These probes answer the practical questions ("is anything
// listening on 1286?", "does /description.xml or / return XML?", "which process
// owns the port?") without requiring SSH.

type LocalInterfaceInfo struct {
	Name  string   `json:"name"`
	Flags string   `json:"flags"`
	Addrs []string `json:"addrs"`
}

type ListenerInfo struct {
	Proto   string `json:"proto"`
	Address string `json:"address"`
	Port    int    `json:"port"`
	UID     int    `json:"uid,omitempty"`
	Inode   string `json:"inode,omitempty"`
	PID     int    `json:"pid,omitempty"`
	Comm    string `json:"comm,omitempty"`
}

type PlayerProcessInfo struct {
	PID     int      `json:"pid"`
	Comm    string   `json:"comm"`
	Cmdline string   `json:"cmdline,omitempty"`
	State   string   `json:"state,omitempty"`
	Threads int      `json:"threads,omitempty"`
	FDs     []string `json:"fds,omitempty"`
}

type SocketInfo struct {
	Path    string `json:"path"`
	Exists  bool   `json:"exists"`
	Mode    string `json:"mode,omitempty"`
	Size    int64  `json:"size,omitempty"`
	ModTime string `json:"mod_time,omitempty"`
	Error   string `json:"error,omitempty"`
}

type RendererProbeAttempt struct {
	URL         string        `json:"url"`
	Source      string        `json:"source"`
	OK          bool          `json:"ok"`
	Status      int           `json:"status,omitempty"`
	ContentType string        `json:"content_type,omitempty"`
	ElapsedMS   int64         `json:"elapsed_ms"`
	Error       string        `json:"error,omitempty"`
	BodyPrefix  string        `json:"body_prefix,omitempty"`
	Renderer    *RendererInfo `json:"renderer,omitempty"`
}

type RendererDiagnostics struct {
	GeneratedAt     string                 `json:"generated_at"`
	LocalIPs        []string               `json:"local_ips"`
	Interfaces      []LocalInterfaceInfo   `json:"interfaces"`
	Listening       []ListenerInfo         `json:"listening"`
	PlayerProcesses []PlayerProcessInfo    `json:"player_processes"`
	ControlSocket   SocketInfo             `json:"control_socket"`
	MQueueSocket    SocketInfo             `json:"mqueue_player"`
	SSDP            []SSDPDiagnostic       `json:"ssdp"`
	Probes          []RendererProbeAttempt `json:"probes"`
	Renderer        *RendererInfo          `json:"renderer,omitempty"`
	RendererError   string                 `json:"renderer_error,omitempty"`
	Override        string                 `json:"renderer_override,omitempty"`
}

type SSDPDiagnostic struct {
	Location string `json:"location"`
	ST       string `json:"st,omitempty"`
	USN      string `json:"usn,omitempty"`
	From     string `json:"from,omitempty"`
	Local    bool   `json:"local"`
}

type PlayerDBInfo struct {
	Path     string `json:"path"`
	Exists   bool   `json:"exists"`
	Size     int64  `json:"size,omitempty"`
	CopiedTo string `json:"copied_to,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	Error    string `json:"error,omitempty"`
}

// exportPlayerDB copies the stock player's SQLite catalogue to the TF card.
// The playlist importer stores the selected M3U paths there; having the file
// available makes it possible to diagnose title/path problems without SSH.
func (a *App) exportPlayerDB() PlayerDBInfo {
	info := PlayerDBInfo{Path: "/usr/data/fiio/db/song.db"}
	st, err := os.Stat(info.Path)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	info.Exists = true
	info.Size = st.Size()
	if a.playlistRoot == "" {
		info.Error = "播放列表目录为空"
		return info
	}
	dest := filepath.Join(a.playlistRoot, "DISC播放器数据库", "song.db")
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		info.Error = err.Error()
		return info
	}
	src, err := os.Open(info.Path)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	defer src.Close()
	tmp := dest + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(out, h), src)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		if copyErr != nil {
			info.Error = copyErr.Error()
		} else {
			info.Error = closeErr.Error()
		}
		return info
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		info.Error = err.Error()
		return info
	}
	info.CopiedTo = dest
	info.SHA256 = hex.EncodeToString(h.Sum(nil))
	return info
}

type rendererCandidate struct {
	URL      string
	Source   string
	Timeout  time.Duration
	Priority int
}

func (a *App) localInterfaceInfos() []LocalInterfaceInfo {
	interfaces, _ := net.Interfaces()
	out := make([]LocalInterfaceInfo, 0, len(interfaces))
	for _, iface := range interfaces {
		info := LocalInterfaceInfo{Name: iface.Name, Flags: iface.Flags.String()}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			info.Addrs = append(info.Addrs, addr.String())
		}
		sort.Strings(info.Addrs)
		out = append(out, info)
	}
	return out
}

func (a *App) localIPv4List() []string {
	seen := make(map[string]bool)
	var out []string
	add := func(ip net.IP) {
		ip4 := ip.To4()
		if ip4 == nil {
			return
		}
		s := ip4.String()
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	add(net.IPv4(127, 0, 0, 1))
	interfaces, _ := net.Interfaces()
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				add(ipnet.IP)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i] == "127.0.0.1" {
			return true
		}
		if out[j] == "127.0.0.1" {
			return false
		}
		return out[i] < out[j]
	})
	return out
}

// listeningPortInfos reads /proc/net/tcp{,6} and maps socket inodes to the
// process that owns them. This is the diagnostic version of listeningPorts().
func (a *App) listeningPortInfos() []ListenerInfo {
	var listeners []ListenerInfo
	inodes := make(map[string]bool)
	for _, spec := range []struct {
		path  string
		proto string
	}{
		{"/proc/net/tcp", "tcp"},
		{"/proc/net/tcp6", "tcp6"},
	} {
		data, err := os.ReadFile(spec.path)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for _, line := range lines[1:] {
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[3] != "0A" { // TCP_LISTEN
				continue
			}
			local := fields[1]
			idx := strings.LastIndex(local, ":")
			if idx < 0 {
				continue
			}
			port64, err := strconv.ParseInt(local[idx+1:], 16, 32)
			if err != nil || port64 <= 0 || port64 > 65535 {
				continue
			}
			uid, _ := strconv.Atoi(fields[7])
			info := ListenerInfo{
				Proto:   spec.proto,
				Address: procNetAddress(spec.proto, local[:idx]),
				Port:    int(port64),
				UID:     uid,
				Inode:   fields[9],
			}
			listeners = append(listeners, info)
			if info.Inode != "" && info.Inode != "0" {
				inodes[info.Inode] = true
			}
		}
	}

	owners := map[string]struct {
		pid  int
		comm string
	}{}
	if len(inodes) > 0 {
		for _, pid := range allProcessIDs() {
			fdDir := fmt.Sprintf("/proc/%d/fd", pid)
			entries, err := os.ReadDir(fdDir)
			if err != nil {
				continue
			}
			comm := readTrimmed(fmt.Sprintf("/proc/%d/comm", pid))
			for _, entry := range entries {
				target, err := os.Readlink(filepath.Join(fdDir, entry.Name()))
				if err != nil || !strings.HasPrefix(target, "socket:[") {
					continue
				}
				inode := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
				if inodes[inode] {
					if _, exists := owners[inode]; !exists {
						owners[inode] = struct {
							pid  int
							comm string
						}{pid: pid, comm: comm}
					}
				}
			}
		}
	}

	for i := range listeners {
		if owner, ok := owners[listeners[i].Inode]; ok {
			listeners[i].PID = owner.pid
			listeners[i].Comm = owner.comm
		}
	}
	sort.Slice(listeners, func(i, j int) bool {
		if listeners[i].Port != listeners[j].Port {
			return listeners[i].Port < listeners[j].Port
		}
		return listeners[i].Address < listeners[j].Address
	})
	return listeners
}

func procNetAddress(proto, hexAddr string) string {
	if proto == "tcp" && len(hexAddr) == 8 {
		b, err := hex.DecodeString(hexAddr)
		if err == nil && len(b) == 4 {
			return net.IPv4(b[3], b[2], b[1], b[0]).String()
		}
	}
	if proto == "tcp6" && len(hexAddr) == 32 {
		// /proc/net/tcp6 uses four little-endian 32-bit groups.
		b, err := hex.DecodeString(hexAddr)
		if err == nil && len(b) == 16 {
			ip := make(net.IP, 16)
			for group := 0; group < 4; group++ {
				for i := 0; i < 4; i++ {
					ip[group*4+i] = b[group*4+3-i]
				}
			}
			if v4 := ip.To4(); v4 != nil {
				return v4.String()
			}
			return ip.String()
		}
	}
	return hexAddr
}

func allProcessIDs() []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	out := make([]int, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err == nil && pid > 0 {
			out = append(out, pid)
		}
	}
	return out
}

func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (a *App) processIDsMatching(term string) []int {
	term = strings.ToLower(strings.TrimSpace(term))
	var out []int
	for _, pid := range allProcessIDs() {
		comm := strings.ToLower(readTrimmed(fmt.Sprintf("/proc/%d/comm", pid)))
		cmdline := strings.ToLower(strings.ReplaceAll(readTrimmed(fmt.Sprintf("/proc/%d/cmdline", pid)), "\x00", " "))
		if strings.Contains(comm, term) || strings.Contains(cmdline, term) {
			out = append(out, pid)
		}
	}
	sort.Ints(out)
	return out
}

func (a *App) playerProcesses() []PlayerProcessInfo {
	var out []PlayerProcessInfo
	seen := make(map[int]bool)
	for _, term := range []string{"mq_player", "mq-player"} {
		for _, pid := range a.processIDsMatching(term) {
			if seen[pid] {
				continue
			}
			seen[pid] = true
			out = append(out, processInfo(pid))
		}
	}
	return out
}

func processInfo(pid int) PlayerProcessInfo {
	info := PlayerProcessInfo{
		PID:     pid,
		Comm:    readTrimmed(fmt.Sprintf("/proc/%d/comm", pid)),
		Cmdline: strings.TrimSpace(strings.ReplaceAll(readTrimmed(fmt.Sprintf("/proc/%d/cmdline", pid)), "\x00", " ")),
	}
	if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		s := string(stat)
		if end := strings.LastIndex(s, ")"); end >= 0 {
			fields := strings.Fields(s[end+1:])
			if len(fields) > 0 {
				info.State = fields[0]
			}
			if len(fields) > 17 {
				info.Threads, _ = strconv.Atoi(fields[17])
			}
		}
	}
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err == nil {
		for _, entry := range entries {
			if len(info.FDs) >= 80 {
				info.FDs = append(info.FDs, "…")
				break
			}
			target, err := os.Readlink(filepath.Join(fdDir, entry.Name()))
			if err == nil {
				info.FDs = append(info.FDs, entry.Name()+" -> "+target)
			}
		}
	}
	return info
}

func (a *App) controlSocketInfo() SocketInfo {
	info := SocketInfo{Path: "/tmp/glyphix_player.sock"}
	st, err := os.Lstat(info.Path)
	if err != nil {
		if !os.IsNotExist(err) {
			info.Error = err.Error()
		}
		return info
	}
	info.Exists = true
	info.Mode = st.Mode().String()
	info.Size = st.Size()
	info.ModTime = st.ModTime().Format("2006-01-02 15:04:05")
	return info
}

func (a *App) rawHTTPGet(ctx context.Context, rawURL string, max int64) (int, string, []byte, time.Duration, error) {
	start := time.Now()
	if err := validateHTTPURL(rawURL); err != nil {
		return 0, "", nil, time.Since(start), err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, "", nil, time.Since(start), err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	client := a.soapClient
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", nil, time.Since(start), err
	}
	defer resp.Body.Close()
	if max <= 0 {
		max = 64 << 10
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, max))
	elapsed := time.Since(start)
	if readErr != nil {
		return resp.StatusCode, resp.Header.Get("Content-Type"), body, elapsed, readErr
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), body, elapsed, nil
}

func rendererURLVariants(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return []string{raw}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return []string{raw}
	}
	if u.Path == "" || u.Path == "/" {
		desc := *u
		desc.Path = "/description.xml"
		return []string{desc.String(), u.String()}
	}
	if strings.HasSuffix(u.Path, "/") {
		desc := *u
		desc.Path += "description.xml"
		return []string{desc.String(), u.String()}
	}
	return []string{u.String()}
}

func truncateForJSON(b []byte, max int) string {
	s := strings.TrimSpace(string(b))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func rendererAttemptSummary(attempts []RendererProbeAttempt, fallback string) string {
	var parts []string
	seen := make(map[string]bool)
	for _, at := range attempts {
		if at.Error == "" || seen[at.URL] {
			continue
		}
		seen[at.URL] = true
		msg := at.URL
		if at.Status > 0 {
			msg += fmt.Sprintf(" (HTTP %d)", at.Status)
		}
		msg += ": " + at.Error
		parts = append(parts, msg)
		if len(parts) >= 3 {
			break
		}
	}
	if len(parts) == 0 {
		return fallback
	}
	return strings.Join(parts, "; ")
}

func (a *App) probeOneRendererURL(ctx context.Context, rawURL, source string, timeout time.Duration) RendererProbeAttempt {
	att := RendererProbeAttempt{URL: rawURL, Source: source}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	status, ct, body, elapsed, err := a.rawHTTPGet(pctx, rawURL, 64<<10)
	att.ElapsedMS = elapsed.Milliseconds()
	att.Status = status
	att.ContentType = ct
	if len(body) > 0 {
		att.BodyPrefix = truncateForJSON(body, 360)
	}
	if err != nil {
		att.Error = err.Error()
		return att
	}
	if status < 200 || status >= 300 {
		att.Error = fmt.Sprintf("HTTP %d", status)
		return att
	}
	r, err := parseRendererDescription(body, rawURL, source)
	if err != nil {
		att.Error = err.Error()
		return att
	}
	att.OK = true
	att.Renderer = r
	return att
}

func (a *App) probeRendererURL(ctx context.Context, rawURL, source string, timeout time.Duration) []RendererProbeAttempt {
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}
	var out []RendererProbeAttempt
	for _, u := range rendererURLVariants(rawURL) {
		select {
		case <-ctx.Done():
			out = append(out, RendererProbeAttempt{URL: u, Source: source, Error: ctx.Err().Error()})
			return out
		default:
		}
		out = append(out, a.probeOneRendererURL(ctx, u, source, timeout))
	}
	return out
}

func dedupeCandidates(in []rendererCandidate) []rendererCandidate {
	seen := make(map[string]bool)
	out := make([]rendererCandidate, 0, len(in))
	for _, c := range in {
		key := c.URL
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	return out
}

func (a *App) probeRendererCandidates(ctx context.Context, candidates []rendererCandidate, all bool) (*RendererInfo, []RendererProbeAttempt) {
	candidates = dedupeCandidates(candidates)
	if len(candidates) == 0 {
		return nil, nil
	}
	type result struct {
		att RendererProbeAttempt
	}
	ch := make(chan result, len(candidates)*2)
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, cand := range candidates {
		wg.Add(1)
		go func(c rendererCandidate) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-cctx.Done():
				ch <- result{att: RendererProbeAttempt{URL: c.URL, Source: c.Source, Error: cctx.Err().Error()}}
				return
			}
			for _, att := range a.probeRendererURL(cctx, c.URL, c.Source, c.Timeout) {
				ch <- result{att: att}
				if att.Renderer != nil {
					if !all {
						cancel()
					}
					return
				}
			}
		}(cand)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	var attempts []RendererProbeAttempt
	var found *RendererInfo
	for {
		select {
		case res := <-ch:
			attempts = append(attempts, res.att)
			if res.att.Renderer != nil && found == nil {
				found = res.att.Renderer
				if !all {
					cancel()
					<-done
					return found, attempts
				}
			}
		case <-done:
			for {
				select {
				case res := <-ch:
					attempts = append(attempts, res.att)
					if res.att.Renderer != nil && found == nil {
						found = res.att.Renderer
					}
				default:
					return found, attempts
				}
			}
		case <-ctx.Done():
			cancel()
			<-done
			for {
				select {
				case res := <-ch:
					attempts = append(attempts, res.att)
					if res.att.Renderer != nil && found == nil {
						found = res.att.Renderer
					}
				default:
					return found, attempts
				}
			}
		}
	}
}

func (a *App) rendererCandidates(includeSSDP bool) ([]rendererCandidate, []SSDPDiagnostic) {
	candidates := []rendererCandidate{
		{URL: "http://127.0.0.1:1286/description.xml", Source: "local-1286", Timeout: 2200 * time.Millisecond, Priority: 0},
		{URL: "http://127.0.0.1:1286/", Source: "local-1286-root", Timeout: 2200 * time.Millisecond, Priority: 1},
	}
	for _, ip := range a.localIPv4List() {
		if ip == "127.0.0.1" {
			continue
		}
		candidates = append(candidates,
			rendererCandidate{URL: "http://" + ip + ":1286/description.xml", Source: "local-ip-1286", Timeout: 1500 * time.Millisecond, Priority: 2},
			rendererCandidate{URL: "http://" + ip + ":1286/", Source: "local-ip-1286-root", Timeout: 1500 * time.Millisecond, Priority: 3},
		)
	}
	for _, port := range listeningPorts() {
		if port == a.port || port < 1024 || port == 1286 || port == 22 || port == 139 || port == 445 || port == 1900 {
			continue
		}
		candidates = append(candidates,
			rendererCandidate{URL: fmt.Sprintf("http://127.0.0.1:%d/description.xml", port), Source: "local-port", Timeout: 1000 * time.Millisecond, Priority: 10},
			rendererCandidate{URL: fmt.Sprintf("http://127.0.0.1:%d/", port), Source: "local-port-root", Timeout: 1000 * time.Millisecond, Priority: 11},
		)
	}
	var ssdp []SSDPDiagnostic
	if includeSSDP {
		ctx, cancel := context.WithTimeout(a.ctx, 4*time.Second)
		results := a.ssdpSearch(ctx)
		cancel()
		for _, res := range results {
			ssdp = append(ssdp, SSDPDiagnostic{Location: res.Location, ST: res.ST, USN: res.USN, From: res.From, Local: res.Local})
			if res.Location == "" {
				continue
			}
			source := "ssdp-remote"
			if res.Local || isLocalURLHost(res.Location) {
				source = "ssdp-local"
			}
			candidates = append(candidates,
				rendererCandidate{URL: res.Location, Source: source, Timeout: 1500 * time.Millisecond, Priority: 20},
			)
		}
	}
	return dedupeCandidates(candidates), ssdp
}

func (a *App) runRendererDiagnostics(ctx context.Context) *RendererDiagnostics {
	d := &RendererDiagnostics{
		GeneratedAt:     time.Now().Format("2006-01-02 15:04:05"),
		LocalIPs:        a.localIPv4List(),
		Interfaces:      a.localInterfaceInfos(),
		Listening:       a.listeningPortInfos(),
		PlayerProcesses: a.playerProcesses(),
		ControlSocket:   a.controlSocketInfo(),
		MQueueSocket:    checkMQueuePlayer(),
		Override:        a.rendererOverride(),
	}
	candidates, ssdp := a.rendererCandidates(true)
	if override := a.rendererOverride(); override != "" {
		candidates = append([]rendererCandidate{{
			URL: override, Source: "manual", Timeout: 2500 * time.Millisecond, Priority: 0,
		}}, candidates...)
	}
	d.SSDP = ssdp
	found, attempts := a.probeRendererCandidates(ctx, candidates, true)
	d.Probes = attempts
	for _, at := range attempts {
		if at.Renderer != nil && at.Renderer.Source != "ssdp-remote" {
			found = at.Renderer
			break
		}
	}
	if found != nil && found.Source != "ssdp-remote" {
		a.setRenderer(found)
	}
	d.Renderer = a.currentRenderer()
	_, rerr, _ := a.rendererSnapshot()
	d.RendererError = rerr
	if d.Renderer == nil && found != nil && found.Source == "ssdp-remote" {
		d.RendererError = "只发现局域网内其他 UPnP 设备（已忽略）: " + found.FriendlyName
	}
	sort.SliceStable(d.Probes, func(i, j int) bool {
		if d.Probes[i].OK != d.Probes[j].OK {
			return d.Probes[i].OK
		}
		if d.Probes[i].Source != d.Probes[j].Source {
			return d.Probes[i].Source < d.Probes[j].Source
		}
		return d.Probes[i].URL < d.Probes[j].URL
	})
	if d.Renderer != nil {
		d.RendererError = ""
	}
	return d
}

func (a *App) saveRendererDiagnostics(d *RendererDiagnostics) {
	if d == nil {
		return
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return
	}
	_ = writeFileAtomic(filepath.Join(a.dataDir, "renderer-diagnostics.json"), b, 0644)
	if a.playlistRoot != "" {
		_ = writeFileAtomic(filepath.Join(a.playlistRoot, "DISC播放器诊断.json"), b, 0644)
	}
}

func (a *App) logStartupDiagnostics() {
	ctx, cancel := context.WithTimeout(a.ctx, 14*time.Second)
	defer cancel()
	d := a.runRendererDiagnostics(ctx)
	a.saveRendererDiagnostics(d)

	ports := make([]string, 0, len(d.Listening))
	for _, l := range d.Listening {
		item := fmt.Sprintf("%d", l.Port)
		if l.PID > 0 {
			item += "/" + l.Comm
		}
		ports = append(ports, item)
	}
	procs := make([]string, 0, len(d.PlayerProcesses))
	for _, p := range d.PlayerProcesses {
		procs = append(procs, fmt.Sprintf("%d(%s)", p.PID, p.Comm))
	}
	sock := d.ControlSocket.Path
	if !d.ControlSocket.Exists {
		sock += "(不存在)"
	}
	a.log.Infof("启动诊断: IP=%s 监听=%s mq_player=%s 控制socket=%s",
		strings.Join(d.LocalIPs, ","), strings.Join(ports, ","), strings.Join(procs, ","), sock)
	for _, p := range d.Probes {
		if p.Source == "local-1286" || p.Source == "local-1286-root" || p.Source == "manual" || p.OK {
			msg := fmt.Sprintf("renderer 探针 %s -> ", p.URL)
			if p.OK {
				msg += "OK " + p.Renderer.FriendlyName
			} else if p.Error != "" {
				msg += p.Error
			} else {
				msg += fmt.Sprintf("HTTP %d", p.Status)
			}
			a.log.Infof("%s (%dms)", msg, p.ElapsedMS)
		}
	}
	if d.Renderer != nil {
		a.log.Infof("启动诊断已连接播放器: %s (%s)", d.Renderer.FriendlyName, d.Renderer.ControlURL)
	} else {
		a.log.Warnf("启动诊断未找到播放器: %s", d.RendererError)
	}
}
