package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParsePCMObservation(t *testing.T) {
	status := "state: RUNNING\nowner_pid   : 1053\ntrigger_time: 123456\ntstamp      : 0\ndelay       : 4410\navail       : 100\nhw_ptr      : 7132832\n"
	params := "access: RW_INTERLEAVED\nformat: S16_LE\nchannels: 2\nrate: 44100 (44100/1)\n"
	obs := parsePCMObservation(status, params)
	if !obs.Valid {
		t.Fatalf("expected valid observation: %+v", obs)
	}
	if obs.Frames != 7132832 || obs.Rate != 44100 || obs.State != "RUNNING" || obs.Trigger != "123456" {
		t.Fatalf("unexpected observation: %+v", obs)
	}
}

func TestCanonicalMediaKeyStripsSignature(t *testing.T) {
	a := "https://jt.ximalaya.com//GKwRIMAOs7DxAyCuoQTvIJVF.m4a?channel=rss&album_id=5574153&track_id=1027435622&jt=expiring-token"
	b := "https://jt.ximalaya.com//GKwRIMAOs7DxAyCuoQTvIJVF.m4a?channel=rss&album_id=5574153&track_id=1027435622&jt=fresh-token"
	ka, kb := canonicalMediaKey(a), canonicalMediaKey(b)
	if ka != kb {
		t.Fatalf("canonical keys differ:\n%s\n%s", ka, kb)
	}
	if ka == "" || ka == a {
		t.Fatalf("signature parameter was not stripped: %s", ka)
	}
}

func TestBreakpointSurvivesURLTokenChange(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "disc_bp_url_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := NewLogger(filepath.Join(tmpDir, "app.log"))
	defer logger.Close()
	app := NewApp(tmpDir, 9999, logger)

	title, subtitle := "第 42 期", "日谈公园"
	first := "https://cdn.example.com/ep42.m4a?track_id=42&jt=old-token"
	second := "https://cdn.example.com/ep42.m4a?track_id=42&jt=new-token"

	app.setBreakpoint(first, title, 123, 3600, subtitle)
	bp, ok := app.getBreakpoint(second, title, subtitle)
	if !ok {
		t.Fatalf("breakpoint did not survive a signed-URL change")
	}
	if bp.Position != 123 || bp.Title != title || bp.Subtitle != subtitle {
		t.Fatalf("unexpected breakpoint: %+v", bp)
	}

	// The same episode key must also match when the CDN host changes.
	third := "https://other-cdn.example.net/ep42.m4a?track_id=42"
	if _, ok := app.getBreakpoint(third, title, subtitle); !ok {
		t.Fatalf("breakpoint did not match by episode title")
	}
}

func TestPlaybackSessionWallClock(t *testing.T) {
	var s playbackSession
	s.start("podcast", "http://example.com/a.mp3", "ep", "show", 120, 3600)
	if got := s.estimate(time.Now(), true); got != 120 {
		t.Fatalf("before playback starts, position = %d, want 120", got)
	}

	now := time.Now()
	s.markPlaying(now)
	if got := s.estimate(now.Add(10*time.Second), true); got != 130 {
		t.Fatalf("after 10s of playback, position = %d, want 130", got)
	}
	s.update(130, 3600, now.Add(10*time.Second))
	s.setPaused(true)
	if got := s.estimate(now.Add(20*time.Second), true); got != 130 {
		t.Fatalf("while paused, position = %d, want 130", got)
	}
	s.setPaused(false)
	s.update(200, 3600, now.Add(20*time.Second))
	if got := s.estimate(now.Add(25*time.Second), true); got != 205 {
		t.Fatalf("after update, position = %d, want 205", got)
	}
}
