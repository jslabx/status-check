package mail

import "context"

// MailService sends alert emails.
type MailService interface {
	Send(ctx context.Context, subject, body string) error
}
