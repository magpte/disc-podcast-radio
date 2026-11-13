package main

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// Seek verification.
//
// mq_player acknowledges every UPnP Seek with HTTP 200 even when the request is
// dropped, and GetPositionInfo reports RelTime=00:00:00 for network streams, so
// neither the SOAP result nor RelTime can tell us where playback really is.
// The only trustworthy evidence is the byte offset mq_player asks for when it
// reopens the proxied stream: after a failed seek it restarts at the mdat
// header (~0.5 MB into the file) instead of the target offset.
//
// So: record the Range start of every audio-proxy connection that starts while
// a seek is being verified, and treat "long target but tiny byte offset" as a
// failed seek.

const (
	// seekObserveWindow bounds how long we wait for the renderer to reopen the
	// stream after a seek.
	seekObserveWindow = 3 * time.Second
	// seekSettleHold covers mq_player's 500ms seek debounce thread plus the
	// demuxer reopen. Probing UPnP inside this window contributed to the
	// 49494 -> 49495 -> 49496 port churn seen in the field logs.
	seekSettleHold = 2 * time.Second
	// seekSmallStart is "close enough to the file header" to be a restart.
	seekSmallStart = int64(1 << 20)
	// seekLongTarget is the shortest target for which the byte offset is a
	// reliable oracle (a 30s seek legitimately lives in the first megabyte).
	seekLongTarget = 60
)

type seekCheckState struct {
	mu       sync.Mutex
	pending  bool
	target   int
	at       time.Time
	maxStart int64
	seen     bool
}

func (s *seekCheckState) begin(target int) {
	s.mu.Lock()
	s.pending = true
	s.target = target
	s.at = time.Now()
	s.maxStart = 0
	s.seen = false
	s.mu.Unlock()
}

func (s *seekCheckState) cancel() {
	s.mu.Lock()
	s.pending = false
	s.mu.Unlock()
}

func (s *seekCheckState) finish() {
	s.mu.Lock()
	s.pending = false
	s.mu.Unlock()
}

// note records the Range start of a new proxy connection.
func (s *seekCheckState) note(rangeHeader string) {
	start, ok := parseRangeStart(rangeHeader)
	if !ok {
		return
	}
	s.mu.Lock()
	if s.pending && time.Since(s.at) <= seekObserveWindow {
		s.seen = true
		if start > s.maxStart {
			s.maxStart = start
		}
	}
	s.mu.Unlock()
}

// holding reports whether the renderer should be left alone for a moment.
func (s *seekCheckState) holding(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending && now.Sub(s.at) < seekSettleHold
}

// verdict waits for the seek outcome. ok=false means the renderer restarted at
// the head of the file instead of the requested position.
func (s *seekCheckState) verdict() (ok bool, target int, start int64) {
	deadline := time.Now().Add(seekObserveWindow)
	for {
		s.mu.Lock()
		pending := s.pending
		seen := s.seen
		maxStart := s.maxStart
		target = s.target
		elapsed := time.Since(s.at)
		s.mu.Unlock()

		if !pending {
			return true, target, -1
		}

		if seen {
			if target > seekLongTarget && maxStart >= seekSmallStart {
				// The renderer jumped to the expected part of the file.
				s.finish()
				return true, target, maxStart
			}
			if target > seekLongTarget && elapsed >= seekSettleHold && maxStart < seekSmallStart {
				s.finish()
				return false, target, maxStart
			}
		} else if elapsed >= seekSettleHold {
			// No reopen at all: the seek was handled inside the open stream.
			s.finish()
			return true, target, -1
		}

		if time.Now().After(deadline) {
			s.finish()
			if seen && target > seekLongTarget && maxStart < seekSmallStart {
				return false, target, maxStart
			}
			return true, target, maxStart
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// confirmSeek runs after Seek() returned so the HTTP handler is not blocked.
func (a *App) confirmSeek() {
	ok, target, start := a.seekCheck.verdict()
	if ok {
		if start > 0 {
			a.log.Debugf("[Seek] 校验通过: 目标=%s 播放器落点=%d 字节", formatDisplayTime(target), start)
		}
		return
	}

	a.log.Warnf("[Seek] 校验失败: 目标=%s，播放器实际从文件开头(%d 字节)重播，进度显示已回退",
		formatDisplayTime(target), start)

	a.pcm.reset(0)
	a.mu.Lock()
	dur := a.status.Duration
	playing := a.status.Playing
	pType := a.status.Type
	a.status.Position = 0
	a.status.PositionStr = "00:00"
	a.mu.Unlock()
	a.session.update(0, dur, time.Now())
	a.broadcastPlaybackState(pType, 0, dur, playing)
}

func parseRangeStart(h string) (int64, bool) {
	h = strings.TrimSpace(h)
	if !strings.HasPrefix(h, "bytes=") {
		return 0, false
	}

	v := strings.TrimPrefix(h, "bytes=")
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v = v[:i]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
