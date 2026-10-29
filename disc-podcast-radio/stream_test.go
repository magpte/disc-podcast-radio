package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCopyAndFlushBurstAndPacing(t *testing.T) {
	// defaultBurstBytes + 24 KB test data
	data := make([]byte, defaultBurstBytes+24*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	rec := httptest.NewRecorder()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	err := copyAndFlush(ctx, rec, bytes.NewReader(data), false)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("copyAndFlush failed: %v", err)
	}
	if rec.Body.Len() != len(data) {
		t.Fatalf("wrote %d bytes, want %d", rec.Body.Len(), len(data))
	}
	// Initial 2 MB is burst, remaining 24 KB is paced at 96 KB/s (~250ms)
	// Total elapsed time should be at least 200ms and well under 1.5s
	if elapsed < 200*time.Millisecond {
		t.Errorf("pacing did not delay delivery: took %v", elapsed)
	}
}

func TestCopyAndFlushLiveNoPacing(t *testing.T) {
	// 500 KB test data for live stream (must finish immediately without sleep)
	data := make([]byte, 500*1024)
	rec := httptest.NewRecorder()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	start := time.Now()
	err := copyAndFlush(ctx, rec, bytes.NewReader(data), true)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("copyAndFlush live failed: %v", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("live stream took too long: %v (expected < 100ms)", elapsed)
	}
}

func TestAppActiveStreamsAtomic(t *testing.T) {
	app := &App{}
	if atomic.LoadInt32(&app.activeStreams) != 0 {
		t.Fatalf("expected 0, got %d", app.activeStreams)
	}
	atomic.AddInt32(&app.activeStreams, 1)
	if atomic.LoadInt32(&app.activeStreams) != 1 {
		t.Fatalf("expected 1, got %d", app.activeStreams)
	}
	atomic.AddInt32(&app.activeStreams, -1)
	if atomic.LoadInt32(&app.activeStreams) != 0 {
		t.Fatalf("expected 0, got %d", app.activeStreams)
	}
}

