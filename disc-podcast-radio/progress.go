package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The renderer (mq_player) does not reliably fill RelTime in GetPositionInfo
// for network streams: the online-music module already works around the same
// behaviour by counting ALSA frames. We do the same here, with a wall-clock
// fallback for devices where /proc/asound is unavailable. Without this the
// podcast progress bar stays at 00:00 and no breakpoint is ever recorded,
// because the old code only accepted RelTime > 0.

type pcmObservation struct {
	Frames  uint64
	Rate    int
	State   string
	Trigger string
	Device  string
	Valid   bool
}

func parsePCMObservation(status, params string) pcmObservation {
	p := pcmObservation{}
	fields := func(line string) []string {
		// X2000 ALSA prints "hw_ptr      : 7132832".
		return strings.Fields(strings.ReplaceAll(line, ":", " "))
	}
	found := false
	for _, line := range strings.Split(status, "\n") {
		f := fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "hw_ptr":
			var err error
			p.Frames, err = strconv.ParseUint(f[1], 10, 64)
			found = err == nil
		case "state":
			p.State = f[1]
		case "trigger_time":
			p.Trigger = f[1]
		}
	}
	for _, line := range strings.Split(params, "\n") {
		f := fields(line)
		if len(f) > 1 && f[0] == "rate" {
			p.Rate, _ = strconv.Atoi(f[1])
		}
	}
	p.Valid = found && p.Rate >= 8000 && p.Rate <= 768000
	return p
}

func readPCMAt(dir string) pcmObservation {
	status, err := os.ReadFile(filepath.Join(dir, "status"))
	if err != nil {
		return pcmObservation{}
	}
	params, _ := os.ReadFile(filepath.Join(dir, "hw_params"))
	obs := parsePCMObservation(string(status), string(params))
	obs.Device = dir
	return obs
}

// readPCMObservation prefers the music playback substream and then falls back
// to any running PCM. Reading three small proc files once a second is cheap
// even on the X2000.
func readPCMObservation(preferred string) pcmObservation {
	candidates := []string{}
	if preferred != "" {
		candidates = append(candidates, preferred)
	}
	candidates = append(candidates,
		"/proc/asound/card0/pcm3p/sub0",
		"/proc/asound/card0/pcm0p/sub0",
	)
	seen := map[string]bool{}
	var best pcmObservation
	bestScore := -1
	consider := func(dir string) {
		if dir == "" || seen[dir] {
			return
		}
		seen[dir] = true
		obs := readPCMAt(dir)
		if !obs.Valid {
			return
		}
		score := 0
		if strings.Contains(dir, "pcm3p") {
			score += 30
		}
		if obs.State == "RUNNING" {
			score += 100
		}
		if obs.Frames > 0 {
			score += 10
		}
		if score > bestScore {
			best, bestScore = obs, score
		}
	}
	for _, dir := range candidates {
		consider(dir)
	}
	if matches, err := filepath.Glob("/proc/asound/card*/pcm*p/sub*/status"); err == nil {
		for _, statusPath := range matches {
			consider(filepath.Dir(statusPath))
		}
	}
	return best
}

type pcmClock struct {
	mu sync.Mutex

	device     string
	base       float64
	seconds    float64
	frames     uint64
	rate       int
	trigger    string
	needRebase bool
	rebaseTo   float64
	seekSet    bool
	seekTo     int
	holdUntil  time.Time
	valid      bool
}

// reset anchors the clock to baseSec. The next observation rebases against the
// current hw_ptr so a PCM stream that keeps running across tracks does not make
// the position jump to the device's cumulative frame count.
func (c *pcmClock) reset(baseSec int) {
	if baseSec < 0 {
		baseSec = 0
	}
	c.mu.Lock()
	c.base = float64(baseSec)
	c.seconds = float64(baseSec)
	c.frames = 0
	c.trigger = ""
	c.needRebase = true
	c.rebaseTo = float64(baseSec)
	c.seekSet = false
	c.valid = false
	c.mu.Unlock()
}

// setSeekTarget is called after a renderer Seek succeeds. The next PCM sample
// is interpreted as the audio at targetSec.
func (c *pcmClock) setSeekTarget(targetSec int) {
	if targetSec < 0 {
		targetSec = 0
	}
	c.mu.Lock()
	now := time.Now()
	c.base = float64(targetSec)
	c.seconds = float64(targetSec)
	c.frames = 0
	c.trigger = ""
	c.needRebase = false
	c.rebaseTo = float64(targetSec)
	c.seekSet = true
	c.seekTo = targetSec
	c.holdUntil = now.Add(1200 * time.Millisecond)
	c.valid = true
	c.mu.Unlock()
}

func (c *pcmClock) position(state string) (int, bool) {
	c.mu.Lock()
	device := c.device
	c.mu.Unlock()

	obs := readPCMObservation(device)
	if !obs.Valid {
		return 0, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if obs.Device != "" {
		c.device = obs.Device
	}
	if obs.Rate != c.rate {
		c.rate = obs.Rate
		c.needRebase = true
		c.rebaseTo = c.seconds
	}

	now := time.Now()
	// During the settling window after a seek, hold the target position
	// so ALSA frames from the previous stream are never subtracted from c.base
	if c.seekSet && now.Before(c.holdUntil) {
		return c.seekTo, true
	}

	if c.seekSet {
		// Seek settling window elapsed: rebase ALSA frame counter to seekTo
		c.base = float64(c.seekTo) - float64(obs.Frames)/float64(obs.Rate)
		c.frames = obs.Frames
		c.trigger = obs.Trigger
		c.seconds = float64(c.seekTo)
		c.seekSet = false
		c.needRebase = false
		c.valid = true
		if c.seconds < 0 {
			c.seconds = 0
		}
		return int(c.seconds), true
	}

	if c.needRebase {
		c.base = c.rebaseTo - float64(obs.Frames)/float64(obs.Rate)
		c.frames = obs.Frames
		c.seconds = c.rebaseTo
		c.trigger = obs.Trigger
		c.needRebase = false
		c.seekSet = false
		c.valid = true
		if c.seconds < 0 {
			c.seconds = 0
		}
		return int(c.seconds), true
	}
	if obs.Trigger != "" && obs.Trigger != c.trigger {
		// A new ALSA trigger means the decoder restarted. Keep the last known
		// media position instead of jumping back to zero.
		if c.seekSet {
			c.base = float64(c.seekTo)
			c.seekSet = false
		} else {
			c.base = c.seconds - float64(obs.Frames)/float64(obs.Rate)
		}
		c.trigger = obs.Trigger
		c.frames = obs.Frames
		c.seconds = c.base + float64(obs.Frames)/float64(obs.Rate)
	} else if obs.Frames < c.frames {
		// hw_ptr wrapped or the stream was rewound.
		if c.seekSet {
			c.base = float64(c.seekTo)
			c.seekSet = false
		} else {
			c.base = c.seconds - float64(obs.Frames)/float64(obs.Rate)
		}
		c.frames = obs.Frames
		c.seconds = c.base + float64(obs.Frames)/float64(obs.Rate)
	} else if state == "PLAYING" {
		delta := float64(obs.Frames-c.frames) / float64(obs.Rate)
		if delta < 0 {
			delta = 0
		}
		if delta > 15 && c.seekSet {
			// Large forward jump right after a seek.
			c.base = float64(c.seekTo)
			c.seekSet = false
			c.seconds = c.base + float64(obs.Frames)/float64(obs.Rate)
		} else {
			c.seconds += delta
		}
		c.frames = obs.Frames
	} else {
		// Paused: remember the frame counter without advancing the clock.
		c.frames = obs.Frames
	}
	if c.seconds < 0 {
		c.seconds = 0
	}
	c.valid = true
	return int(c.seconds), true
}

type sessionSnapshot struct {
	Active      bool
	MediaType   string
	StreamURL   string
	Title       string
	Subtitle    string
	Base        int
	Duration    int
	LastPos     int
	SeenPlaying bool
	Paused      bool
	EOF         bool
	LastUpdate  time.Time
}

type playbackSession struct {
	mu sync.Mutex

	active      bool
	mediaType   string
	streamURL   string
	title       string
	subtitle    string
	base        int
	duration    int
	lastPos     int
	seenPlaying bool
	paused      bool
	eof         bool
	lastUpdate  time.Time
}

func (s *playbackSession) start(mediaType, streamURL, title, subtitle string, base, duration int) {
	if base < 0 {
		base = 0
	}
	s.mu.Lock()
	s.active = true
	s.mediaType = mediaType
	s.streamURL = streamURL
	s.title = title
	s.subtitle = subtitle
	s.base = base
	s.duration = duration
	s.lastPos = base
	s.seenPlaying = false
	s.paused = false
	s.eof = false
	s.lastUpdate = time.Now()
	s.mu.Unlock()
}

func (s *playbackSession) markEOF() {
	s.mu.Lock()
	if s.active {
		s.eof = true
	}
	s.mu.Unlock()
}

func (s *playbackSession) clearEOF() {
	s.mu.Lock()
	s.eof = false
	s.mu.Unlock()
}

func (s *playbackSession) markPlaying(now time.Time) {
	s.mu.Lock()
	if s.active && !s.seenPlaying {
		s.seenPlaying = true
		s.paused = false
		s.lastUpdate = now
		s.lastPos = s.base
	}
	s.mu.Unlock()
}

func (s *playbackSession) setPaused(paused bool) {
	s.mu.Lock()
	if s.active {
		if paused && !s.paused {
			now := time.Now()
			if s.seenPlaying && !s.lastUpdate.IsZero() {
				if delta := int(now.Sub(s.lastUpdate).Seconds()); delta > 0 {
					s.lastPos += delta
				}
			}
			if s.duration > 0 && s.lastPos > s.duration {
				s.lastPos = s.duration
			}
			s.lastUpdate = now
		}
		s.paused = paused
	}
	s.mu.Unlock()
}

func (s *playbackSession) update(pos, duration int, now time.Time) {
	s.mu.Lock()
	if s.active {
		if pos >= 0 {
			s.lastPos = pos
		}
		if duration > 0 {
			s.duration = sanitizeDuration(s.duration, duration)
		}
		s.lastUpdate = now
	}
	s.mu.Unlock()
}

func (s *playbackSession) setDuration(duration int) {
	s.mu.Lock()
	if s.active && duration > 0 {
		s.duration = sanitizeDuration(s.duration, duration)
	}
	s.mu.Unlock()
}

func (s *playbackSession) estimate(now time.Time, playing bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return 0
	}
	pos := s.lastPos
	if s.seenPlaying && playing && !s.paused && !s.lastUpdate.IsZero() {
		pos += int(now.Sub(s.lastUpdate).Seconds())
	}
	if pos < 0 {
		pos = 0
	}
	if s.duration > 0 && pos > s.duration {
		pos = s.duration
	}
	return pos
}

func (s *playbackSession) snapshot() sessionSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sessionSnapshot{
		Active:      s.active,
		MediaType:   s.mediaType,
		StreamURL:   s.streamURL,
		Title:       s.title,
		Subtitle:    s.subtitle,
		Base:        s.base,
		Duration:    s.duration,
		LastPos:     s.lastPos,
		SeenPlaying: s.seenPlaying,
		Paused:      s.paused,
		EOF:         s.eof,
		LastUpdate:  s.lastUpdate,
	}
}

func (s *playbackSession) stop() sessionSnapshot {
	s.mu.Lock()
	snap := sessionSnapshot{
		Active:      s.active,
		MediaType:   s.mediaType,
		StreamURL:   s.streamURL,
		Title:       s.title,
		Subtitle:    s.subtitle,
		Base:        s.base,
		Duration:    s.duration,
		LastPos:     s.lastPos,
		SeenPlaying: s.seenPlaying,
		Paused:      s.paused,
		EOF:         s.eof,
		LastUpdate:  s.lastUpdate,
	}
	s.active = false
	s.mu.Unlock()
	return snap
}

// canonicalMediaKey removes CDN signature/expiry parameters while keeping
// identity parameters such as track_id. Podcast CDNs hand out a fresh signed
// URL for the same episode, so an exact-URL breakpoint would never match again.
func canonicalMediaKey(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return raw
	}
	u.Fragment = ""
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	q := u.Query()
	volatile := map[string]bool{
		"jt": true, "token": true, "sign": true, "signature": true,
		"expires": true, "expire": true, "timestamp": true, "ts": true,
		"auth": true, "key": true, "policy": true, "from": true, "spm": true,
	}
	for k := range q {
		lk := strings.ToLower(k)
		if volatile[lk] || strings.HasPrefix(lk, "utm_") {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func episodeBreakpointKey(title, subtitle string) string {
	t := strings.ToLower(strings.TrimSpace(title))
	if t == "" {
		return ""
	}
	return "ep|" + t + "|" + strings.ToLower(strings.TrimSpace(subtitle))
}
