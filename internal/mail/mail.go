package mail

import "context"

// MailService sends alert emails.
type MailService interface {
	Send(ctx context.Context, subject, body string) error
	// AlertRecipients returns addresses a successful Send delivers to, for logging.
	// Implementations that do not send mail may return nil.
	AlertRecipients() []string
}
