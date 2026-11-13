package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	serviceAVTransport     = "urn:schemas-upnp-org:service:AVTransport:1"
	serviceRendering       = "urn:schemas-upnp-org:service:RenderingControl:1"
	serviceConnectionMgr   = "urn:schemas-upnp-org:service:ConnectionManager:1"
	ssdpMediaRenderer      = "urn:schemas-upnp-org:device:MediaRenderer:1"
	ssdpMulticastAddress   = "239.255.255.250:1900"
	rendererStaleAfter     = 3 * time.Minute
	discoveryRetryInterval = 8 * time.Second
)

type RendererInfo struct {
	Location            string    `json:"location"`
	ControlURL          string    `json:"control_url"`
	EventURL            string    `json:"event_url,omitempty"`
	RenderingControlURL string    `json:"rendering_control_url,omitempty"`
	RenderingEventURL   string    `json:"rendering_event_url,omitempty"`
	ConnectionMgrURL    string    `json:"connection_manager_url,omitempty"`
	FriendlyName        string    `json:"friendly_name"`
	ModelName           string    `json:"model_name"`
	UDN                 string    `json:"udn,omitempty"`
	Source              string    `json:"source"`
	LastSeen            time.Time `json:"last_seen"`
}

type upnpDescription struct {
	Device upnpDevice `xml:"device"`
}

type upnpDevice struct {
	DeviceType   string        `xml:"deviceType"`
	FriendlyName string        `xml:"friendlyName"`
	ModelName    string        `xml:"modelName"`
	UDN          string        `xml:"UDN"`
	ServiceList  []upnpService `xml:"serviceList>service"`
	DeviceList   []upnpDevice  `xml:"deviceList>device"`
}

type upnpService struct {
	ServiceType string `xml:"serviceType"`
	ServiceID   string `xml:"serviceId"`
	SCPDURL     string `xml:"SCPDURL"`
	ControlURL  string `xml:"controlURL"`
	EventSubURL string `xml:"eventSubURL"`
}

type ssdpResult struct {
	Location string
	ST       string
	USN      string
	From     string
	Local    bool
}

func (a *App) currentRenderer() *RendererInfo {
	a.rendererMu.RLock()
	defer a.rendererMu.RUnlock()
	if a.renderer == nil {
		return nil
	}
	r := *a.renderer
	return &r
}

func (a *App) rendererSnapshot() (*RendererInfo, string, time.Time) {
	a.rendererMu.RLock()
	defer a.rendererMu.RUnlock()
	var r *RendererInfo
	if a.renderer != nil {
		c := *a.renderer
		r = &c
	}
	return r, a.rendererErr, a.rendererChecked
}

func (a *App) setRenderer(r *RendererInfo) {
	a.rendererMu.Lock()
	if r == nil {
		a.renderer = nil
		a.rendererMu.Unlock()
		return
	}
	now := time.Now()
	r.LastSeen = now
	r.Location = localizeURL(r.Location)
	r.ControlURL = localizeURL(r.ControlURL)
	r.EventURL = localizeURL(r.EventURL)
	r.RenderingControlURL = localizeURL(r.RenderingControlURL)
	r.RenderingEventURL = localizeURL(r.RenderingEventURL)
	r.ConnectionMgrURL = localizeURL(r.ConnectionMgrURL)

	old := a.renderer
	a.renderer = r
	a.rendererErr = ""
	a.rendererChecked = now
	a.rememberRendererPortLocked(portFromURL(r.ControlURL))
	a.rendererMu.Unlock()

	port := portFromURL(r.ControlURL)
	a.mu.Lock()
	a.status.UPnPPort = port
	a.mu.Unlock()

	if old == nil || old.ControlURL != r.ControlURL || old.FriendlyName != r.FriendlyName {
		a.log.Infof("发现播放器 renderer: %s (%s) source=%s control=%s", r.FriendlyName, r.ModelName, r.Source, r.ControlURL)
	} else {
		a.log.Debugf("renderer 心跳正常: %s", r.ControlURL)
	}
	go a.refreshVolume(r)
}

func (a *App) setRendererError(err error, checked time.Time) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	a.rendererMu.Lock()
	first := a.rendererErr != msg
	a.rendererErr = msg
	a.rendererChecked = checked
	a.rendererMu.Unlock()
	if first && msg != "" {
		a.log.Warnf("未找到可用播放器 renderer: %s", msg)
	}
}

func (a *App) discoveryLoop() {
	failStreak := 0
	for {
		select {
		case <-a.ctx.Done():
			return
		default:
		}

		// A playback request discovers the renderer itself. Skipping the
		// background scan while one is running keeps it from stealing the
		// discovery lock and adding seconds to start-up latency.
		if atomic.LoadInt32(&a.playInFlight) == 1 {
			select {
			case <-a.ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}

		// If the cached port still answers, a full SSDP round is pointless.
		cachedAlive := false
		var deadPort int
		if r := a.currentRenderer(); r != nil {
			if hp := hostPortFromURL(r.ControlURL); hp != "" {
				if conn, err := net.DialTimeout("tcp4", hp, 250*time.Millisecond); err == nil {
					conn.Close()
					a.touchRenderer(r)
					cachedAlive = true
				} else {
					// mq_player rotates its port monotonically (+1) on each restart.
					// Pre-probe port+1 in the background so it is ready in the
					// cache the next time ensureRenderer is called.
					deadPort = portFromURL(r.ControlURL)
				}
			}
		}
		if deadPort > 0 {
			go a.preProbeNextPort(deadPort)
		}
		if !cachedAlive {
			ctx, cancel := context.WithTimeout(a.ctx, 9*time.Second)
			r, err := a.discoverRenderer(ctx)
			cancel()
			if r != nil {
				a.setRenderer(r)
				failStreak = 0
			} else {
				a.setRendererError(err, time.Now())
				failStreak++
			}
		} else {
			failStreak = 0
		}

		delay := discoveryRetryInterval
		if a.currentRenderer() != nil {
			// mq_player restarts often; a 15s heartbeat notices a rotated port
			// quickly while still costing only a TCP dial.
			delay = 15 * time.Second
		} else {
			// A renderer that stays gone used to be rescanned every 8s with a
			// 9s SSDP + port sweep, which kept the CPU and Wi-Fi busy forever.
			// Back off to one attempt per minute instead.
			delay = reconnectDelay(failStreak-1, discoveryRetryInterval, 60*time.Second)
			st := a.getStatus()
			sess := a.session.snapshot()
			if !st.Playing && !sess.Active && delay < 60*time.Second {
				delay = 60 * time.Second
			}
		}
		select {
		case <-a.ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// reconnectDelay grows the interval between renderer discovery attempts so a
// renderer that has gone away cannot saturate the CPU and Wi-Fi with SSDP
// rounds and 10-port scans. doublings is the number of consecutive failures
// minus one; the result is capped at max.
func reconnectDelay(doublings int, base, max time.Duration) time.Duration {
	if base <= 0 {
		return base
	}
	d := base
	for i := 0; i < doublings && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}

var (
	lastPlayerWakeTime time.Time
	lastPlayerWakeMu   sync.Mutex
)

// wakePlayerDLNA is a no-op because mq_ui owns input mode switching and native IPC.
// Sending raw bytes to /dev/mqueue/player corrupts the player queue and causes panics.
func (a *App) wakePlayerDLNA() error {
	return nil
}

func hasMQueuePlayer() bool {
	_, err := os.Stat("/dev/mqueue/player")
	return err == nil
}

func (a *App) ensureRenderer(ctx context.Context, timeout time.Duration) (*RendererInfo, error) {
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if r := a.currentRenderer(); r != nil {
			// Fast active probe to check if the cached renderer's TCP port is still listening
			hp := hostPortFromURL(r.ControlURL)
			if hp != "" {
				conn, err := net.DialTimeout("tcp4", hp, 250*time.Millisecond)
				if err == nil {
					conn.Close()
					a.touchRenderer(r)
					return r, nil
				}
				a.log.Warnf("缓存的播放器端口不可达 (%s: %v)，立即重新探测…", hp, err)
				a.setRenderer(nil)
			}
		}

		// mq_player rotates its UPnP port on every restart (49494 -> 49495 ...).
		// Probing the handful of ports we have seen before takes a few
		// milliseconds and catches the new port immediately, instead of waiting
		// for a full SSDP + local port scan.
		if r := a.probeKnownRendererPorts(ctx); r != nil {
			a.setRenderer(r)
			return r, nil
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, errors.New("没有找到 FiiO DISC 本机的 UPnP AVTransport 服务")
		}
		if remaining > 600*time.Millisecond {
			remaining = 600 * time.Millisecond
		}
		dctx, cancel := context.WithTimeout(ctx, remaining)
		r, _ := a.discoverRenderer(dctx)
		cancel()
		if r != nil {
			a.setRenderer(r)
			return r, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(80 * time.Millisecond):
		}
	}
}

// probeKnownRendererPorts tries the UPnP ports that were recently active plus
// the stock mq_player range, on loopback and the first LAN address only.
//
// Two-phase approach:
//  1. Fast TCP dial all candidate (ip, port) pairs in parallel (~50ms) to
//     discard unreachable ports immediately.
//  2. Fetch the UPnP description XML only from ports whose TCP socket is open.
//
// This avoids stacking N×250ms HTTP timeouts for ports that are not listening.
func (a *App) probeKnownRendererPorts(ctx context.Context) *RendererInfo {
	ports := a.candidateRendererPorts()
	if len(ports) == 0 {
		return nil
	}
	ips := a.localIPv4List()
	if len(ips) > 2 {
		ips = ips[:2]
	}

	// Phase 1: parallel TCP pre-screen – keep only ports with an open socket.
	type ipPort struct{ ip, port string }
	type tcpResult struct {
		ip   string
		port int
		ok   bool
	}
	tcpCh := make(chan tcpResult, len(ports)*len(ips))
	for _, port := range ports {
		for _, ip := range ips {
			go func(ip string, port int) {
				hp := fmt.Sprintf("%s:%d", ip, port)
				conn, err := net.DialTimeout("tcp4", hp, 150*time.Millisecond)
				if err == nil {
					conn.Close()
					tcpCh <- tcpResult{ip: ip, port: port, ok: true}
				} else {
					tcpCh <- tcpResult{ip: ip, port: port, ok: false}
				}
			}(ip, port)
		}
	}
	// Collect all TCP results (they complete in ~150ms regardless of count).
	total := len(ports) * len(ips)
	type aliveKey struct{ ip string; port int }
	alive := make(map[aliveKey]bool, total)
	for i := 0; i < total; i++ {
		res := <-tcpCh
		if res.ok {
			alive[aliveKey{res.ip, res.port}] = true
		}
	}
	if len(alive) == 0 {
		return nil
	}

	// Phase 2: UPnP description fetch only for alive ports.
	candidates := make([]rendererCandidate, 0, len(alive)*2)
	for _, port := range ports { // preserve priority order
		for _, ip := range ips {
			if !alive[aliveKey{ip, port}] {
				continue
			}
			candidates = append(candidates,
				rendererCandidate{URL: fmt.Sprintf("http://%s:%d/description.xml", ip, port), Source: "known-port", Timeout: 250 * time.Millisecond, Priority: 0},
				rendererCandidate{URL: fmt.Sprintf("http://%s:%d/", ip, port), Source: "known-port-root", Timeout: 250 * time.Millisecond, Priority: 1},
			)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	r, _ := a.probeRendererCandidates(ctx, candidates, false)
	return r
}

// preProbeNextPort speculatively probes deadPort+1 (and deadPort+2) in the
// background because mq_player rotates its UPnP port monotonically on every
// restart. If a renderer is found it is stored immediately so that the next
// ensureRenderer call can return from the cached-port fast path.
func (a *App) preProbeNextPort(deadPort int) {
	ips := a.localIPv4List()
	if len(ips) > 2 {
		ips = ips[:2]
	}
	candidates := make([]rendererCandidate, 0, 4*len(ips))
	for _, delta := range []int{1, 2} {
		next := deadPort + delta
		if next <= 0 || next > 65535 {
			continue
		}
		for _, ip := range ips {
			candidates = append(candidates,
				rendererCandidate{URL: fmt.Sprintf("http://%s:%d/description.xml", ip, next), Source: "known-port", Timeout: 300 * time.Millisecond, Priority: 0},
				rendererCandidate{URL: fmt.Sprintf("http://%s:%d/", ip, next), Source: "known-port-root", Timeout: 300 * time.Millisecond, Priority: 1},
			)
		}
	}
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Second)
	defer cancel()
	r, _ := a.probeRendererCandidates(ctx, candidates, false)
	if r != nil {
		a.setRenderer(r)
	}
}

func (a *App) candidateRendererPorts() []int {
	a.rendererMu.RLock()
	remembered := append([]int(nil), a.rendererPorts...)
	a.rendererMu.RUnlock()

	seen := make(map[int]bool, len(remembered)+4)
	out := make([]int, 0, len(remembered)+4)
	for _, p := range remembered {
		if p > 0 && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range listeningPorts() {
		if p >= 49000 && p <= 50000 && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if len(out) > 4 {
		out = out[:4]
	}
	return out
}

func (a *App) rememberRendererPortLocked(port int) {
	if port <= 0 {
		return
	}
	next := make([]int, 0, 4)
	next = append(next, port)
	for _, p := range a.rendererPorts {
		if p != port && len(next) < 4 {
			next = append(next, p)
		}
	}
	a.rendererPorts = next
}

func (a *App) touchRenderer(r *RendererInfo) {
	if r == nil {
		return
	}
	a.rendererMu.Lock()
	if a.renderer != nil && a.renderer.ControlURL == r.ControlURL {
		a.renderer.LastSeen = time.Now()
		a.rendererErr = ""
	}
	a.rendererMu.Unlock()
}

func (a *App) discoverControlOnce(ctx context.Context) (string, error) {
	// Stock renderer binds the Wi-Fi address and rotates ports on re-entry.
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	var ip string
	if wifi, err := net.InterfaceByName("wlan0"); err == nil {
		if wifiAddrs, err := wifi.Addrs(); err == nil {
			addrs = wifiAddrs
		}
	}
	for _, addr := range addrs {
		if n, ok := addr.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() {
			ip = n.IP.String()
			break
		}
	}
	if ip == "" {
		return "", errors.New("请先连接 Wi-Fi")
	}

	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return "", err
	}
	defer conn.Close()

	deadline := time.Now().Add(400 * time.Millisecond)
	if until, ok := ctx.Deadline(); ok && until.Before(deadline) {
		deadline = until
	}
	_ = conn.SetDeadline(deadline)

	dst, err := net.ResolveUDPAddr("udp4", ip+":1900")
	if err != nil {
		return "", err
	}
	msg := "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 1\r\nST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n\r\n"
	_, _ = conn.WriteTo([]byte(msg), dst)

	buf := make([]byte, 4096)
	for {
		n, peer, err := conn.ReadFrom(buf)
		if err != nil {
			return "", errors.New("接收服务尚未就绪，请稍后再试")
		}
		host, _, _ := net.SplitHostPort(peer.String())
		if host != ip {
			continue
		}
		for _, line := range strings.Split(string(buf[:n]), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(strings.ToLower(line), "location:") {
				continue
			}
			parts := strings.SplitN(line, ":", 2)
			if len(parts) < 2 {
				continue
			}
			u, err := url.Parse(strings.TrimSpace(parts[1]))
			if err == nil && u.Hostname() == ip && u.Scheme == "http" {
				return "http://" + u.Host + "/upnp/control/rendertransport1", nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
	}
}

// acquireDiscovery serialises renderer discovery but stays cancellable: a
// playback request must never sit behind a background scan that is already
// running. If ctx is cancelled while waiting, a helper releases the lock as
// soon as it is granted so nothing stays wedged.
func (a *App) acquireDiscovery(ctx context.Context) (func(), error) {
	done := make(chan struct{})
	go func() {
		a.discoverMu.Lock()
		close(done)
	}()
	select {
	case <-done:
		return a.discoverMu.Unlock, nil
	case <-ctx.Done():
		go func() {
			<-done
			a.discoverMu.Unlock()
		}()
		return nil, ctx.Err()
	}
}

func (a *App) discoverRenderer(ctx context.Context) (*RendererInfo, error) {
	release, err := a.acquireDiscovery(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	// 1. First and most reliable: Stock FiiO DISC direct unicast SSDP to local Wi-Fi IP:1900
	if control, err := a.discoverControlOnce(ctx); err == nil && control != "" {
		r := &RendererInfo{
			ControlURL:   control,
			FriendlyName: "FiiO DISC Renderer",
			ModelName:    "X2000",
			Source:       "stock-unicast",
			LastSeen:     time.Now(),
		}
		return r, nil
	}

	var errs []string

	// 2. User-provided address takes precedence
	if override := strings.TrimSpace(a.rendererOverride()); override != "" {
		candidates := []rendererCandidate{{
			URL: override, Source: "manual", Timeout: 3 * time.Second, Priority: 0,
		}}
		r, attempts := a.probeRendererCandidates(ctx, candidates, false)
		if r != nil {
			return r, nil
		}
		errs = append(errs, "手动地址: "+rendererAttemptSummary(attempts, override))
	}

	// 3. Fallback to local listening ports
	if r, err := a.discoverLocal(ctx); r != nil {
		return r, nil
	} else if err != nil {
		errs = append(errs, "本机扫描: "+err.Error())
	}
	if r, err := a.discoverSSDP(ctx); r != nil {
		return r, nil
	} else if err != nil {
		errs = append(errs, "SSDP: "+err.Error())
	}
	if len(errs) == 0 {
		return nil, errors.New("本机扫描和 SSDP 均未发现 mq_player")
	}
	return nil, errors.New(strings.Join(errs, "; "))
}

func (a *App) discoverLocal(ctx context.Context) (*RendererInfo, error) {
	candidates, _ := a.rendererCandidates(false)
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority < candidates[j].Priority
		}
		return candidates[i].URL < candidates[j].URL
	})
	r, attempts := a.probeRendererCandidates(ctx, candidates, false)
	if r != nil {
		return r, nil
	}
	if len(attempts) == 0 {
		return nil, errors.New("没有可探测的本机地址/端口")
	}
	ports := listeningPorts()
	return nil, fmt.Errorf("检查了 %d 个本机监听端口，未找到 UPnP 描述: %s",
		len(ports), rendererAttemptSummary(attempts, "无响应"))
}

func (a *App) discoverSSDP(ctx context.Context) (*RendererInfo, error) {
	results := a.ssdpSearch(ctx)
	if len(results) == 0 {
		return nil, errors.New("SSDP 没有收到响应")
	}
	var candidates []rendererCandidate
	seen := make(map[string]bool)
	for _, res := range results {
		location := strings.TrimSpace(res.Location)
		if location == "" || seen[location] {
			continue
		}
		// Automatic playback is only safe on the device itself. A PC or NAS on
		// the same LAN can advertise a MediaRenderer too, but sending the
		// device's audio there would surprise the user.
		if !res.Local && !isLocalURLHost(location) {
			continue
		}
		seen[location] = true
		candidates = append(candidates, rendererCandidate{
			URL: localizeURL(location), Source: "ssdp-local", Timeout: 1500 * time.Millisecond, Priority: 20,
		})
	}
	if len(candidates) == 0 {
		return nil, errors.New("SSDP 响应中没有可用 LOCATION")
	}
	r, attempts := a.probeRendererCandidates(ctx, candidates, false)
	if r != nil {
		return r, nil
	}
	return nil, errors.New("SSDP 描述不可用: " + rendererAttemptSummary(attempts, "无响应"))
}

func (a *App) ssdpSearch(ctx context.Context) []ssdpResult {
	msg := "M-SEARCH * HTTP/1.1\r\n" +
		"HOST: " + ssdpMulticastAddress + "\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: 2\r\n" +
		"ST: " + ssdpMediaRenderer + "\r\n" +
		"USER-AGENT: Linux/3.4 UPnP/1.0 DISC-Podcast-Radio/1.0\r\n\r\n"

	interfaces, _ := net.Interfaces()
	var ips []net.IP
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				if ip4 := ipnet.IP.To4(); ip4 != nil {
					ips = append(ips, ip4)
				}
			}
		}
	}
	if len(ips) == 0 {
		ips = append(ips, net.IPv4zero)
	}

	ch := make(chan ssdpResult, 64)
	var wg sync.WaitGroup
	for _, ip := range ips {
		wg.Add(1)
		go func(bindIP net.IP) {
			defer wg.Done()
			conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: bindIP, Port: 0})
			if err != nil {
				return
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3200 * time.Millisecond))
			dest := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
			_, _ = conn.WriteToUDP([]byte(msg), dest)
			buf := make([]byte, 8192)
			for {
				n, from, err := conn.ReadFromUDP(buf)
				if err != nil {
					return
				}
				headers := parseSSDPHeaders(buf[:n])
				if headers["location"] != "" {
					local := false
					location := headers["location"]
					if from != nil {
						local = isLocalIPv4(from.IP)
						location = replaceUnspecifiedHost(location, from.IP)
					}
					ch <- ssdpResult{
						Location: location,
						ST:       headers["st"],
						USN:      headers["usn"],
						From:     from.String(),
						Local:    local,
					}
				}
			}
		}(ip)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
	case <-done:
	case <-time.After(4 * time.Second):
	}
	// Each socket has a 3.2s read deadline. Wait for the senders before
	// closing the channel so a late SSDP response cannot panic.
	<-done
	close(ch)
	var out []ssdpResult
	for r := range ch {
		out = append(out, r)
	}
	return out
}

func parseSSDPHeaders(b []byte) map[string]string {
	out := make(map[string]string)
	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if i == 0 || line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if ok {
			out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	return out
}

func parseRendererDescription(body []byte, location, source string) (*RendererInfo, error) {
	location = localizeURL(location)
	var desc upnpDescription
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.Strict = false
	if err := dec.Decode(&desc); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", location, err)
	}
	av, ok := findService(desc.Device, serviceAVTransport)
	if !ok {
		return nil, fmt.Errorf("%s 不是 AVTransport renderer", location)
	}
	r := &RendererInfo{
		Location:     location,
		FriendlyName: strings.TrimSpace(desc.Device.FriendlyName),
		ModelName:    strings.TrimSpace(desc.Device.ModelName),
		UDN:          strings.TrimSpace(desc.Device.UDN),
		Source:       source,
	}
	if r.FriendlyName == "" {
		r.FriendlyName = "mq_player"
	}
	if av.ControlURL == "" {
		av.ControlURL = "/upnp/control/rendertransport1"
	}
	r.ControlURL = resolveURL(location, av.ControlURL)
	r.EventURL = resolveURL(location, av.EventSubURL)
	if rc, ok := findService(desc.Device, serviceRendering); ok {
		r.RenderingControlURL = resolveURL(location, rc.ControlURL)
		r.RenderingEventURL = resolveURL(location, rc.EventSubURL)
	}
	if cm, ok := findService(desc.Device, serviceConnectionMgr); ok {
		r.ConnectionMgrURL = resolveURL(location, cm.ControlURL)
	}
	if r.ControlURL == "" {
		return nil, fmt.Errorf("%s 中没有 controlURL", location)
	}
	return r, nil
}

func (a *App) fetchRendererDescription(ctx context.Context, location, source string) (*RendererInfo, error) {
	location = localizeURL(location)
	body, _, err := a.fetchBytes(ctx, location, 2<<20)
	if err != nil {
		return nil, err
	}
	r, err := parseRendererDescription(body, location, source)
	if err != nil {
		return nil, err
	}

	// Best-effort validation. A renderer that is still initializing can return
	// a SOAP fault briefly; its description is still a valid discovery result.
	vctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	_, verr := a.getTransportInfo(vctx, r)
	cancel()
	if verr != nil {
		a.log.Warnf("renderer 描述有效，但 GetTransportInfo 未通过 (%s): %v", r.ControlURL, verr)
	}
	return r, nil
}

func findService(dev upnpDevice, serviceType string) (upnpService, bool) {
	for _, s := range dev.ServiceList {
		if strings.Contains(s.ServiceType, serviceType) || strings.Contains(s.ServiceID, "AVTransport") && strings.Contains(serviceType, "AVTransport") {
			return s, true
		}
	}
	for _, child := range dev.DeviceList {
		if s, ok := findService(child, serviceType); ok {
			return s, true
		}
	}
	return upnpService{}, false
}

func resolveURL(base, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

func isLocalURLHost(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && isLocalIPv4(ip)
}

// localizeURL only replaces the unspecified address. Earlier versions rewrote
// every local interface address to 127.0.0.1, which breaks a player that binds
// its HTTP server to the LAN address only: discovery succeeds on the LAN URL,
// then every SOAP request goes to a loopback port that is not listening. Keep
// the address that actually answered the description request.
func localizeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsUnspecified() {
		return raw
	}
	if port := u.Port(); port != "" {
		u.Host = "127.0.0.1:" + port
	} else {
		u.Host = "127.0.0.1"
	}
	return u.String()
}

func replaceUnspecifiedHost(raw string, ip net.IP) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || ip == nil {
		return raw
	}
	hostIP := net.ParseIP(u.Hostname())
	if hostIP == nil || !hostIP.IsUnspecified() {
		return raw
	}
	host := ip.String()
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else {
		u.Host = host
	}
	return u.String()
}

func isLocalIPv4(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	interfaces, _ := net.Interfaces()
	for _, iface := range interfaces {
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.Equal(ip) {
				return true
			}
		}
	}
	return false
}

func listeningPorts() []int {
	ports := make(map[int]bool)
	for _, file := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		lines := strings.Split(string(b), "\n")
		for _, line := range lines[1:] {
			fields := strings.Fields(line)
			if len(fields) < 4 || fields[3] != "0A" { // TCP_LISTEN
				continue
			}
			local := fields[1]
			idx := strings.LastIndex(local, ":")
			if idx < 0 {
				continue
			}
			p, err := strconv.ParseInt(local[idx+1:], 16, 32)
			if err == nil && p > 0 && p <= 65535 {
				ports[int(p)] = true
			}
		}
	}
	out := make([]int, 0, len(ports))
	for p := range ports {
		out = append(out, p)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(out)))
	return out
}

func portFromURL(raw string) int {
	u, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(u.Port())
	return p
}

func hostPortFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// --- SOAP / AVTransport ---

type soapArg struct {
	Name  string
	Value string
}

func (a *App) soapAction(ctx context.Context, controlURL, serviceType, action string, args ...soapArg) (map[string]string, error) {
	if controlURL == "" {
		return nil, errors.New("controlURL 为空")
	}
	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="utf-8"?>`)
	body.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	body.WriteString(`<u:` + action + ` xmlns:u="` + xmlEscape(serviceType) + `">`)
	for _, arg := range args {
		body.WriteString("<" + arg.Name + ">" + xmlEscape(arg.Value) + "</" + arg.Name + ">")
	}
	body.WriteString(`</u:` + action + `></s:Body></s:Envelope>`)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, controlURL, strings.NewReader(body.String()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPACTION", `"`+serviceType+"#"+action+`"`)
	req.Header.Set("User-Agent", userAgent)
	resp, err := a.soapClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || bytes.Contains(data, []byte("UPnPError")) {
		return nil, soapFault(resp.StatusCode, data)
	}
	values, err := parseSOAPValues(data)
	if err != nil {
		return nil, fmt.Errorf("%s 响应解析失败: %w", action, err)
	}
	return values, nil
}

func soapFault(status int, body []byte) error {
	code := regexp.MustCompile(`(?s)<errorCode>\s*([0-9]+)\s*</errorCode>`).FindSubmatch(body)
	desc := regexp.MustCompile(`(?s)<errorDescription>\s*([^<]+)\s*</errorDescription>`).FindSubmatch(body)
	msg := fmt.Sprintf("UPnP HTTP %d", status)
	if len(code) > 0 {
		msg += " errorCode=" + string(code[1])
	}
	if len(desc) > 0 {
		msg += " " + strings.TrimSpace(string(desc[1]))
	}
	if len(code) == 0 && len(desc) == 0 {
		excerpt := strings.TrimSpace(string(body))
		if len(excerpt) > 240 {
			excerpt = excerpt[:240] + "…"
		}
		if excerpt != "" {
			msg += ": " + excerpt
		}
	}
	return errors.New(msg)
}

func parseSOAPValues(data []byte) (map[string]string, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	values := make(map[string]string)
	var stack []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if len(stack) == 0 {
				continue
			}
			txt := strings.TrimSpace(string(t))
			if txt == "" {
				continue
			}
			name := stack[len(stack)-1]
			if _, exists := values[name]; !exists {
				values[name] = txt
			}
		}
	}
	return values, nil
}

func (a *App) getTransportInfo(ctx context.Context, r *RendererInfo) (map[string]string, error) {
	if r == nil {
		return nil, errors.New("renderer 为空")
	}
	return a.soapAction(ctx, r.ControlURL, serviceAVTransport, "GetTransportInfo", soapArg{"InstanceID", "0"})
}

func (a *App) getMediaInfo(ctx context.Context, r *RendererInfo) (map[string]string, error) {
	return a.soapAction(ctx, r.ControlURL, serviceAVTransport, "GetMediaInfo", soapArg{"InstanceID", "0"})
}

func (a *App) getPositionInfo(ctx context.Context, r *RendererInfo) (map[string]string, error) {
	return a.soapAction(ctx, r.ControlURL, serviceAVTransport, "GetPositionInfo", soapArg{"InstanceID", "0"})
}

func (a *App) stopRenderer(ctx context.Context, r *RendererInfo) error {
	if r == nil {
		return errors.New("renderer 为空")
	}
	_, err := a.soapAction(ctx, r.ControlURL, serviceAVTransport, "Stop", soapArg{"InstanceID", "0"})
	return err
}

func (a *App) pauseRenderer(ctx context.Context, r *RendererInfo) error {
	if r == nil {
		return errors.New("renderer 为空")
	}
	_, err := a.soapAction(ctx, r.ControlURL, serviceAVTransport, "Pause", soapArg{"InstanceID", "0"})
	return err
}

func (a *App) rendererSeek(ctx context.Context, r *RendererInfo, target string) error {
	if r == nil {
		return errors.New("renderer 为空")
	}
	_, err := a.soapAction(ctx, r.ControlURL, serviceAVTransport, "Seek",
		soapArg{"InstanceID", "0"},
		soapArg{"Unit", "REL_TIME"},
		soapArg{"Target", target},
	)
	return err
}

func (a *App) setAVTransportURI(ctx context.Context, r *RendererInfo, mediaURL, metadata string) error {
	if r == nil {
		return errors.New("renderer 为空")
	}
	_, err := a.soapAction(ctx, r.ControlURL, serviceAVTransport, "SetAVTransportURI",
		soapArg{"InstanceID", "0"},
		soapArg{"CurrentURI", mediaURL},
		soapArg{"CurrentURIMetaData", metadata},
	)
	return err
}

func (a *App) rendererPlay(ctx context.Context, r *RendererInfo) error {
	if r == nil {
		return errors.New("renderer 为空")
	}
	_, err := a.soapAction(ctx, r.ControlURL, serviceAVTransport, "Play",
		soapArg{"InstanceID", "0"}, soapArg{"Speed", "1"})
	return err
}

func (a *App) refreshVolume(r *RendererInfo) {
	if r == nil || r.RenderingControlURL == "" {
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
	defer cancel()
	vals, err := a.soapAction(ctx, r.RenderingControlURL, serviceRendering, "GetVolume",
		soapArg{"InstanceID", "0"}, soapArg{"Channel", "Master"})
	if err != nil {
		a.log.Debugf("读取播放器音量失败: %v", err)
		return
	}
	if v, err := strconv.Atoi(vals["CurrentVolume"]); err == nil && v >= 0 && v <= 100 {
		a.mu.Lock()
		a.status.Volume = v
		a.mu.Unlock()
	}
}

func (a *App) SetVolume(volume int) error {
	if volume < 0 {
		volume = 0
	}
	if volume > 100 {
		volume = 100
	}
	ctx, ctxCancel := context.WithTimeout(a.ctx, 9*time.Second)
	defer ctxCancel()
	r, err := a.ensureRenderer(ctx, 5*time.Second)
	if err != nil {
		return err
	}
	if r.RenderingControlURL == "" {
		return errors.New("播放器没有暴露 RenderingControl 服务")
	}
	sctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	_, err = a.soapAction(sctx, r.RenderingControlURL, serviceRendering, "SetVolume",
		soapArg{"InstanceID", "0"}, soapArg{"Channel", "Master"}, soapArg{"DesiredVolume", strconv.Itoa(volume)})
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.status.Volume = volume
	a.mu.Unlock()
	a.log.Infof("音量设置为 %d", volume)
	return nil
}

// --- Playback status polling / recovery ---

func (a *App) statusPollLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	stoppedCount := 0
	failStreak := 0
	nextProbe := time.Time{}
	lastGen := int64(-1)
	lastRecovery := time.Time{}
	recoveries := 0
	lastBreakpointSave := time.Time{}
	lastBroadcastPlaying := false
	lastProgressLog := time.Time{}
	lastPlayingAt := time.Time{}
	lastPositionInfo := time.Time{}
	lastDurationLookup := time.Time{}

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
		}

		st := a.getStatus()
		sess := a.session.snapshot()
		now := time.Now()

		// A seek is in flight. mq_player needs ~0.5s to run its debounce thread
		// and then reopens the stream; probing UPnP inside that window is what
		// pushed its server into the 49494 -> 49495 -> 49496 port churn.
		if a.seekCheck.holding(now) {
			continue
		}

		// A new play request must never inherit an old reconnect backoff.
		if gen := a.currentPlayGen(); gen != lastGen {
			lastGen = gen
			failStreak = 0
			nextProbe = time.Time{}
		}

		if st.ProxyURL == "" {
			stoppedCount = 0
			failStreak = 0
			nextProbe = time.Time{}
			if lastBroadcastPlaying {
				lastBroadcastPlaying = false
				a.broadcastPlaybackState("stopped", 0, 0, false)
				go a.saveBreakpoints()
			}
			continue
		}
		// Keep polling while a podcast session is alive even if the renderer
		// briefly reports STOPPED: that is how mq_player behaves while it
		// rebinds its UPnP port, and it used to kill breakpoint recording.
		if !st.Playing && !sess.Active {
			stoppedCount = 0
			failStreak = 0
			nextProbe = time.Time{}
			if lastBroadcastPlaying {
				lastBroadcastPlaying = false
				a.broadcastPlaybackState("stopped", 0, 0, false)
				go a.saveBreakpoints()
			}
			continue
		}

		// Do not probe a dead renderer every second. This is the exact loop
		// that produced the 14:09:41 -> 14:10:15 retry storm in the field.
		// A user tap bypasses it: PlayStream calls ensureRenderer directly.
		if !nextProbe.IsZero() && now.Before(nextProbe) {
			continue
		}

		// Record a conservative fallback before touching the renderer, so a
		// port restart cannot lose the last known position.
		if sess.Active && sess.MediaType == "podcast" && sess.StreamURL != "" {
			fallback := a.session.estimate(now, st.Playing)
			if fallback >= 5 && !durationReachedEnd(fallback, sess.Duration) {
				a.setBreakpoint(sess.StreamURL, sess.Title, fallback, sess.Duration, sess.Subtitle)
				if now.Sub(lastBreakpointSave) > 10*time.Second {
					lastBreakpointSave = now
					go a.saveBreakpoints()
				}
			}
		}

		r := a.currentRenderer()
		if r == nil {
			if fresh := a.probeKnownRendererPorts(a.ctx); fresh != nil {
				a.setRenderer(fresh)
				r = fresh
			}
		}
		if r == nil {
			if _, err := a.ensureRenderer(a.ctx, 4*time.Second); err != nil {
				a.log.Warnf("播放中 renderer 丢失: %v", err)
				a.mu.Lock()
				a.status.RendererState = "RECONNECTING"
				a.status.UpdatedAt = now.Format("2006-01-02 15:04:05")
				a.mu.Unlock()
				failStreak++
				nextProbe = time.Now().Add(reconnectDelay(failStreak-1, time.Second, 60*time.Second))
				continue
			}
			r = a.currentRenderer()
		}
		if r == nil {
			continue
		}

		ctx, cancel := context.WithTimeout(a.ctx, 1800*time.Millisecond)
		vals, err := a.getTransportInfo(ctx, r)
		cancel()
		if err != nil {
			stoppedCount++
			a.log.Warnf("查询播放器状态失败 (%d): %v", stoppedCount, err)
			// A single probe of the known port range is much cheaper than a
			// full SSDP scan and usually finds mq_player's new port at once.
			if fresh := a.probeKnownRendererPorts(a.ctx); fresh != nil {
				a.setRenderer(fresh)
				r = fresh
				failStreak = 0
				nextProbe = time.Time{}
			} else {
				if stoppedCount >= 8 {
					a.setRenderer(nil)
					stoppedCount = 0
				}
				failStreak++
				nextProbe = time.Now().Add(reconnectDelay(failStreak-1, time.Second, 60*time.Second))
			}
			a.mu.Lock()
			if a.status.RendererState == "" || a.status.RendererState == "PLAYING" {
				a.status.RendererState = "RECONNECTING"
			}
			a.status.UpdatedAt = now.Format("2006-01-02 15:04:05")
			a.mu.Unlock()
			continue
		}
		stoppedCount = 0
		failStreak = 0
		nextProbe = time.Time{}
		state := vals["CurrentTransportState"]
		a.touchRenderer(r)

		posSec := st.Position
		durSec := st.Duration
		source := "cached"
		relPos := -1

		if state == "PLAYING" || state == "PAUSED_PLAYBACK" || state == "TRANSITIONING" {
			// The ALSA frame counter is the cheapest and most reliable clock
			// for network streams. When it is available, poll
			// GetPositionInfo only every 10 seconds: mq_player's SOAP server
			// is fragile and frequent position requests contributed to its
			// UPnP port restarts. Without a PCM clock we still need every
			// second to fall back to RelTime.
			pcmPos, pcmOK := 0, false
			if state == "PLAYING" || state == "PAUSED_PLAYBACK" {
				pcmPos, pcmOK = a.pcm.position(state)
			}
			if !pcmOK || now.Sub(lastPositionInfo) >= 10*time.Second {
				posCtx, posCancel := context.WithTimeout(a.ctx, 1500*time.Millisecond)
				posVals, posErr := a.getPositionInfo(posCtx, r)
				posCancel()
				lastPositionInfo = now
				if posErr == nil {
					relPos = parseTimeSeconds(posVals["RelTime"])
					durSec = sanitizeDuration(durSec, parseTimeSeconds(posVals["TrackDuration"]))
				} else {
					a.log.Debugf("GetPositionInfo 失败: %v", posErr)
				}
			}

			// mq_player usually reports RelTime=00:00:00 for network streams,
			// mq_player usually reports RelTime=00:00:00 for network streams,
			// so prefer the ALSA frame counter, then RelTime, then wall clock.
			if state == "PLAYING" || state == "PAUSED_PLAYBACK" {
				if pcmOK {
					posSec = pcmPos
					source = "pcm"
				} else if relPos > 0 {
					posSec = relPos
					source = "reltime"
				} else if sess.Active && sess.SeenPlaying {
					posSec = a.session.estimate(now, state == "PLAYING")
					source = "wall"
				}
			} else if relPos > 0 {
				posSec = relPos
				source = "reltime"
			} else if sess.Active && sess.SeenPlaying {
				posSec = a.session.estimate(now, false)
				source = "wall-hold"
			}
		} else if sess.Active && sess.SeenPlaying {
			posSec = a.session.estimate(now, false)
			source = "wall-hold"
		}

		// The UI ships a hardcoded episode list, so a tap can arrive before the
		// background RSS refresh has finished. mq_player also reports bogus
		// 2-6 second durations for network streams. Retry the feed lookup while
		// the real length is unknown so the progress bar gets a usable total.
		if sess.Active && sess.MediaType == "podcast" && durSec < minimumPlausibleDuration && now.Sub(lastDurationLookup) >= 3*time.Second {
			lastDurationLookup = now
			if d := a.findPodcastDuration(sess.StreamURL, sess.Title); d != "" {
				if sec := parseTimeSeconds(d); sec >= minimumPlausibleDuration {
					durSec = sec
					a.session.setDuration(sec)
					a.log.Infof("[Duration] 从订阅源补全时长: %s -> %s", sess.Title, formatDisplayTime(sec))
				}
			}
		}

		if durSec > 0 && posSec > durSec {
			posSec = durSec
		}
		if posSec < 0 {
			posSec = 0
		}

		if now.Sub(lastProgressLog) >= 5*time.Second {
			lastProgressLog = now
			a.log.Debugf("[Progress] state=%s src=%s pos=%d dur=%d rel=%d", state, source, posSec, durSec, relPos)
		}

		a.mu.Lock()
		a.status.RendererState = state
		a.status.UpdatedAt = now.Format("2006-01-02 15:04:05")
		a.status.Position = posSec
		a.status.Duration = durSec
		a.status.PositionStr = formatDisplayTime(posSec)
		a.status.DurationStr = formatDisplayTime(durSec)

		ended := false
		switch state {
		case "PLAYING", "TRANSITIONING":
			a.status.Playing = true
			lastPlayingAt = now
		case "PAUSED_PLAYBACK", "PAUSED_RECORDING":
			a.status.Playing = false
		case "STOPPED", "NO_MEDIA_PRESENT":
			if a.status.Type == "podcast" && sess.Active {
				if durationReachedEnd(posSec, durSec) || sess.EOF {
					ended = true
					a.status.Playing = false
					a.status.Error = ""
				} else if sess.SeenPlaying && now.Sub(lastPlayingAt) < 10*time.Second {
					// mq_player is rebinding its UPnP port. Keep the UI
					// session alive while the recovery loop reconnects.
					a.status.Playing = true
					a.status.RendererState = "RECONNECTING"
					a.status.Error = ""
				} else {
					a.status.Playing = false
					a.status.Error = ""
				}
			} else {
				a.status.Playing = false
				if a.status.Type == "radio" {
					a.status.Error = "播放器已停止（" + state + "）"
				} else {
					a.status.Error = ""
				}
			}
		}
		isPlaying := a.status.Playing
		mediaType := a.status.Type
		a.mu.Unlock()

		if state == "PAUSED_PLAYBACK" || state == "PAUSED_RECORDING" {
			a.session.setPaused(true)
		} else {
			a.session.setPaused(false)
		}
		if state == "PLAYING" {
			a.session.markPlaying(now)
		}
		if sess.Active {
			a.session.update(posSec, durSec, now)
		}

		lastBroadcastPlaying = isPlaying
		a.broadcastPlaybackState(mediaType, posSec, durSec, isPlaying)

		// 播客断点记录与完成清理
		if ended {
			a.clearBreakpoint(sess.StreamURL, sess.Title, sess.Subtitle)
			a.session.stop()
			a.pcm.reset(0)
			if now.Sub(lastBreakpointSave) > 5*time.Second {
				lastBreakpointSave = now
				go a.saveBreakpoints()
			}
		} else if mediaType == "podcast" && sess.Active {
			if durationReachedEnd(posSec, durSec) {
				a.clearBreakpoint(sess.StreamURL, sess.Title, sess.Subtitle)
				a.session.stop()
				if now.Sub(lastBreakpointSave) > 5*time.Second {
					lastBreakpointSave = now
					go a.saveBreakpoints()
				}
			} else if posSec >= 5 {
				a.setBreakpoint(sess.StreamURL, sess.Title, posSec, durSec, sess.Subtitle)
				if now.Sub(lastBreakpointSave) > 10*time.Second {
					lastBreakpointSave = now
					go a.saveBreakpoints()
				}
			}
		}

		if (state == "STOPPED" || state == "NO_MEDIA_PRESENT") && !ended {
			if mediaType == "radio" {
				if now.Sub(lastRecovery) > 45*time.Second && recoveries < 3 {
					lastRecovery = now
					recoveries++
					a.log.Warnf("播放器在播放过程中停止，尝试恢复 (%d/3)", recoveries)
					go a.recoverPlayback(r)
				}
			} else if mediaType == "podcast" && sess.Active {
				if now.Sub(lastRecovery) > 15*time.Second && recoveries < 3 {
					lastRecovery = now
					recoveries++
					a.log.Warnf("播客播放器意外停止，尝试从断点恢复 (%d/3)", recoveries)
					go a.recoverPlayback(r)
				}
			}
		} else if state == "PLAYING" {
			recoveries = 0
		}
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func (a *App) recoverPlayback(r *RendererInfo) {
	if r == nil {
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 12*time.Second)
	defer cancel()
	st := a.getStatus()
	if st.ProxyURL == "" {
		return
	}
	vals, err := a.getMediaInfo(ctx, r)
	if err == nil {
		cur := vals["CurrentURI"]
		a.log.Infof("恢复播放：renderer 当前 URI=%s，目标=%s", cur, st.ProxyURL)
		if cur == "" || cur != st.ProxyURL {
			meta := didlMetadata(st.Title, st.SubTitle, albumForType(st.Type), st.Cover, st.ProxyURL, st.ContentType)
			if err := a.setAVTransportURI(ctx, r, st.ProxyURL, meta); err != nil {
				a.log.Errorf("恢复播放 SetAVTransportURI 失败: %v", err)
				return
			}
		}
	} else {
		a.log.Warnf("恢复播放时 GetMediaInfo 失败: %v", err)
	}
	if err := a.rendererPlay(ctx, r); err != nil {
		a.log.Errorf("恢复播放 Play 失败: %v", err)
		return
	}
	a.mu.Lock()
	a.status.Playing = true
	a.status.Error = ""
	a.mu.Unlock()
	a.session.clearEOF()

	// A podcast that was interrupted mid-episode must resume at the saved
	// position, not at 00:00. Re-issue the seek after the renderer is up.
	sess := a.session.snapshot()
	if sess.Active && sess.MediaType == "podcast" && sess.LastPos >= 5 {
		target := formatRendererSeekTarget(sess.LastPos)
		time.Sleep(500 * time.Millisecond)
		for attempt := 0; attempt < 2; attempt++ {
			seekCtx, seekCancel := context.WithTimeout(a.ctx, 3*time.Second)
			err := a.rendererSeek(seekCtx, r, target)
			seekCancel()
			if err == nil {
				a.pcm.setSeekTarget(sess.LastPos)
				a.log.Infof("恢复播放：已跳转到断点 %s (%d秒, UPnP REL_TIME: %s)", formatDisplayTime(sess.LastPos), sess.LastPos, target)
				break
			}
			a.log.Warnf("恢复播放 Seek 失败 (%d/2): %v", attempt+1, err)
			time.Sleep(400 * time.Millisecond)
		}
	}
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
