package mail

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ses"
)

func testDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type mockSESClient struct {
	lastInput *ses.SendEmailInput
	err       error
	quotaErr  error
}

func (m *mockSESClient) GetSendQuota(_ context.Context, _ *ses.GetSendQuotaInput, _ ...func(*ses.Options)) (*ses.GetSendQuotaOutput, error) {
	if m.quotaErr != nil {
		return nil, m.quotaErr
	}
	return &ses.GetSendQuotaOutput{}, nil
}

func (m *mockSESClient) SendEmail(_ context.Context, input *ses.SendEmailInput, _ ...func(*ses.Options)) (*ses.SendEmailOutput, error) {
	m.lastInput = input
	if m.err != nil {
		return nil, m.err
	}
	return &ses.SendEmailOutput{}, nil
}

func TestSesMailService_AlertRecipients(t *testing.T) {
	mock := &mockSESClient{}
	to := []string{"a@example.com", "b@example.com"}
	svc := NewSesMailService(context.Background(), testDiscardLogger(), mock, "from@example.com", to)

	got := svc.AlertRecipients()
	if !slices.Equal(got, to) {
		t.Errorf("AlertRecipients: got %v, want %v", got, to)
	}
	got[0] = "mutated@example.com"
	got2 := svc.AlertRecipients()
	if got2[0] != "a@example.com" {
		t.Errorf("AlertRecipients must not expose internal slice: after mutating first return, second call got %q", got2[0])
	}
}

func TestSesMailService_Send_Success(t *testing.T) {
	mock := &mockSESClient{}
	svc := NewSesMailService(context.Background(), testDiscardLogger(), mock, "from@example.com", []string{"to@example.com"})

	err := svc.Send(context.Background(), "Test Subject", "Test Body")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mock.lastInput == nil {
		t.Fatal("expected SendEmail to be called")
	}
}

func TestSesMailService_Send_PassesCorrectFields(t *testing.T) {
	mock := &mockSESClient{}
	svc := NewSesMailService(context.Background(), testDiscardLogger(), mock, "from@example.com", []string{"to@example.com"})

	err := svc.Send(context.Background(), "My Subject", "My Body")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	in := mock.lastInput
	if *in.Source != "from@example.com" {
		t.Errorf("unexpected From: %s", *in.Source)
	}
	if len(in.Destination.ToAddresses) != 1 || in.Destination.ToAddresses[0] != "to@example.com" {
		t.Errorf("unexpected To: %v", in.Destination.ToAddresses)
	}
	if *in.Message.Subject.Data != "My Subject" {
		t.Errorf("unexpected subject: %s", *in.Message.Subject.Data)
	}
	if *in.Message.Body.Text.Data != "My Body" {
		t.Errorf("unexpected body: %s", *in.Message.Body.Text.Data)
	}
}

func TestSesMailService_Send_MultipleRecipients(t *testing.T) {
	mock := &mockSESClient{}
	to := []string{"a@example.com", "b@example.com", "c@example.com"}
	svc := NewSesMailService(context.Background(), testDiscardLogger(), mock, "from@example.com", to)

	err := svc.Send(context.Background(), "Alert", "Something is down")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.lastInput.Destination.ToAddresses) != 3 {
		t.Errorf("expected 3 recipients, got %d", len(mock.lastInput.Destination.ToAddresses))
	}
}

func TestSesMailService_Send_SESError(t *testing.T) {
	sesErr := errors.New("SES rate limit exceeded")
	mock := &mockSESClient{err: sesErr}
	svc := NewSesMailService(context.Background(), testDiscardLogger(), mock, "from@example.com", []string{"to@example.com"})

	err := svc.Send(context.Background(), "Alert", "Something is down")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sesErr) {
		t.Errorf("expected wrapped SES error, got: %v", err)
	}
}

func TestNewSesMailService_LogsConnectionSuccessWhenGetSendQuotaSucceeds(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	mock := &mockSESClient{}
	_ = NewSesMailService(context.Background(), logger, mock, "from@example.com", []string{"to@example.com"})
	if !strings.Contains(buf.String(), "SES connection check (GetSendQuota) passed") {
		t.Fatalf("expected success log in output; got: %q", buf.String())
	}
}

func TestNewSesMailService_PanicsWhenGetSendQuotaFails(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	mock := &mockSESClient{quotaErr: errors.New("unreachable")}
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic when GetSendQuota fails")
		}
	}()
	NewSesMailService(context.Background(), logger, mock, "from@example.com", []string{"to@example.com"})
	if strings.Contains(buf.String(), "SES connection check (GetSendQuota) passed") {
		t.Errorf("did not expect success log when GetSendQuota fails; got: %s", buf.String())
	}
}

func TestSesMailService_CallerMutationAfterConstructionHasNoEffect(t *testing.T) {
	original := []string{"original@example.com"}
	mock := &mockSESClient{}
	svc := NewSesMailService(context.Background(), testDiscardLogger(), mock, "from@example.com", original)

	// Mutate the caller's slice after the service is constructed.
	original[0] = "mutated@example.com"

	_ = svc.Send(context.Background(), "s", "b")

	if mock.lastInput == nil {
		t.Fatal("expected SendEmail to be called")
	}
	if got := mock.lastInput.Destination.ToAddresses[0]; got != "original@example.com" {
		t.Errorf("got %q, want original@example.com (mutating caller slice after NewSesMailService should not change recipients)", got)
	}
}
