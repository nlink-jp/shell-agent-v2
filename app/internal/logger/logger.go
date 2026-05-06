// Package logger provides file-based structured logging.
package logger

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
)

var (
	global *Logger
	once   sync.Once
)

// Level controls which log calls reach the file. Calls below
// the configured level are silently dropped.
//
// Default after Init is LevelInfo so DEBUG-tier output (which
// contains user message snippets, tool arguments, and LLM
// response bodies) is suppressed without an opt-in. Privacy
// rationale: app.log is the easiest path for sensitive
// conversation data to leak off-device, so we err on the side
// of less. The Settings UI exposes a select to switch to Debug
// for diagnosis. See docs/en/privacy-controls.md §3.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// Logger writes structured logs to a file and stderr.
type Logger struct {
	mu    sync.RWMutex
	level Level
	file  *os.File
	info  *log.Logger
	err   *log.Logger
	dbg   *log.Logger
}

// Init initializes the global logger with a log file in the given directory.
func Init(dir string) error {
	var initErr error
	once.Do(func() {
		if err := os.MkdirAll(dir, 0700); err != nil {
			initErr = err
			return
		}
		path := filepath.Join(dir, "app.log")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			initErr = err
			return
		}
		global = &Logger{
			level: LevelInfo, // default: drop DEBUG so conversation snippets don't reach disk
			file:  f,
			info:  log.New(f, "[INFO]  ", log.LstdFlags|log.Lmicroseconds),
			err:   log.New(f, "[ERROR] ", log.LstdFlags|log.Lmicroseconds),
			dbg:   log.New(f, "[DEBUG] ", log.LstdFlags|log.Lmicroseconds),
		}
		// Also print errors to stderr
		global.err.SetOutput(multiWriter{f, os.Stderr})
	})
	return initErr
}

// SetLevel sets the runtime threshold. Safe to call from any
// goroutine; takes effect on the next Info/Debug call.
func SetLevel(l Level) {
	if global == nil {
		return
	}
	global.mu.Lock()
	global.level = l
	global.mu.Unlock()
}

// CurrentLevel returns the active threshold (test / debug helper).
func CurrentLevel() Level {
	if global == nil {
		return LevelInfo
	}
	global.mu.RLock()
	defer global.mu.RUnlock()
	return global.level
}

func enabled(at Level) bool {
	if global == nil {
		return false
	}
	global.mu.RLock()
	defer global.mu.RUnlock()
	return at >= global.level
}

// Info logs an informational message (gated by level).
func Info(msg string, args ...any) {
	if !enabled(LevelInfo) {
		return
	}
	global.info.Printf(msg, args...)
}

// Error logs an error message. Always emitted regardless of
// level — errors are diagnostic-critical and never sensitive
// content (the convention is to log fmt.Errorf-wrapped values).
func Error(msg string, args ...any) {
	if global != nil {
		global.err.Printf(msg, args...)
	}
}

// Debug logs a debug message (gated by level).
func Debug(msg string, args ...any) {
	if !enabled(LevelDebug) {
		return
	}
	global.dbg.Printf(msg, args...)
}

// Close flushes and closes the log file.
func Close() {
	if global != nil && global.file != nil {
		global.file.Close()
	}
}

// Path returns the log file path.
func Path() string {
	if global != nil && global.file != nil {
		return global.file.Name()
	}
	return ""
}

type multiWriter struct {
	a, b *os.File
}

func (w multiWriter) Write(p []byte) (int, error) {
	w.a.Write(p)
	return w.b.Write(p)
}

// Truncate truncates a string for logging.
func Truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + fmt.Sprintf("... (%d bytes truncated)", len(s)-maxLen)
}
