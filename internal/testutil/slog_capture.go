package testutil

import (
	"context"
	"log/slog"
	"sync"
)

// LogCapture stores slog messages in order for assertions.
type LogCapture struct {
	mu       sync.Mutex
	messages []string
}

// Messages returns a copy of captured log messages.
func (c *LogCapture) Messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.messages))
	copy(out, c.messages)
	return out
}

func (c *LogCapture) addMessage(msg string) {
	c.mu.Lock()
	c.messages = append(c.messages, msg)
	c.mu.Unlock()
}

type captureHandler struct {
	c *LogCapture
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.c.addMessage(r.Message)
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// NewCaptureLogger builds a logger whose messages land in the returned LogCapture.
func NewCaptureLogger() (*slog.Logger, *LogCapture) {
	cap := &LogCapture{}
	return slog.New(&captureHandler{c: cap}), cap
}
