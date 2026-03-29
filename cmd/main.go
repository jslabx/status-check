package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ses"

	"status-check/internal/checker"
	"status-check/internal/config"
	"status-check/internal/mail"
)

func main() {
	var logLevel slog.LevelVar
	logLevel.Set(slog.LevelInfo)
	if s := os.Getenv("LOG_LEVEL"); s != "" {
		var lvl slog.Level
		if err := lvl.UnmarshalText([]byte(s)); err == nil {
			logLevel.Set(lvl)
		}
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:     &logLevel,
		AddSource: false,
	}))

	configPath, err := configPathFromArgs(os.Args)
	if err != nil {
		logger.Error("invalid arguments", "error", err)
		os.Exit(2)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	logger.Info("URLs to check", "count", len(cfg.URLs), "urls", cfg.URLs)

	// Onboard new services by appending to this list.
	mailServices := []mail.MailService{}

	if sesMailService := buildSesMailService(cfg, logger); sesMailService != nil {
		mailServices = append(mailServices, sesMailService)
	}
	
	if len(mailServices) == 0 {
		logger.Warn("no mail service enabled, alerts will only be logged")
		mailServices = []mail.MailService{mail.NewNoopMailService(logger)}
	}

	httpClient := &http.Client{
		Timeout: time.Duration(cfg.Checker.TimeoutSeconds) * time.Second,
	}

	recheckInterval := time.Duration(cfg.Checker.RecheckIntervalSeconds) * time.Second
	urlChecker := checker.New(httpClient, mailServices, logger, cfg.URLs, recheckInterval)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	interval := time.Duration(cfg.Checker.IntervalSeconds) * time.Second
	urlChecker.RunLoop(ctx, interval)

	logger.Info("shutdown complete")
}

func configPathFromArgs(args []string) (string, error) {
	switch len(args) {
	case 1:
		return "config.yaml", nil
	case 2:
		return args[1], nil
	default:
		prog := filepath.Base(args[0])
		return "", fmt.Errorf("usage: %s [config-path]", prog)
	}
}

// buildSesMailService returns an SES-backed MailService when mail.ses.enabled is true and AWS
// configuration loads successfully; otherwise nil (caller combines with other providers or noop).
func buildSesMailService(cfg *config.Config, logger *slog.Logger) mail.MailService {
	if !cfg.Mail.SES.Enabled {
		return nil
	}

	region, regionSource := resolveRegion(cfg.Mail.SES.Region)

	awsCfg, err := awsconfig.LoadDefaultConfig(
		context.Background(),
		awsconfig.WithRegion(region),
	)
	if err != nil {
		logger.Error("failed to load AWS config, SES mail service skipped", "error", err)
		return nil
	}

	sesClient := ses.NewFromConfig(awsCfg)
	logger.Info("SES mail service enabled",
		"region", region,
		"region_source", regionSource,
		"from", cfg.Mail.SES.From,
		"to", cfg.Mail.SES.To,
	)
	probeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return mail.NewSesMailService(probeCtx, logger, sesClient, cfg.Mail.SES.From, cfg.Mail.SES.To)
}

// resolveRegion picks the AWS region and a short label for logs.
// Order: AWS_REGION, then AWS_DEFAULT_REGION, then config file.
func resolveRegion(configRegion string) (region string, source string) {
	if r := os.Getenv("AWS_REGION"); r != "" {
		return r, "AWS_REGION env var"
	}
	if r := os.Getenv("AWS_DEFAULT_REGION"); r != "" {
		return r, "AWS_DEFAULT_REGION env var"
	}
	return configRegion, "config file"
}
