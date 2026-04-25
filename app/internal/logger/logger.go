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

// Logger writes structured logs to a file and stderr.
type Logger struct {
	file *os.File
	info *log.Logger
	err  *log.Logger
	dbg  *log.Logger
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
			file: f,
			info: log.New(f, "[INFO]  ", log.LstdFlags|log.Lmicroseconds),
			err:  log.New(f, "[ERROR] ", log.LstdFlags|log.Lmicroseconds),
			dbg:  log.New(f, "[DEBUG] ", log.LstdFlags|log.Lmicroseconds),
		}
		// Also print errors to stderr
		global.err.SetOutput(multiWriter{f, os.Stderr})
	})
	return initErr
}

// Info logs an informational message.
func Info(msg string, args ...any) {
	if global != nil {
		global.info.Printf(msg, args...)
	}
}

// Error logs an error message.
func Error(msg string, args ...any) {
	if global != nil {
		global.err.Printf(msg, args...)
	}
}

// Debug logs a debug message.
func Debug(msg string, args ...any) {
	if global != nil {
		global.dbg.Printf(msg, args...)
	}
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
