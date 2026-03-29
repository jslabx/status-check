package mail

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ses"
	"github.com/aws/aws-sdk-go-v2/service/ses/types"
)

// sesAPI is the part of the SES client we need; tests can stub it.
type sesAPI interface {
	SendEmail(ctx context.Context, params *ses.SendEmailInput, optFns ...func(*ses.Options)) (*ses.SendEmailOutput, error)
	GetSendQuota(ctx context.Context, params *ses.GetSendQuotaInput, optFns ...func(*ses.Options)) (*ses.GetSendQuotaOutput, error)
}

// SesMailService sends mail through Amazon SES using the usual AWS credential
// sources (env vars, IAM role, shared config files, etc.).
type SesMailService struct {
	client sesAPI
	from   string
	to     []string
	logger *slog.Logger
}

// NewSesMailService builds an SES mailer and verifies connection health.
func NewSesMailService(ctx context.Context, logger *slog.Logger, client sesAPI, from string, to []string) *SesMailService {
	if _, err := client.GetSendQuota(ctx, &ses.GetSendQuotaInput{}); err != nil {
		panic(fmt.Errorf("SES connection check (GetSendQuota) failed: %w", err))
	}
	logger.Info("SES connection check (GetSendQuota) passed")
	toCopy := make([]string, len(to))
	copy(toCopy, to)
	return &SesMailService{
		client: client,
		from:   from,
		to:     toCopy,
		logger: logger,
	}
}

func (s *SesMailService) Send(ctx context.Context, subject, body string) error {
	input := &ses.SendEmailInput{
		Source: aws.String(s.from),
		Destination: &types.Destination{
			ToAddresses: s.to,
		},
		Message: &types.Message{
			Subject: &types.Content{
				Data:    aws.String(subject),
				Charset: aws.String("UTF-8"),
			},
			Body: &types.Body{
				Text: &types.Content{
					Data:    aws.String(body),
					Charset: aws.String("UTF-8"),
				},
			},
		},
	}

	_, err := s.client.SendEmail(ctx, input)
	if err != nil {
		return fmt.Errorf("sending email via SES: %w", err)
	}
	return nil
}
