// Package logger provides structured logging.
package logger

import (
	"log"
	"os"
)

// Logger wraps the standard logger with structured output.
type Logger struct {
	info *log.Logger
	err  *log.Logger
}

// New creates a new Logger writing to stderr.
func New() *Logger {
	return &Logger{
		info: log.New(os.Stderr, "[INFO] ", log.LstdFlags),
		err:  log.New(os.Stderr, "[ERROR] ", log.LstdFlags),
	}
}

// Info logs an informational message.
func (l *Logger) Info(msg string, args ...interface{}) {
	l.info.Printf(msg, args...)
}

// Error logs an error message.
func (l *Logger) Error(msg string, args ...interface{}) {
	l.err.Printf(msg, args...)
}
