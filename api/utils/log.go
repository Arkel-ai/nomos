// Package utils gathers cross-cutting helpers used throughout the API:
// the slog-backed logger, external client initialisation, and the cost
// tracker. The flat layout mirrors the accor-smartnews reference project.
//
// Logging — this file wires log/slog with the project's canonical line
// format and exposes a small *Logger wrapper carrying printf-style helpers
// (Infof, Debugf, Warnf, Errorf, Fatalf):
//
//	HH:MM:SS LEVEL   logger.name            message
package utils

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

const (
	loggerKey   = "logger"
	defaultName = "app"
	nameWidth   = 22
	levelWidth  = 7
)

var levelVar slog.LevelVar

func init() {
	// Install the project formatter as the slog default at package init so
	// any utils.NewLogger(...) call made before main() runs (package-level
	// vars) already produces correctly-formatted output. main() then calls
	// InitLogger() to set the level from configuration.
	slog.SetDefault(slog.New(newTextHandler(os.Stderr, &levelVar)))
}

// InitLogger updates the active log level from a string ("DEBUG", "INFO",
// "WARN", "ERROR" — case-insensitive). Unknown values fall back to INFO.
func InitLogger(level string) {
	levelVar.Set(parseLevel(level))
}

// SetLogLevel updates the level at runtime.
func SetLogLevel(level string) {
	levelVar.Set(parseLevel(level))
}

// NewLogger returns a project Logger tagged with the given name (mirrors
// python's `logging.getLogger(__name__)`). Use it once at package init:
//
//	var log = utils.NewLogger("controllers.chat")
//
// The slog default handler is resolved lazily on each log call.
func NewLogger(name string) *Logger {
	return &Logger{name: name}
}

// Logger is a thin printf-style wrapper around log/slog.
type Logger struct {
	name string
}

func (l *Logger) Debug(msg string) { l.log(slog.LevelDebug, msg) }
func (l *Logger) Info(msg string)  { l.log(slog.LevelInfo, msg) }
func (l *Logger) Warn(msg string)  { l.log(slog.LevelWarn, msg) }
func (l *Logger) Error(msg string) { l.log(slog.LevelError, msg) }

func (l *Logger) Debugf(format string, args ...any) {
	l.log(slog.LevelDebug, fmt.Sprintf(format, args...))
}
func (l *Logger) Infof(format string, args ...any) {
	l.log(slog.LevelInfo, fmt.Sprintf(format, args...))
}
func (l *Logger) Warnf(format string, args ...any) {
	l.log(slog.LevelWarn, fmt.Sprintf(format, args...))
}
func (l *Logger) Errorf(format string, args ...any) {
	l.log(slog.LevelError, fmt.Sprintf(format, args...))
}

// Fatalf logs at ERROR level and exits with status 1.
func (l *Logger) Fatalf(format string, args ...any) {
	l.log(slog.LevelError, fmt.Sprintf(format, args...))
	os.Exit(1)
}

func (l *Logger) log(level slog.Level, msg string) {
	logger := slog.Default()
	ctx := context.Background()
	if !logger.Enabled(ctx, level) {
		return
	}
	logger.LogAttrs(ctx, level, msg, slog.String(loggerKey, l.name))
}

type textHandler struct {
	w     io.Writer
	level slog.Leveler
	mu    *sync.Mutex
	name  string
}

func newTextHandler(w io.Writer, level slog.Leveler) *textHandler {
	return &textHandler{w: w, level: level, mu: &sync.Mutex{}}
}

func (h *textHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *textHandler) Handle(_ context.Context, r slog.Record) error {
	name := h.name
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == loggerKey {
			name = a.Value.String()
			return false
		}
		return true
	})
	if name == "" {
		name = defaultName
	}

	var b strings.Builder
	b.Grow(64 + len(r.Message))
	b.WriteString(r.Time.Format("15:04:05"))
	b.WriteByte(' ')
	b.WriteString(padRight(levelString(r.Level), levelWidth))
	b.WriteByte(' ')
	b.WriteString(padRight(name, nameWidth))
	b.WriteByte(' ')
	b.WriteString(r.Message)
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *textHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	for _, a := range attrs {
		if a.Key == loggerKey {
			clone.name = a.Value.String()
		}
	}
	return &clone
}

func (h *textHandler) WithGroup(_ string) slog.Handler { return h }

func levelString(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARN"
	case l >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
