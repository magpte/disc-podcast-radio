package main

import (
	"testing"
	"time"
)

func TestSanitizeDurationRejectsProbeValues(t *testing.T) {
	cases := []struct {
		name      string
		known     int
		candidate int
		want      int
	}{
		{"zero candidate keeps known", 3600, 0, 3600},
		{"tiny candidate keeps known", 3600, 6, 3600},
		{"tiny candidate without known stays unknown", 0, 2, 0},
		{"plausible candidate replaces unknown", 0, 1800, 1800},
		{"modest shrink is accepted", 3600, 3000, 3000},
		{"large shrink is rejected", 3600, 600, 3600},
		{"growing duration is accepted", 1800, 1900, 1900},
		{"short trusted duration is kept", 20, 6, 20},
	}
	for _, tc := range cases {
		if got := sanitizeDuration(tc.known, tc.candidate); got != tc.want {
			t.Errorf("%s: sanitizeDuration(%d, %d) = %d, want %d", tc.name, tc.known, tc.candidate, got, tc.want)
		}
	}
}

func TestDurationReachedEndIgnoresImplausibleDuration(t *testing.T) {
	if durationReachedEnd(2, 6) {
		t.Fatalf("a 6 second probe duration must not mean the episode ended")
	}
	if durationReachedEnd(10, 0) {
		t.Fatalf("an unknown duration must not mean the episode ended")
	}
	if !durationReachedEnd(3590, 3600) {
		t.Fatalf("position within 15s of a plausible duration should end the episode")
	}
	if durationReachedEnd(100, 3600) {
		t.Fatalf("position far from the end should not end the episode")
	}
}

func TestPlaybackSessionUpdateRejectsProbeDuration(t *testing.T) {
	var s playbackSession
	s.start("podcast", "http://example.com/a.mp3", "ep", "show", 0, 3600)
	s.update(2, 6, time.Now())
	snap := s.snapshot()
	if snap.Duration != 3600 {
		t.Fatalf("session duration shrank to %d, want 3600", snap.Duration)
	}
}
