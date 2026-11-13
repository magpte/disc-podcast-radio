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

func TestDIDLMetadataTitleAndArtistMapping(t *testing.T) {
	// Episode title must be placed in <upnp:artist> (FiiO 28pt top main label 0x8de26c)
	// Subscription/feed title must be placed in <dc:title> (FiiO 22pt bottom subtitle label 0x8de264)
	meta := didlMetadata("第100期: 聊聊AI", "科技早知道", "DISC 播客", "", "http://127.0.0.1:8080/stream", "audio/mpeg")
	if !bytes.Contains([]byte(meta), []byte("<dc:title>科技早知道</dc:title>")) {
		t.Errorf("expected <dc:title> to contain feed subtitle '科技早知道', got: %s", meta)
	}
	if !bytes.Contains([]byte(meta), []byte("<upnp:artist>第100期: 聊聊AI</upnp:artist>")) {
		t.Errorf("expected <upnp:artist> to contain episode title '第100期: 聊聊AI', got: %s", meta)
	}

	// Radio with category
	metaRadio := didlMetadata("中国之声", "国家台", "DISC 网络电台", "", "http://127.0.0.1:8080/stream", "audio/mpeg")
	if !bytes.Contains([]byte(metaRadio), []byte("<dc:title>国家台</dc:title>")) {
		t.Errorf("expected <dc:title> to contain category '国家台', got: %s", metaRadio)
	}
	if !bytes.Contains([]byte(metaRadio), []byte("<upnp:artist>中国之声</upnp:artist>")) {
		t.Errorf("expected <upnp:artist> to contain station name '中国之声', got: %s", metaRadio)
	}

	// Empty subtitle fallback
	metaFallback := didlMetadata("中国之声", "", "DISC 网络电台", "", "http://127.0.0.1:8080/stream", "audio/mpeg")
	if !bytes.Contains([]byte(metaFallback), []byte("<dc:title>DISC 网络电台</dc:title>")) {
		t.Errorf("expected <dc:title> fallback to album 'DISC 网络电台', got: %s", metaFallback)
	}
	if !bytes.Contains([]byte(metaFallback), []byte("<upnp:artist>中国之声</upnp:artist>")) {
		t.Errorf("expected <upnp:artist> to contain station name '中国之声', got: %s", metaFallback)
	}
}


