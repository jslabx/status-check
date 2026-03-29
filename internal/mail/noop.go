package mail

import (
	"context"
	"log/slog"
)

// NoopMailService silently discards all alerts. Used when no mail provider is enabled.
type NoopMailService struct {
	logger *slog.Logger
}

func NewNoopMailService(logger *slog.Logger) *NoopMailService {
	return &NoopMailService{logger: logger}
}

func (n *NoopMailService) AlertRecipients() []string {
	return nil
}

func (n *NoopMailService) Send(_ context.Context, subject, _ string) error {
	n.logger.Warn("alert suppressed: no mail service configured", "subject", subject)
	return nil
}
