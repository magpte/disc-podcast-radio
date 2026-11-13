package main

import (
	"testing"
	"time"
)

func TestParseRangeStart(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"bytes=0-", 0, true},
		{"bytes=556047-", 556047, true},
		{"bytes=25000000-26000000", 25000000, true},
		{" bytes=1024- ", 1024, true},
		{"", 0, false},
		{"bytes=", 0, false},
		{"bytes=abc-", 0, false},
		{"items=0-", 0, false},
	}
	for _, c := range cases {
		got, ok := parseRangeStart(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Fatalf("parseRangeStart(%q) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// age moves the seek start time into the past so verdict() does not sleep.
func age(s *seekCheckState, d time.Duration) {
	s.mu.Lock()
	s.at = s.at.Add(-d)
	s.mu.Unlock()
}

func TestSeekCheckDetectsRestartFromHead(t *testing.T) {
	var s seekCheckState
	s.begin(4563)
	s.note("bytes=556047-")
	age(&s, 3*time.Second)

	ok, target, start := s.verdict()
	if ok {
		t.Fatalf("expected failure for a restart at the mdat header")
	}
	if target != 4563 || start != 556047 {
		t.Fatalf("got target=%d start=%d", target, start)
	}
}

func TestSeekCheckAcceptsRealJump(t *testing.T) {
	var s seekCheckState
	s.begin(4563)
	s.note("bytes=25000000-")
	age(&s, 3*time.Second)

	ok, target, start := s.verdict()
	if !ok {
		t.Fatalf("expected success, got failure (start=%d)", start)
	}
	if target != 4563 || start != 25000000 {
		t.Fatalf("got target=%d start=%d", target, start)
	}
}

func TestSeekCheckIgnoresSmallTarget(t *testing.T) {
	var s seekCheckState
	s.begin(30)
	s.note("bytes=200000-")
	age(&s, 3*time.Second)

	if ok, _, _ := s.verdict(); !ok {
		t.Fatalf("a 30s target can legitimately live in the first megabyte")
	}
}

func TestSeekCheckNoReopenIsAccepted(t *testing.T) {
	var s seekCheckState
	s.begin(4563)
	age(&s, 3*time.Second)

	if ok, _, _ := s.verdict(); !ok {
		t.Fatalf("no reopen means the seek was handled in place")
	}
}

func TestSeekCheckIgnoresConnectionsOutsideWindow(t *testing.T) {
	var s seekCheckState
	s.begin(4563)
	age(&s, 10*time.Second)
	s.note("bytes=556047-")

	ok, _, start := s.verdict()
	if !ok {
		t.Fatalf("a stale connection must not fail the seek (start=%d)", start)
	}
}

func TestSeekCheckHolding(t *testing.T) {
	var s seekCheckState
	s.begin(4563)
	if !s.holding(time.Now()) {
		t.Fatalf("expected hold right after begin")
	}
	if s.holding(time.Now().Add(seekSettleHold)) {
		t.Fatalf("hold must expire")
	}
	s.cancel()
	if s.holding(time.Now()) {
		t.Fatalf("cancelled seek must not hold polling")
	}
}
