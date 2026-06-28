package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/Reederey87/DevStrap/internal/redact"
)

type SecretString string

var Level slog.LevelVar

func Configure(out io.Writer, json bool, quiet bool, verbose int) *slog.Logger {
	level := slog.LevelInfo
	if quiet {
		level = slog.LevelError
	} else {
		switch {
		case verbose >= 2:
			level = slog.LevelDebug
		case verbose == 1:
			level = slog.LevelInfo
		}
	}
	if raw := strings.TrimSpace(os.Getenv("DEVSTRAP_LOG_LEVEL")); raw != "" {
		level = parseLevel(raw, level)
	}
	Level.Set(level)
	opts := &slog.HandlerOptions{Level: &Level, ReplaceAttr: ReplaceAttr}
	var handler slog.Handler
	if json {
		handler = slog.NewJSONHandler(out, opts)
	} else {
		handler = slog.NewTextHandler(out, opts)
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}

func ReplaceAttr(groups []string, attr slog.Attr) slog.Attr {
	if shouldRedact(attr.Key) {
		attr.Value = slog.StringValue("[REDACTED]")
		return attr
	}
	switch attr.Value.Kind() {
	case slog.KindAny:
		if _, ok := attr.Value.Any().(SecretString); ok {
			attr.Value = slog.StringValue("[REDACTED]")
		}
	case slog.KindString:
		// Value-level backstop: catch token-shaped secrets formatted into a
		// string or attached under a benign key, which key-name matching misses.
		if s := attr.Value.String(); s != "" {
			if scrubbed := redact.Scrub(s); scrubbed != s {
				attr.Value = slog.StringValue(scrubbed)
			}
		}
	}
	return attr
}

func Logger(ctx context.Context) *slog.Logger {
	_ = ctx
	return slog.Default().With("component", "devstrap")
}

func shouldRedact(key string) bool {
	key = strings.ToLower(key)
	for _, needle := range []string{"secret", "token", "password", "passwd", "credential", "private_key", "apikey", "api_key"} {
		if strings.Contains(key, needle) {
			return true
		}
	}
	return false
}

func parseLevel(raw string, fallback slog.Level) slog.Level {
	switch strings.ToLower(raw) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return fallback
	}
}
