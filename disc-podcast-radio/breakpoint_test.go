package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTimeSeconds(t *testing.T) {
	cases := []struct {
		input    string
		expected int
	}{
		{"", 0},
		{"0", 0},
		{"120", 120},
		{"01:30", 90},
		{"00:01:30", 90},
		{"01:00:00", 3600},
		{"02:15:30.500", 8130},
		{"45:20", 2720},
	}
	for _, c := range cases {
		got := parseTimeSeconds(c.input)
		if got != c.expected {
			t.Errorf("parseTimeSeconds(%q) = %d, expected %d", c.input, got, c.expected)
		}
	}
}

func TestFormatSeconds(t *testing.T) {
	if got := formatSeconds(0); got != "00:00:00" {
		t.Errorf("expected 00:00:00, got %s", got)
	}
	if got := formatSeconds(90); got != "00:01:30" {
		t.Errorf("expected 00:01:30, got %s", got)
	}
	if got := formatSeconds(3665); got != "01:01:05" {
		t.Errorf("expected 01:01:05, got %s", got)
	}
}

func TestFormatRendererSeekTarget(t *testing.T) {
	cases := []struct {
		sec      int
		expected string
		targetSec int
	}{
		{0, "00:00:00", 0},
		{1, "00:00:01", 1},
		{60, "00:01:00", 60},
		{90, "00:01:30", 90},
		{3843, "01:04:03", 3843},
	}
	for _, c := range cases {
		got := formatRendererSeekTarget(c.sec)
		if got != c.expected {
			t.Errorf("formatRendererSeekTarget(%d) = %q, expected %q", c.sec, got, c.expected)
		}
		// Verify mathematical equivalence under mq_player's sscanf("%d:%02d:%02d", &h, &m, &s)
		var h, m, s int
		n, _ := fmt.Sscanf(got, "%d:%d:%d", &h, &m, &s)
		if n != 3 {
			t.Fatalf("failed to scan %q", got)
		}
		computedSec := h*3600 + m*60 + s
		if computedSec != c.targetSec {
			t.Errorf("mq_player total for %q = %d sec, expected %d sec", got, computedSec, c.targetSec)
		}
	}
}

func TestFormatDisplayTime(t *testing.T) {
	if got := formatDisplayTime(0); got != "00:00" {
		t.Errorf("expected 00:00, got %s", got)
	}
	if got := formatDisplayTime(90); got != "01:30" {
		t.Errorf("expected 01:30, got %s", got)
	}
	if got := formatDisplayTime(3665); got != "01:01:05" {
		t.Errorf("expected 01:01:05, got %s", got)
	}
}

func TestBreakpointOperations(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "disc_bp_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := NewLogger(filepath.Join(tmpDir, "app.log"))
	defer logger.Close()
	app := NewApp(tmpDir, 9999, logger)

	streamURL := "http://example.com/podcast/ep1.mp3"
	title := "第1期 启程"

	// 初始状态应不存在
	if _, ok := app.getBreakpoint(streamURL); ok {
		t.Fatalf("breakpoint should not exist initially")
	}

	// 记录断点 (position=120, duration=2400)
	app.setBreakpoint(streamURL, title, 120, 2400)
	bp, ok := app.getBreakpoint(streamURL)
	if !ok {
		t.Fatalf("failed to get breakpoint")
	}
	if bp.Position != 120 || bp.Duration != 2400 || bp.Title != title {
		t.Errorf("unexpected breakpoint data: %+v", bp)
	}

	// 保存断点并重新加载验证持久化
	if err := app.saveBreakpoints(); err != nil {
		t.Fatalf("saveBreakpoints failed: %v", err)
	}

	app2 := NewApp(tmpDir, 9999, logger)
	app2.loadBreakpoints()
	bp2, ok2 := app2.getBreakpoint(streamURL)
	if !ok2 || bp2.Position != 120 {
		t.Errorf("failed to reload persisted breakpoint: %+v", bp2)
	}

	// 清除断点
	app.clearBreakpoint(streamURL)
	if _, ok := app.getBreakpoint(streamURL); ok {
		t.Errorf("breakpoint should have been cleared")
	}
}

func TestBroadcastPlaybackState(t *testing.T) {
	logger := NewLogger(os.DevNull)
	defer logger.Close()
	app := NewApp(os.TempDir(), 9999, logger)

	app.broadcastPlaybackState("podcast", 125, 3600, true)
	stateFile := "/tmp/disc-podcast-radio/playback_state"
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("failed to read playback_state: %v", err)
	}
	content := string(data)
	if content == "" {
		t.Fatalf("playback_state is empty")
	}
	if !strings.Contains(content, "podcast") || !strings.Contains(content, "125") {
		t.Errorf("unexpected content: %s", content)
	}
}
