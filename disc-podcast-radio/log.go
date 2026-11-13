package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// LogEntry is one structured log line kept in the in-memory ring buffer.
type LogEntry struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// Logger writes to stderr (captured by the launcher) and to a rotating file.
// The in-memory ring is exposed through /api/logs so failures can be diagnosed
// without SSH access.
//
// The TF card copy is never written from the caller's goroutine: /usr/data and
// the exFAT card can stall for hundreds of milliseconds, and every log call
// would otherwise serialise behind the same mutex (HTTP handlers included).
// Lines are queued on a bounded channel and flushed by tfLoop in batches.
type Logger struct {
	mu        sync.Mutex
	ring      []LogEntry
	ringStart int
	max       int
	file      *os.File
	path      string
	fileSize  int64

	tfCh      chan string
	tfStop    chan struct{}
	tfDone    chan struct{}
	tfDropped int32

	// debug is off by default. The field logs showed that per-request and
	// per-probe debug lines were the bulk of the flash writes (four sinks per
	// line: ring, service.log, stderr and the TF card batch) without helping
	// diagnosis. Set DISC_LOG_DEBUG=1 in the launcher to re-enable them.
	debug bool
}

const (
	logMaxEntries = 2000
	logMaxBytes   = 2 * 1024 * 1024

	tfCardDir   = "/tmp/sdcard"
	tfLogPath   = tfCardDir + "/disc-podcast-radio.log"
	tfQueueLen  = 2048
	tfBatchMax  = 32 * 1024
	tfFlushTick = 300 * time.Millisecond
)

func debugLoggingEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DISC_LOG_DEBUG"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func NewLogger(dataDir string) *Logger {
	l := &Logger{
		max:    logMaxEntries,
		path:   filepath.Join(dataDir, "service.log"),
		tfCh:   make(chan string, tfQueueLen),
		tfStop: make(chan struct{}),
		tfDone: make(chan struct{}),
		debug:  debugLoggingEnabled(),
	}
	if err := os.MkdirAll(dataDir, 0755); err == nil {
		if f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			l.file = f
			if st, err := f.Stat(); err == nil {
				l.fileSize = st.Size()
			}
		}
	}
	go l.tfLoop()
	return l
}

// tfLoop owns the TF card file descriptor. Nothing else may touch it, so the
// slow exFAT writes never hold l.mu and never block a request handler.
func (l *Logger) tfLoop() {
	defer close(l.tfDone)

	var (
		f         *os.File
		size      int64
		lastCheck time.Time
	)
	buf := make([]byte, 0, tfBatchMax+8192)

	openIfNeeded := func() {
		if f != nil {
			return
		}
		// /tmp/sdcard is mounted (or remounted) asynchronously at boot.
		if time.Since(lastCheck) < 3*time.Second {
			return
		}
		lastCheck = time.Now()
		st, err := os.Stat(tfCardDir)
		if err != nil || !st.IsDir() {
			return
		}
		nf, err := os.OpenFile(tfLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err != nil {
			return
		}
		f = nf
		size = 0
		if s, err := nf.Stat(); err == nil {
			size = s.Size()
		}
	}

	var rotate func()
	rotate = func() {
		if f == nil {
			return
		}
		_ = f.Close()
		f = nil
		backup := tfLogPath + ".1"
		_ = os.Remove(backup)
		_ = os.Rename(tfLogPath, backup)
		lastCheck = time.Time{}
		openIfNeeded()
		size = 0
	}

	flush := func() {
		if len(buf) == 0 {
			return
		}
		openIfNeeded()
		if f == nil {
			buf = buf[:0]
			return
		}
		n, err := f.Write(buf)
		if err != nil {
			_ = f.Close()
			f = nil
			buf = buf[:0]
			return
		}
		size += int64(n)
		buf = buf[:0]
		if size > logMaxBytes {
			rotate()
		}
	}

	ticker := time.NewTicker(tfFlushTick)
	defer ticker.Stop()

	for {
		select {
		case line := <-l.tfCh:
			if len(buf)+len(line) > tfBatchMax {
				flush()
			}
			buf = append(buf, line...)
		case <-ticker.C:
			flush()
		case <-l.tfStop:
			// Drain what is still queued, then leave.
			for {
				select {
				case line := <-l.tfCh:
					if len(buf)+len(line) > tfBatchMax {
						flush()
					}
					buf = append(buf, line...)
					continue
				default:
				}
				break
			}
			flush()
			if f != nil {
				_ = f.Close()
				f = nil
			}
			return
		}
	}
}

func (l *Logger) write(level, msg string) {
	entry := LogEntry{
		Time:    time.Now().Format("2006-01-02 15:04:05.000"),
		Level:   level,
		Message: msg,
	}
	line := fmt.Sprintf("%s [%s] %s\n", entry.Time, level, msg)

	l.mu.Lock()
	l.appendRingLocked(entry)

	_, _ = os.Stderr.WriteString(line)
	if l.file != nil {
		n, err := l.file.WriteString(line)
		if err == nil {
			l.fileSize += int64(n)
		}
		if l.fileSize > logMaxBytes {
			l.rotateLocked()
		}
	}
	l.mu.Unlock()

	// The TF copy is best effort: drop the line instead of blocking the caller.
	select {
	case l.tfCh <- line:
	default:
		atomic.AddInt32(&l.tfDropped, 1)
	}
}

func (l *Logger) appendRingLocked(entry LogEntry) {
	if l.max <= 0 {
		return
	}
	if len(l.ring) < l.max {
		l.ring = append(l.ring, entry)
		return
	}
	l.ring[l.ringStart] = entry
	l.ringStart++
	if l.ringStart >= l.max {
		l.ringStart = 0
	}
}

func (l *Logger) rotateLocked() {
	if l.file == nil {
		return
	}
	_ = l.file.Close()
	backup := l.path + ".1"
	_ = os.Remove(backup)
	_ = os.Rename(l.path, backup)
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		l.file = nil
		return
	}
	l.file = f
	l.fileSize = 0
	_, _ = os.Stderr.WriteString("[" + time.Now().Format("2006-01-02 15:04:05.000") + "] [warn] 日志文件已轮转\n")
}

func (l *Logger) Debugf(format string, args ...interface{}) {
	if !l.debug {
		return
	}
	l.write("debug", fmt.Sprintf(format, args...))
}
func (l *Logger) Infof(format string, args ...interface{}) {
	l.write("info", fmt.Sprintf(format, args...))
}
func (l *Logger) Warnf(format string, args ...interface{}) {
	l.write("warn", fmt.Sprintf(format, args...))
}
func (l *Logger) Errorf(format string, args ...interface{}) {
	l.write("error", fmt.Sprintf(format, args...))
}

func (l *Logger) Entries(limit int, minLevel string) []LogEntry {
	if limit <= 0 || limit > l.max {
		limit = 300
	}
	minRank := levelRank(minLevel)

	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]LogEntry, 0, limit)
	n := len(l.ring)
	for k := 0; k < n && len(out) < limit; k++ {
		idx := n - 1 - k
		if n == l.max {
			idx = l.ringStart - 1 - k
			if idx < 0 {
				idx += l.max
			}
		}
		if levelRank(l.ring[idx].Level) >= minRank {
			out = append(out, l.ring[idx])
		}
	}
	return out
}

func (l *Logger) Clear() {
	l.mu.Lock()
	l.ring = nil
	l.ringStart = 0
	l.mu.Unlock()
}

func (l *Logger) File() string {
	if l == nil {
		return ""
	}
	return l.path
}

// DroppedTFLines reports how many log lines were discarded because the TF card
// writer could not keep up. A non-zero value means the card is too slow.
func (l *Logger) DroppedTFLines() int {
	if l == nil {
		return 0
	}
	return int(atomic.LoadInt32(&l.tfDropped))
}

func (l *Logger) Close() {
	l.mu.Lock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	l.mu.Unlock()

	if l.tfStop != nil {
		close(l.tfStop)
		<-l.tfDone
		l.tfStop = nil
	}
}

func levelRank(level string) int {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return 0
	case "warn", "warning":
		return 2
	case "error", "fatal":
		return 3
	default:
		return 1
	}
}
