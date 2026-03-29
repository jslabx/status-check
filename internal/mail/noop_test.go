package mail

import (
	"context"
	"slices"
	"testing"

	"status-check/internal/testutil"
)

func TestNoopMailService_AlertRecipients(t *testing.T) {
	svc := NewNoopMailService(testDiscardLogger())
	if svc.AlertRecipients() != nil {
		t.Errorf("AlertRecipients: got %v, want nil", svc.AlertRecipients())
	}
}

func TestNoopMailService_Send_LogsSuppressedAlert(t *testing.T) {
	logger, cap := testutil.NewCaptureLogger()
	svc := NewNoopMailService(logger)

	err := svc.Send(context.Background(), "Down: https://example.com", "body")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := cap.Messages()
	want := []string{"alert suppressed: no mail service configured"}
	if !slices.Equal(got, want) {
		t.Fatalf("log messages\ngot:  %q\nwant: %q", got, want)
	}
}
