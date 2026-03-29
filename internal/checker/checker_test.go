package checker_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"status-check/internal/checker"
	"status-check/internal/mail"
	"status-check/internal/testutil"
)

// mockMailService counts Send calls for tests and optionally records subjects in order.
type mockMailService struct {
	calls atomic.Int32
	err   error
	mu    sync.Mutex
	// subjects is append-only order of Send subject lines (for async ordering tests).
	subjects []string
}

func (m *mockMailService) Send(_ context.Context, subject, _ string) error {
	m.calls.Add(1)
	m.mu.Lock()
	m.subjects = append(m.subjects, subject)
	m.mu.Unlock()
	return m.err
}

// Subjects returns a copy of Send subjects in invocation order.
func (m *mockMailService) Subjects() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.subjects))
	copy(out, m.subjects)
	return out
}

// waitForEachMailServiceCalls blocks until every mock has at least wantCalls Send invocations.
// Use when Check (or notify) runs concurrently and you need to observe progress before it returns.
func waitForEachMailServiceCalls(t *testing.T, wantCalls int32, timeout time.Duration, mocks ...*mockMailService) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		allOK := true
		for _, m := range mocks {
			if m.calls.Load() < wantCalls {
				allOK = false
				break
			}
		}
		if allOK {
			return
		}
		if time.Now().After(deadline) {
			var got []int32
			for _, m := range mocks {
				got = append(got, m.calls.Load())
			}
			t.Fatalf("timed out waiting for each mock to reach %d calls, got %v", wantCalls, got)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForMailCalls(t *testing.T, m *mockMailService, want int32, timeout time.Duration) {
	t.Helper()
	waitForEachMailServiceCalls(t, want, timeout, m)
}

func newChecker(t *testing.T, urls []string, timeout time.Duration, recheckInterval time.Duration, mailSvc *mockMailService) (*checker.URLChecker, *testutil.LogCapture) {
	t.Helper()
	client := &http.Client{Timeout: timeout}
	logger, cap := testutil.NewCaptureLogger()
	return checker.New(client, []mail.MailService{mailSvc}, logger, urls, recheckInterval), cap
}

func assertLogsEqual(t *testing.T, cap *testutil.LogCapture, want []string) {
	t.Helper()
	got := cap.Messages()
	if !slices.Equal(got, want) {
		t.Fatalf("log messages\ngot:  %q\nwant: %q", got, want)
	}
}

// assertLogsEqualUnordered compares log multisets (order ignored). Use when URLs are checked concurrently.
func assertLogsEqualUnordered(t *testing.T, cap *testutil.LogCapture, want []string) {
	t.Helper()
	got := slices.Clone(cap.Messages())
	w := slices.Clone(want)
	slices.Sort(got)
	slices.Sort(w)
	if !slices.Equal(got, w) {
		t.Fatalf("log messages (unordered)\ngot:  %q\nwant: %q", cap.Messages(), want)
	}
}

func TestCheck_2xxDoesNotAlert(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	mail := &mockMailService{}
	c, logs := newChecker(t, []string{server.URL}, 5*time.Second, 0, mail)
	c.Check(context.Background())

	if mail.calls.Load() != 0 {
		t.Errorf("expected 0 alert calls, got %d", mail.calls.Load())
	}
	assertLogsEqual(t, logs, []string{"checking URL", "check passed"})
}

func TestCheck_201DoesNotAlert(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	mail := &mockMailService{}
	c, logs := newChecker(t, []string{server.URL}, 5*time.Second, 0, mail)
	c.Check(context.Background())

	if mail.calls.Load() != 0 {
		t.Errorf("expected 0 alert calls, got %d", mail.calls.Load())
	}
	assertLogsEqual(t, logs, []string{"checking URL", "check passed"})
}

func TestCheck_ConcurrentURLs_FastFailureAlertsBeforeSlowResponds(t *testing.T) {
	// Slow URL does not respond until the test unblocks it; the fast URL fails immediately.
	// The first alert must be sent while the slow request is still in flight.
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

	mail := &mockMailService{}
	c, _ := newChecker(t, []string{fast.URL, slow.URL}, 5*time.Second, 0, mail)

	done := make(chan struct{})
	go func() {
		c.Check(context.Background())
		close(done)
	}()

	waitForMailCalls(t, mail, 1, 3*time.Second)
	subs := mail.Subjects()
	if len(subs) != 1 || !strings.Contains(subs[0], fast.URL) {
		t.Fatalf("first alert should be for fast URL before slow responds; subjects=%q", subs)
	}

	close(unblockSlow)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Check did not finish after unblocking slow URL")
	}

	if mail.calls.Load() != 2 {
		t.Fatalf("expected 2 alerts, got %d", mail.calls.Load())
	}
	subs = mail.Subjects()
	if !strings.Contains(subs[1], slow.URL) {
		t.Fatalf("second alert should be for slow URL; subjects=%q", subs)
	}
}

func TestCheck_MultipleURLsAllPass(t *testing.T) {
	makeServer := func(status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
	}
	s1 := makeServer(http.StatusOK)
	defer s1.Close()
	s2 := makeServer(http.StatusNoContent)
	defer s2.Close()

	mail := &mockMailService{}
	c, logs := newChecker(t, []string{s1.URL, s2.URL}, 5*time.Second, 0, mail)
	c.Check(context.Background())

	if mail.calls.Load() != 0 {
		t.Errorf("expected 0 alert calls, got %d", mail.calls.Load())
	}
	assertLogsEqualUnordered(t, logs, []string{
		"checking URL", "check passed",
		"checking URL", "check passed",
	})
}

func TestCheck_404Alerts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	mail := &mockMailService{}
	c, logs := newChecker(t, []string{server.URL}, 5*time.Second, 0, mail)
	c.Check(context.Background())

	if mail.calls.Load() != 1 {
		t.Errorf("expected 1 alert call, got %d", mail.calls.Load())
	}
	assertLogsEqual(t, logs, []string{"checking URL", "non-2xx response", "alert sent"})
}

func TestCheck_500Alerts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	mail := &mockMailService{}
	c, logs := newChecker(t, []string{server.URL}, 5*time.Second, 0, mail)
	c.Check(context.Background())

	if mail.calls.Load() != 1 {
		t.Errorf("expected 1 alert call, got %d", mail.calls.Load())
	}
	assertLogsEqual(t, logs, []string{"checking URL", "non-2xx response", "alert sent"})
}

func TestCheck_301Alerts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer server.Close()

	mailSvc := &mockMailService{}
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	logger, logs := testutil.NewCaptureLogger()
	c := checker.New(client, []mail.MailService{mailSvc}, logger, []string{server.URL}, 0)
	c.Check(context.Background())

	if mailSvc.calls.Load() != 1 {
		t.Errorf("expected 1 alert call, got %d", mailSvc.calls.Load())
	}
	assertLogsEqual(t, logs, []string{"checking URL", "non-2xx response", "alert sent"})
}

func TestCheck_ConnectionRefusedAlerts(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()

	mail := &mockMailService{}
	c, logs := newChecker(t, []string{"http://" + addr}, 5*time.Second, 0, mail)
	c.Check(context.Background())

	if mail.calls.Load() != 1 {
		t.Errorf("expected 1 alert call, got %d", mail.calls.Load())
	}
	assertLogsEqual(t, logs, []string{"checking URL", "request failed", "alert sent"})
}

func TestCheck_TimeoutAlerts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	mail := &mockMailService{}
	c, logs := newChecker(t, []string{server.URL}, 10*time.Millisecond, 0, mail)
	c.Check(context.Background())

	if mail.calls.Load() != 1 {
		t.Errorf("expected 1 alert call, got %d", mail.calls.Load())
	}
	assertLogsEqual(t, logs, []string{"checking URL", "request failed", "alert sent"})
}

func TestCheck_MixedURLsSendsAlertOnlyForFailed(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer fail.Close()

	mail := &mockMailService{}
	c, logs := newChecker(t, []string{ok.URL, fail.URL, ok.URL}, 5*time.Second, 0, mail)
	c.Check(context.Background())

	if mail.calls.Load() != 1 {
		t.Errorf("expected 1 alert call, got %d", mail.calls.Load())
	}
	assertLogsEqualUnordered(t, logs, []string{
		"checking URL", "check passed",
		"checking URL", "non-2xx response", "alert sent",
		"checking URL", "check passed",
	})
}

func TestCheck_AlertSendsThroughAllMailServices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	a := &mockMailService{}
	b := &mockMailService{}
	logger, logs := testutil.NewCaptureLogger()
	client := &http.Client{Timeout: 5 * time.Second}
	c := checker.New(client, []mail.MailService{a, b}, logger, []string{server.URL}, 0)
	c.Check(context.Background())

	if a.calls.Load() != 1 || b.calls.Load() != 1 {
		t.Errorf("expected each mail service to receive 1 Send, got a=%d b=%d", a.calls.Load(), b.calls.Load())
	}
	subsA := a.Subjects()
	subsB := b.Subjects()
	if len(subsA) != 1 || len(subsB) != 1 || subsA[0] != subsB[0] {
		t.Fatalf("both services should get the same subject; a=%q b=%q", subsA, subsB)
	}
	// Mail providers run concurrently; only the first two log lines are ordered.
	assertLogsEqualUnordered(t, logs, []string{"checking URL", "non-2xx response", "alert sent", "alert sent"})
}

// funcMail implements mail.MailService for tests that need custom Send behavior.
type funcMail struct {
	sendFn func(ctx context.Context, subject, body string) error
}

func (f funcMail) Send(ctx context.Context, subject, body string) error {
	return f.sendFn(ctx, subject, body)
}

func TestCheck_SlowMailServiceDoesNotBlockFasterMailService(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	slowEntered := make(chan struct{}, 1)
	stall := make(chan struct{})
	slow := funcMail{sendFn: func(ctx context.Context, _, _ string) error {
		slowEntered <- struct{}{}
		<-stall
		return nil
	}}
	fast := &mockMailService{}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := &http.Client{Timeout: 5 * time.Second}
	c := checker.New(client, []mail.MailService{slow, fast}, logger, []string{server.URL}, 0)

	done := make(chan struct{})
	go func() {
		c.Check(context.Background())
		close(done)
	}()

	select {
	case <-slowEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for slow mail service to enter Send")
	}

	waitForEachMailServiceCalls(t, 1, 2*time.Second, fast)

	close(stall)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Check to finish after unblocking slow sender")
	}
}

func TestCheck_OneMailServiceSendErrorDoesNotBlockOther(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	bad := &mockMailService{err: errors.New("SMTP failed")}
	good := &mockMailService{}
	logger, logs := testutil.NewCaptureLogger()
	client := &http.Client{Timeout: 5 * time.Second}
	c := checker.New(client, []mail.MailService{bad, good}, logger, []string{server.URL}, 0)
	c.Check(context.Background())

	if bad.calls.Load() != 1 || good.calls.Load() != 1 {
		t.Fatalf("expected both mail services to be tried, bad=%d good=%d", bad.calls.Load(), good.calls.Load())
	}
	assertLogsEqualUnordered(t, logs, []string{
		"checking URL", "non-2xx response", "failed to send alert", "alert sent",
	})
}

func TestCheck_OneMailServiceSendPanicDoesNotBlockOther(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	panicker := funcMail{sendFn: func(context.Context, string, string) error {
		panic("intentional mail panic")
	}}
	ok := &mockMailService{}
	logger, logs := testutil.NewCaptureLogger()
	client := &http.Client{Timeout: 5 * time.Second}
	c := checker.New(client, []mail.MailService{panicker, ok}, logger, []string{server.URL}, 0)
	c.Check(context.Background())

	if ok.calls.Load() != 1 {
		t.Fatalf("expected second mail service to run after first panicked, got %d calls", ok.calls.Load())
	}
	assertLogsEqualUnordered(t, logs, []string{
		"checking URL", "non-2xx response", "panic while sending alert mail", "alert sent",
	})
}

func TestCheck_BlockedMailForFailingURLDoesNotBlockHealthyURL(t *testing.T) {
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer badSrv.Close()

	goodSeen := make(chan struct{}, 1)
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case goodSeen <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer goodSrv.Close()

	stall := make(chan struct{})
	badURL := badSrv.URL
	mailer := funcMail{sendFn: func(_ context.Context, subject, _ string) error {
		if strings.Contains(subject, badURL) {
			<-stall
		}
		return nil
	}}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := &http.Client{Timeout: 5 * time.Second}
	c := checker.New(client, []mail.MailService{mailer}, logger, []string{badSrv.URL, goodSrv.URL}, 0)

	done := make(chan struct{})
	go func() {
		c.Check(context.Background())
		close(done)
	}()

	select {
	case <-goodSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for healthy URL check; it may have been blocked by failing URL mail path")
	}

	close(stall)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Check to finish")
	}
}

// Requirement: a Send error on one MailService must not prevent other providers from running.
func TestCheck_MultipleMailServiceSendErrorsStillInvokeLaterProviders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	a := &mockMailService{err: errors.New("provider a")}
	b := &mockMailService{err: errors.New("provider b")}
	c := &mockMailService{}
	logger, logs := testutil.NewCaptureLogger()
	client := &http.Client{Timeout: 5 * time.Second}
	chk := checker.New(client, []mail.MailService{a, b, c}, logger, []string{server.URL}, 0)
	chk.Check(context.Background())

	if a.calls.Load() != 1 || b.calls.Load() != 1 || c.calls.Load() != 1 {
		t.Fatalf("expected each of 3 mail services to be invoked once, got a=%d b=%d c=%d",
			a.calls.Load(), b.calls.Load(), c.calls.Load())
	}
	assertLogsEqualUnordered(t, logs, []string{
		"checking URL", "non-2xx response",
		"failed to send alert", "failed to send alert", "alert sent",
	})
}

// Requirement: Send error order must not matter; a failing provider after a successful one still means all ran.
func TestCheck_SecondMailServiceSendErrorDoesNotBlockEarlierSuccessfulSend(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	ok := &mockMailService{}
	bad := &mockMailService{err: errors.New("second fails")}
	logger, logs := testutil.NewCaptureLogger()
	client := &http.Client{Timeout: 5 * time.Second}
	chk := checker.New(client, []mail.MailService{ok, bad}, logger, []string{server.URL}, 0)
	chk.Check(context.Background())

	if ok.calls.Load() != 1 || bad.calls.Load() != 1 {
		t.Fatalf("expected both providers invoked, ok=%d bad=%d", ok.calls.Load(), bad.calls.Load())
	}
	assertLogsEqualUnordered(t, logs, []string{
		"checking URL", "non-2xx response", "alert sent", "failed to send alert",
	})
}

// Requirement: panic on a middle MailService must not skip other providers.
func TestCheck_MiddleMailServiceSendPanicDoesNotBlockOtherProviders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	first := &mockMailService{}
	middle := funcMail{sendFn: func(context.Context, string, string) error {
		panic("middle provider")
	}}
	last := &mockMailService{}
	logger, logs := testutil.NewCaptureLogger()
	client := &http.Client{Timeout: 5 * time.Second}
	chk := checker.New(client, []mail.MailService{first, middle, last}, logger, []string{server.URL}, 0)
	chk.Check(context.Background())

	if first.calls.Load() != 1 || last.calls.Load() != 1 {
		t.Fatalf("expected first and last mail after middle panic, first=%d last=%d", first.calls.Load(), last.calls.Load())
	}
	assertLogsEqualUnordered(t, logs, []string{
		"checking URL", "non-2xx response",
		"alert sent", "panic while sending alert mail", "alert sent",
	})
}

// Requirement: transport failure on one URL must not stop another URL from being checked the same round.
func TestCheck_ConnectionRefusedOnOneURLDoesNotBlockHealthyURLCheck(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	refusedURL := "http://" + l.Addr().String()
	l.Close()

	goodSeen := make(chan struct{}, 1)
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case goodSeen <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer goodSrv.Close()

	mailSvc := &mockMailService{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := &http.Client{Timeout: 5 * time.Second}
	chk := checker.New(client, []mail.MailService{mailSvc}, logger, []string{refusedURL, goodSrv.URL}, 0)

	done := make(chan struct{})
	go func() {
		chk.Check(context.Background())
		close(done)
	}()

	select {
	case <-goodSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("healthy URL was not checked while another URL had connection refused")
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Check to finish")
	}

	if mailSvc.calls.Load() != 1 {
		t.Fatalf("expected 1 alert for refused URL only, got %d", mailSvc.calls.Load())
	}
}

// Requirement: one URL timing out must not delay HTTP checks for other URLs (parallel per-round checks).
func TestCheck_OneURLHTTPTimeoutDoesNotDelayHealthyURLCheck(t *testing.T) {
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Long sleep so the client times out first; exit early when the client aborts so the test does not wait 2s.
		select {
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer slowSrv.Close()

	fastSeen := make(chan struct{}, 1)
	fastSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case fastSeen <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer fastSrv.Close()

	mailSvc := &mockMailService{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := &http.Client{Timeout: 80 * time.Millisecond}
	chk := checker.New(client, []mail.MailService{mailSvc}, logger, []string{slowSrv.URL, fastSrv.URL}, 0)

	done := make(chan struct{})
	go func() {
		chk.Check(context.Background())
		close(done)
	}()

	select {
	case <-fastSeen:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("fast URL was not checked promptly; URL checks may be serial instead of concurrent")
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Check to finish")
	}

	if mailSvc.calls.Load() != 1 {
		t.Fatalf("expected 1 alert for timed-out slow URL, got %d", mailSvc.calls.Load())
	}
}

func TestCheck_AlertEmailFailureDoesNotPanic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	mail := &mockMailService{err: errors.New("SMTP connection refused")}
	c, logs := newChecker(t, []string{server.URL}, 5*time.Second, 0, mail)
	c.Check(context.Background())

	if mail.calls.Load() != 1 {
		t.Errorf("expected 1 alert attempt, got %d", mail.calls.Load())
	}
	assertLogsEqual(t, logs, []string{"checking URL", "non-2xx response", "failed to send alert"})
}

func TestRunLoop_StopsOnContextCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	mail := &mockMailService{}
	c, logs := newChecker(t, []string{server.URL}, 5*time.Second, 0, mail)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.RunLoop(ctx, time.Hour)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop did not stop within 2 seconds after context cancel")
	}

	assertLogsEqual(t, logs, []string{
		"starting URL checker",
		"checking URL", "check passed",
		"URL checker stopped",
	})
}

func TestCheck_RecheckInterval_SecondAlertSuppressedWithinWindow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	mail := &mockMailService{}
	c, logs := newChecker(t, []string{server.URL}, 5*time.Second, time.Hour, mail)

	c.Check(context.Background())
	c.Check(context.Background())

	if mail.calls.Load() != 1 {
		t.Errorf("expected 1 alert (second suppressed), got %d", mail.calls.Load())
	}
	assertLogsEqual(t, logs, []string{
		"checking URL", "non-2xx response", "alert sent",
		"checking URL", "non-2xx response", "alert suppressed: recheck interval not elapsed",
	})
}

func TestCheck_RecheckInterval_AlertFiresAgainAfterWindowElapses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	mail := &mockMailService{}
	c, logs := newChecker(t, []string{server.URL}, 5*time.Second, 20*time.Millisecond, mail)

	c.Check(context.Background())
	time.Sleep(30 * time.Millisecond)
	c.Check(context.Background())

	if mail.calls.Load() != 2 {
		t.Errorf("expected 2 alerts (window elapsed between checks), got %d", mail.calls.Load())
	}
	assertLogsEqual(t, logs, []string{
		"checking URL", "non-2xx response", "alert sent",
		"checking URL", "non-2xx response", "alert sent",
	})
}

func TestCheck_RecheckInterval_RecoveryClearsTimestamp(t *testing.T) {
	// A passing check should reset throttle state before the window expires.
	var statusCode atomic.Int32
	statusCode.Store(int32(http.StatusInternalServerError))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(statusCode.Load()))
	}))
	defer server.Close()

	mail := &mockMailService{}
	c, logs := newChecker(t, []string{server.URL}, 5*time.Second, time.Hour, mail)

	c.Check(context.Background())
	statusCode.Store(int32(http.StatusOK))
	c.Check(context.Background())
	statusCode.Store(int32(http.StatusInternalServerError))
	c.Check(context.Background())

	if mail.calls.Load() != 2 {
		t.Errorf("expected 2 alerts (recovery cleared the recheck window), got %d", mail.calls.Load())
	}
	assertLogsEqual(t, logs, []string{
		"checking URL", "non-2xx response", "alert sent",
		"checking URL", "check passed",
		"checking URL", "non-2xx response", "alert sent",
	})
}

func TestCheck_RecheckInterval_DisabledWhenZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	mail := &mockMailService{}
	// recheckInterval 0 means every failed check mails.
	c, _ := newChecker(t, []string{server.URL}, 5*time.Second, 0, mail)

	c.Check(context.Background())
	c.Check(context.Background())
	c.Check(context.Background())

	if mail.calls.Load() != 3 {
		t.Errorf("expected 3 alerts when recheck interval is disabled, got %d", mail.calls.Load())
	}
}

func TestCheck_RecheckInterval_IndependentPerURL(t *testing.T) {
	// Each URL gets its own suppression clock.
	url1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer url1.Close()
	url2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer url2.Close()

	mail := &mockMailService{}
	c, _ := newChecker(t, []string{url1.URL, url2.URL}, 5*time.Second, time.Hour, mail)

	c.Check(context.Background())
	c.Check(context.Background())

	if mail.calls.Load() != 2 {
		t.Errorf("expected 2 alerts (one per URL, second round suppressed), got %d", mail.calls.Load())
	}
}
