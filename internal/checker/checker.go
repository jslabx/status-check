package checker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"status-check/internal/mail"
)

// URLChecker hits each configured URL and asks each configured MailService to send mail when a check fails.
type URLChecker struct {
	httpClient      *http.Client
	mailServices    []mail.MailService
	logger          *slog.Logger
	urls            []string
	recheckInterval time.Duration

	// lastAlertAt is per-URL throttle state when recheckInterval > 0 (mu guards it).
	mu          sync.Mutex
	lastAlertAt map[string]time.Time
}

func New(httpClient *http.Client, mailServices []mail.MailService, logger *slog.Logger, urls []string, recheckInterval time.Duration) *URLChecker {
	return &URLChecker{
		httpClient:      httpClient,
		mailServices:    mailServices,
		logger:          logger,
		urls:            urls,
		recheckInterval: recheckInterval,
		lastAlertAt:     make(map[string]time.Time),
	}
}

// Check runs a single round of checks against all configured URLs concurrently.
// Each URL is checked in its own goroutine; a slow check, transport error, or blocked
// alert path for one URL does not stop the others from running in parallel.
// Check blocks until every URL goroutine in this round has finished.
func (c *URLChecker) Check(ctx context.Context) {
	var wg sync.WaitGroup
	for _, url := range c.urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			c.checkURL(ctx, u)
		}(url)
	}
	wg.Wait()
}

// RunLoop checks immediately, then every interval, until ctx ends.
func (c *URLChecker) RunLoop(ctx context.Context, interval time.Duration) {
	c.logger.Info("starting URL checker", "url_count", len(c.urls), "interval", interval)
	c.Check(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.logger.Info("URL checker stopped")
			return
		case <-ticker.C:
			c.Check(ctx)
		}
	}
}

func (c *URLChecker) checkURL(ctx context.Context, url string) {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("panic while checking URL", "url", url, "panic", r)
			c.notify(ctx, url, fmt.Sprintf("Unexpected panic while checking %s: %v", url, r))
		}
	}()

	c.logger.Info("checking URL", "url", url)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		c.logger.Error("failed to build request", "url", url, "error", err)
		c.notify(ctx, url, fmt.Sprintf("Failed to build request for %s: %v", url, err))
		return
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.Error("request failed", "url", url, "error", err)
		c.notify(ctx, url, fmt.Sprintf("Request to %s failed: %v", url, err))
		return
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.logger.Warn("non-2xx response", "url", url, "status_code", resp.StatusCode)
		c.notify(ctx, url, fmt.Sprintf("%s returned HTTP %d", url, resp.StatusCode))
		return
	}

	c.logger.Info("check passed", "url", url, "status_code", resp.StatusCode)
	// Success clears any per-URL alert throttle so the next failure is not
	// held back by an old recheck window.
	c.clearLastAlert(url)
}

func (c *URLChecker) notify(ctx context.Context, url, message string) {
	if c.recheckInterval > 0 {
		c.mu.Lock()
		lastAlert, alerted := c.lastAlertAt[url]
		if alerted && time.Since(lastAlert) < c.recheckInterval {
			remaining := c.recheckInterval - time.Since(lastAlert)
			c.mu.Unlock()
			c.logger.Info("alert suppressed: recheck interval not elapsed",
				"url", url, "next_alert_in", remaining.Round(time.Second))
			return
		}
		c.lastAlertAt[url] = time.Now()
		c.mu.Unlock()
	}

	subject := fmt.Sprintf("Status Check Alert: %s", url)
	var wg sync.WaitGroup
	for _, svc := range c.mailServices {
		wg.Add(1)
		go func(svc mail.MailService) {
			defer wg.Done()
			mailSvcName := fmt.Sprintf("%T", svc)
			defer func() {
				if r := recover(); r != nil {
					c.logger.Error("panic while sending alert mail", "url", url, "mail_service", mailSvcName, "panic", r)
				}
			}()
			if err := svc.Send(ctx, subject, message); err != nil {
				c.logger.Error("failed to send alert", "url", url, "mail_service", mailSvcName, "error", err)
				return
			}
			c.logger.Info("alert sent", "url", url, "mail_service", mailSvcName, "to", svc.AlertRecipients())
		}(svc)
	}
	wg.Wait()
}

func (c *URLChecker) clearLastAlert(url string) {
	if c.recheckInterval > 0 {
		c.mu.Lock()
		delete(c.lastAlertAt, url)
		c.mu.Unlock()
	}
}
