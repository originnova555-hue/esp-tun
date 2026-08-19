// Package logging wires the process-wide logger. Output goes to stderr so it
// lands in the journal when run under systemd.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// Setup installs a text logger at the named level and returns it.
func Setup(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "error":
		lv = slog.LevelError
	case "warn", "warning":
		lv = slog.LevelWarn
	case "debug":
		lv = slog.LevelDebug
	default:
		lv = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: lv,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			// systemd already stamps every line with a timestamp.
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
	l := slog.New(h)
	slog.SetDefault(l)
	return l
}
