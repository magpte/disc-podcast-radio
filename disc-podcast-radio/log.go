package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LogEntry is one structured log line kept in the in-memory ring buffer.
type LogEntry struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// Logger writes to stderr (captured by disc-runtime) and to a rotating file.
// The in-memory ring is exposed through /api/logs so failures can be diagnosed
// without SSH access.
type Logger struct {
	mu       sync.Mutex
	ring     []LogEntry
	max      int
	file     *os.File
	path     string
	fileSize int64
}

const (
	logMaxEntries = 2000
	logMaxBytes   = 2 * 1024 * 1024
)

func NewLogger(dataDir string) *Logger {
	l := &Logger{max: logMaxEntries, path: filepath.Join(dataDir, "service.log")}
	if err := os.MkdirAll(dataDir, 0755); err == nil {
		if f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			l.file = f
			if st, err := f.Stat(); err == nil {
				l.fileSize = st.Size()
			}
		}
	}
	return l
}

func (l *Logger) write(level, msg string) {
	entry := LogEntry{
		Time:    time.Now().Format("2006-01-02 15:04:05.000"),
		Level:   level,
		Message: msg,
	}
	line := fmt.Sprintf("%s [%s] %s\n", entry.Time, level, msg)

	l.mu.Lock()
	defer l.mu.Unlock()

	l.ring = append(l.ring, entry)
	if len(l.ring) > l.max {
		copy(l.ring, l.ring[len(l.ring)-l.max:])
		l.ring = l.ring[:l.max]
	}

	_, _ = os.Stderr.WriteString(line)
	if l.file == nil {
		return
	}
	n, err := l.file.WriteString(line)
	if err == nil {
		l.fileSize += int64(n)
	}
	if l.fileSize > logMaxBytes {
		l.rotateLocked()
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
	for i := len(l.ring) - 1; i >= 0 && len(out) < limit; i-- {
		if levelRank(l.ring[i].Level) >= minRank {
			out = append(out, l.ring[i])
		}
	}
	return out
}

func (l *Logger) Clear() {
	l.mu.Lock()
	l.ring = nil
	l.mu.Unlock()
}

func (l *Logger) File() string {
	if l == nil {
		return ""
	}
	return l.path
}

func (l *Logger) Close() {
	l.mu.Lock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	l.mu.Unlock()
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
