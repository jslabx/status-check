// Package integration runs config load, checker, and mail against fake HTTP
// servers. Assertions stick to the outside: alert counts, recipients, and URLs.
package integration_test

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"status-check/internal/checker"
	"status-check/internal/config"
	"status-check/internal/mail"
)

const testAlertRecipient = "oncall@example.com"

// Test doubles

type sentAlert struct {
	to      string
	subject string
	body    string
}

// recordingMailService remembers each Send like production code would route mail.
type recordingMailService struct {
	to     string
	calls  atomic.Int32
	alerts []sentAlert
	mu     sync.Mutex
}

func newRecordingMailService() *recordingMailService {
	return &recordingMailService{to: testAlertRecipient}
}

func (r *recordingMailService) Send(_ context.Context, subject, body string) error {
	r.calls.Add(1)
	r.mu.Lock()
	r.alerts = append(r.alerts, sentAlert{to: r.to, subject: subject, body: body})
	r.mu.Unlock()
	return nil
}

func (r *recordingMailService) CallCount() int {
	return int(r.calls.Load())
}

// Alerts returns a snapshot of all recorded Send calls.
func (r *recordingMailService) Alerts() []sentAlert {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]sentAlert, len(r.alerts))
	copy(out, r.alerts)
	return out
}

// assertAllAlertsDeliveredTo fails if any alert went to the wrong address.
func assertAllAlertsDeliveredTo(t *testing.T, svc *recordingMailService, wantTo string) {
	t.Helper()
	for i, a := range svc.Alerts() {
		if a.to != wantTo {
			t.Errorf("alert[%d]: expected recipient %q, got %q (subject: %q)", i, wantTo, a.to, a.subject)
		}
	}
}

// assertAlertSentForURL fails if no alert subject mentions the URL.
func assertAlertSentForURL(t *testing.T, svc *recordingMailService, url string) {
	t.Helper()
	for _, a := range svc.Alerts() {
		if strings.Contains(a.subject, url) {
			return
		}
	}
	t.Errorf("expected an alert for URL %q but none was found (all subjects: %v)", url, alertSubjects(svc))
}

// assertNoAlertSentForURL fails if any alert subject mentions the URL.
func assertNoAlertSentForURL(t *testing.T, svc *recordingMailService, url string) {
	t.Helper()
	for _, a := range svc.Alerts() {
		if strings.Contains(a.subject, url) {
			t.Errorf("expected no alert for URL %q but found one (subject: %q)", url, a.subject)
			return
		}
	}
}

func alertSubjects(svc *recordingMailService) []string {
	alerts := svc.Alerts()
	subjects := make([]string, len(alerts))
	for i, a := range alerts {
		subjects[i] = a.subject
	}
	return subjects
}

func waitForAlertCount(t *testing.T, svc *recordingMailService, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for svc.CallCount() < want {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d alerts, got %d", want, svc.CallCount())
		}
		time.Sleep(time.Millisecond)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func writeConfigFile(t *testing.T, urls []string, timeoutSecs int, recheckIntervalSecs int) *config.Config {
	t.Helper()

	urlBlock := ""
	for _, u := range urls {
		urlBlock += fmt.Sprintf("  - %s\n", u)
	}

	content := fmt.Sprintf(`
urls:
%schecker:
  timeout_seconds: %d
  interval_seconds: 60
  recheck_interval_seconds: %d
`, urlBlock, timeoutSecs, recheckIntervalSecs)

	f, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatalf("creating temp config: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	f.Close()

	cfg, err := config.Load(f.Name())
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	return cfg
}

func buildChecker(cfg *config.Config, mailServices []mail.MailService) *checker.URLChecker {
	client := &http.Client{Timeout: time.Duration(cfg.Checker.TimeoutSeconds) * time.Second}
	recheckInterval := time.Duration(cfg.Checker.RecheckIntervalSeconds) * time.Second
	return checker.New(client, mailServices, testLogger(), cfg.URLs, recheckInterval)
}

func TestIntegration_ConcurrentChecks_FastAlertBeforeSlowResponds(t *testing.T) {
	unblockSlow := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-unblockSlow
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer slow.Close()

	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer fast.Close()

	mailSvc := newRecordingMailService()
	cfg := writeConfigFile(t, []string{fast.URL, slow.URL}, 5, 0)
	c := buildChecker(cfg, []mail.MailService{mailSvc})

	done := make(chan struct{})
	go func() {
		c.Check(context.Background())
		close(done)
	}()

	waitForAlertCount(t, mailSvc, 1, 3*time.Second)
	alerts := mailSvc.Alerts()
	if len(alerts) != 1 || !strings.Contains(alerts[0].subject, fast.URL) {
		t.Fatalf("first alert should reference fast URL before slow responds; got %+v", alerts)
	}

	close(unblockSlow)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Check did not finish after unblocking slow URL")
	}

	if mailSvc.CallCount() != 2 {
		t.Fatalf("expected 2 alerts, got %d", mailSvc.CallCount())
	}
	alerts = mailSvc.Alerts()
	if !strings.Contains(alerts[1].subject, slow.URL) {
		t.Fatalf("second alert should reference slow URL; subjects=%v", alertSubjects(mailSvc))
	}
	assertAllAlertsDeliveredTo(t, mailSvc, testAlertRecipient)
}

func TestIntegration_AllURLsHealthy_NoAlerts(t *testing.T) {
	s1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer s1.Close()
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201) }))
	defer s2.Close()
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer s3.Close()

	mailSvc := newRecordingMailService()
	cfg := writeConfigFile(t, []string{s1.URL, s2.URL, s3.URL}, 5, 0)
	buildChecker(cfg, []mail.MailService{mailSvc}).Check(context.Background())

	if got := mailSvc.CallCount(); got != 0 {
		t.Errorf("expected 0 alerts for all-healthy URLs, got %d", got)
	}
	assertNoAlertSentForURL(t, mailSvc, s1.URL)
	assertNoAlertSentForURL(t, mailSvc, s2.URL)
	assertNoAlertSentForURL(t, mailSvc, s3.URL)
}

func TestIntegration_MixedStatuses_AlertsOnlyForFailed(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer healthy.Close()
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	defer notFound.Close()
	serverErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer serverErr.Close()
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer unavailable.Close()

	mailSvc := newRecordingMailService()
	cfg := writeConfigFile(t, []string{healthy.URL, notFound.URL, serverErr.URL, unavailable.URL}, 5, 0)
	buildChecker(cfg, []mail.MailService{mailSvc}).Check(context.Background())

	if got := mailSvc.CallCount(); got != 3 {
		t.Errorf("expected 3 alerts (one per failing URL), got %d", got)
	}
	assertAllAlertsDeliveredTo(t, mailSvc, testAlertRecipient)
	assertAlertSentForURL(t, mailSvc, notFound.URL)
	assertAlertSentForURL(t, mailSvc, serverErr.URL)
	assertAlertSentForURL(t, mailSvc, unavailable.URL)
	assertNoAlertSentForURL(t, mailSvc, healthy.URL)
}

func TestIntegration_ConnectionRefused_Alerts(t *testing.T) {
	// Grab a free port then immediately release it so nothing is listening.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()

	refusedURL := "http://" + addr
	mailSvc := newRecordingMailService()
	cfg := writeConfigFile(t, []string{refusedURL}, 5, 0)
	buildChecker(cfg, []mail.MailService{mailSvc}).Check(context.Background())

	if got := mailSvc.CallCount(); got != 1 {
		t.Errorf("expected 1 alert for connection refused, got %d", got)
	}
	assertAllAlertsDeliveredTo(t, mailSvc, testAlertRecipient)
	assertAlertSentForURL(t, mailSvc, refusedURL)
}

func TestIntegration_Timeout_Alerts(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond) // outlasts the 10ms timeout
		w.WriteHeader(200)
	}))
	defer slow.Close()

	mailSvc := newRecordingMailService()
	cfg := writeConfigFile(t, []string{slow.URL}, 1, 0)

	shortClient := &http.Client{Timeout: 10 * time.Millisecond}
	c := checker.New(shortClient, []mail.MailService{mailSvc}, testLogger(), cfg.URLs, 0)
	c.Check(context.Background())

	if got := mailSvc.CallCount(); got != 1 {
		t.Errorf("expected 1 alert for timeout, got %d", got)
	}
	assertAllAlertsDeliveredTo(t, mailSvc, testAlertRecipient)
	assertAlertSentForURL(t, mailSvc, slow.URL)
}

func TestIntegration_NoopMailService_DoesNotPanic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer server.Close()

	noopSvc := mail.NewNoopMailService(testLogger())
	cfg := writeConfigFile(t, []string{server.URL}, 5, 0)
	buildChecker(cfg, []mail.MailService{noopSvc}).Check(context.Background())
}

func TestIntegration_ConfigDefaultsApplied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer server.Close()

	content := fmt.Sprintf("urls:\n  - %s\n", server.URL)
	f, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatalf("creating temp config: %v", err)
	}
	_, _ = f.WriteString(content)
	f.Close()

	cfg, err := config.Load(f.Name())
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	if cfg.Checker.TimeoutSeconds != 10 {
		t.Errorf("expected default timeout 10s, got %d", cfg.Checker.TimeoutSeconds)
	}
	if cfg.Checker.IntervalSeconds != 60 {
		t.Errorf("expected default interval 60s, got %d", cfg.Checker.IntervalSeconds)
	}
}

func TestIntegration_RunLoop_ChecksAndStops(t *testing.T) {
	var hitCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitCount.Add(1)
		w.WriteHeader(200)
	}))
	defer server.Close()

	mailSvc := newRecordingMailService()
	cfg := writeConfigFile(t, []string{server.URL}, 5, 0)
	c := buildChecker(cfg, []mail.MailService{mailSvc})

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	// Long interval: only the first check runs before the context times out.
	c.RunLoop(ctx, 10*time.Second)

	if got := hitCount.Load(); got < 1 {
		t.Errorf("expected at least 1 HTTP hit, got %d", got)
	}
	if mailSvc.CallCount() != 0 {
		t.Errorf("expected no alerts for a healthy URL, got %d", mailSvc.CallCount())
	}
	assertNoAlertSentForURL(t, mailSvc, server.URL)
}

func TestIntegration_RecheckInterval_SuppressesDuplicateAlerts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	mailSvc := newRecordingMailService()
	// One hour between repeat alerts: two back-to-back checks should mail once.
	cfg := writeConfigFile(t, []string{server.URL}, 5, 3600)
	c := buildChecker(cfg, []mail.MailService{mailSvc})

	c.Check(context.Background())
	c.Check(context.Background()) // still inside window

	if mailSvc.CallCount() != 1 {
		t.Errorf("expected 1 alert (second suppressed), got %d", mailSvc.CallCount())
	}
	assertAllAlertsDeliveredTo(t, mailSvc, testAlertRecipient)
	assertAlertSentForURL(t, mailSvc, server.URL)
}

func TestIntegration_RecheckInterval_AlertsAgainAfterWindowElapses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	mailSvc := newRecordingMailService()
	cfg := writeConfigFile(t, []string{server.URL}, 5, 0)
	// Short window in code so the test does not sleep for a full second.
	shortInterval := 20 * time.Millisecond
	client := &http.Client{Timeout: 5 * time.Second}
	c := checker.New(client, []mail.MailService{mailSvc}, testLogger(), cfg.URLs, shortInterval)

	c.Check(context.Background())
	time.Sleep(30 * time.Millisecond)
	c.Check(context.Background())

	if mailSvc.CallCount() != 2 {
		t.Errorf("expected 2 alerts (window elapsed), got %d", mailSvc.CallCount())
	}
	assertAllAlertsDeliveredTo(t, mailSvc, testAlertRecipient)
}

func TestIntegration_RecheckInterval_RecoveryClearsWindow(t *testing.T) {
	// After a 2xx, the next failure should alert even if the hour window is not up.
	var statusCode atomic.Int32
	statusCode.Store(int32(http.StatusInternalServerError))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(statusCode.Load()))
	}))
	defer server.Close()

	mailSvc := newRecordingMailService()
	// Long window: recovery must reset state or the third check would stay quiet.
	cfg := writeConfigFile(t, []string{server.URL}, 5, 3600)
	c := buildChecker(cfg, []mail.MailService{mailSvc})

	c.Check(context.Background())
	statusCode.Store(int32(http.StatusOK))
	c.Check(context.Background())
	statusCode.Store(int32(http.StatusInternalServerError))
	c.Check(context.Background())

	if mailSvc.CallCount() != 2 {
		t.Errorf("expected 2 alerts (recovery cleared recheck window), got %d", mailSvc.CallCount())
	}
	assertAllAlertsDeliveredTo(t, mailSvc, testAlertRecipient)
	assertAlertSentForURL(t, mailSvc, server.URL)
}

func TestIntegration_AlertSendsThroughAllMailServices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	svcA := newRecordingMailService()
	svcB := newRecordingMailService()
	cfg := writeConfigFile(t, []string{server.URL}, 5, 0)
	buildChecker(cfg, []mail.MailService{svcA, svcB}).Check(context.Background())

	if svcA.CallCount() != 1 || svcB.CallCount() != 1 {
		t.Fatalf("expected each mail service to record 1 send, got a=%d b=%d", svcA.CallCount(), svcB.CallCount())
	}
	alertsA := svcA.Alerts()
	alertsB := svcB.Alerts()
	if len(alertsA) != 1 || len(alertsB) != 1 {
		t.Fatalf("expected one alert per service, got a=%d b=%d", len(alertsA), len(alertsB))
	}
	if alertsA[0].subject != alertsB[0].subject || alertsA[0].body != alertsB[0].body {
		t.Errorf("both services should receive the same alert; a=%+v b=%+v", alertsA[0], alertsB[0])
	}
	assertAlertSentForURL(t, svcA, server.URL)
	assertAlertSentForURL(t, svcB, server.URL)
	assertAllAlertsDeliveredTo(t, svcA, testAlertRecipient)
	assertAllAlertsDeliveredTo(t, svcB, testAlertRecipient)
}
