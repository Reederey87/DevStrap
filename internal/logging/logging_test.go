package logging

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestConfigureRedactsSecretLikeKeysAndSecretStrings(t *testing.T) {
	var buf bytes.Buffer
	logger := Configure(&buf, true, false, 0)
	logger.Info("test", "api_token", "super-secret", "value", SecretString("classified"))
	out := buf.String()
	if strings.Contains(out, "super-secret") || strings.Contains(out, "classified") {
		t.Fatalf("log output leaked secret: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("log output = %s, want redaction marker", out)
	}
}

func TestLoggerDoesNotEmitContextAttribute(t *testing.T) {
	var buf bytes.Buffer
	Configure(&buf, true, false, 0)
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "should-not-appear")
	Logger(ctx).Info("hello")
	out := buf.String()
	if strings.Contains(out, "ctx=") || strings.Contains(out, "context.") || strings.Contains(out, "should-not-appear") {
		t.Fatalf("logger leaked context internals: %s", out)
	}
}

func TestConfigureScrubsTokenShapedValuesUnderBenignKey(t *testing.T) {
	var buf bytes.Buffer
	logger := Configure(&buf, true, false, 0)
	// Built at runtime so no contiguous secret literal is committed.
	token := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"
	logger.Info("event", "note", "deploying with "+token+" now")
	out := buf.String()
	if strings.Contains(out, token) {
		t.Fatalf("token-shaped value under benign key leaked: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("expected redaction marker: %s", out)
	}
}

func TestEnvironmentLogLevelOverridesFlags(t *testing.T) {
	t.Setenv("DEVSTRAP_LOG_LEVEL", "debug")
	var buf bytes.Buffer
	Configure(&buf, true, true, 0)
	slog.Debug("visible")
	if !strings.Contains(buf.String(), "visible") {
		t.Fatalf("debug log was not emitted: %s", buf.String())
	}
}
