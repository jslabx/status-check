# status-check

A lightweight Go service that periodically checks a list of URLs and sends an email when 
any endpoint returns a non-2xx status or a connection error is encountered.

Amazon SES is supported out of the box and additional mail services can be onboarded.

## How it works

On startup and every `interval_seconds`, `GET` requests are sent to check each URL asynchronously.

URLs are checked in parallel each round, so a slow endpoint, transport error, or stuck alert send for one URL does not stop the others from being checked in that same round.
Any status outside `2xx` or connection error (timeout, connection refused, etc.) triggers an alert email on every configured mail provider.

Errors and panics are recovered so that they cannot take down the entire runner.

## Quick start

```bash
# Copy and edit the config
cp config.example.yaml config.yaml

# Build
go build -o status-check ./cmd/

# Run (uses ./config.yaml in the current directory)
./status-check

# Run pointing at a specific file
./status-check ./config.yaml
```

## Configuration

Endpoints, intervals and email settings are defined in the configuration file.


```yaml
# URLs to watch. Alerts fire on non-2xx responses or any transport errors.
urls:
  - https://www.example.com
  - https://api.example.com

checker:
  # Per-request timeout in seconds.
  timeout_seconds: 10

  # Seconds between full rounds of checks.
  interval_seconds: 60

  # Minimum seconds between repeat alerts for the same URL.
  recheck_interval_seconds: 600

mail:
  ses:
    # Runs in dry mode if set to false.
    enabled: true

    from: alerts@example.com

    to:
      - oncall@example.com
      - backup@example.com

    # AWS_REGION / AWS_DEFAULT_REGION takes precedence over this.
    region: eu-west-2
```

With `mail.ses.enabled` set to `false`, alerts only show up in logs. Handy for
trying the tool out before you wire up SES.

### AWS credentials

Credentials are **never** stored in the config file. The service uses the
standard AWS credential chain, so any of the following work:

| Method | How |
|---|---|
| Environment variables | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, optionally `AWS_SESSION_TOKEN` |
| IAM role (EC2 / ECS / Lambda) | Role needs `ses:SendEmail`; no keys in the container if the platform injects the role |
| Profile files | `~/.aws/credentials` and `~/.aws/config`, with `AWS_PROFILE` set |

The `from` address or domain must be configured in SES before mail will deliver.

### AWS region resolution

Order of precedence:

1. `AWS_REGION`
2. `AWS_DEFAULT_REGION`
3. `mail.ses.region` in `config.yaml`

Env vars override the file, which helps when you ship one image to several
regions. On boot the service logs the region it picked and why:

```
{"level":"INFO","msg":"SES mail service enabled","region":"eu-west-2","region_source":"AWS_REGION env var","from":"alerts@example.com"}
```

## Running locally

```bash
go run ./cmd/
go run ./cmd/ /path/to/config.yaml
```

For SES, pass keys through the environment:

```bash
AWS_ACCESS_KEY_ID=AKIA... \
AWS_SECRET_ACCESS_KEY=... \
go run ./cmd/
```

## Development

The repo includes a [Dev Container](https://containers.dev/) under `.devcontainer/`
(Go toolchain, `go mod download` on create, optional forwarding of host AWS env vars
for SES). In VS Code, install the **Dev Containers** extension, open this folder, then
choose **Dev Containers: Reopen in Container**.

VS Code tasks live in `.vscode/tasks.json`. Open the command palette and run
**Tasks: Run Task** to use **Build** (default build task), **Run** (`go run ./cmd/`), or **Test**
(`go test ./... -v`).

## Onboarding new mail providers

- Add a service in `internal/mail/` that satisfies `MailService` (see `ses.go` for a working example). 
- Wire it into `cmd/main.go` by adding a builder function as appropriate and appending to `mailServices`.
- Extend `internal/config` to add config keys for the new service.

Builders in `main.go` should return `nil` if that provider is disabled or cannot start.
If no services start, `NoopMailService` is started and a warning in printed in the logs.

For each alert, `Send` runs on every service concurrently (`notify` waits for all to finish before returning). Errors and panics from one provider are isolated: they are logged and do not stop the others.

## Project structure

```
status-check/
├── cmd/
│   ├── main.go              # Entry point; wires dependencies
│   └── main_test.go         # Tests for config path CLI args
├── internal/
│   ├── checker/
│   │   ├── checker.go       # HTTP checks and alert logic
│   │   └── checker_test.go
│   ├── config/
│   │   ├── config.go        # YAML, defaults, validation
│   │   └── config_test.go
│   └── mail/
│       ├── mail.go          # MailService interface
│       ├── noop.go          # Logs only
│       ├── ses.go           # Amazon SES
│       └── ses_test.go
├── test/
│   └── integration_test.go  # Cross-layer tests
├── config.yaml              # Example config
└── Dockerfile
```

## Running the tests

```bash
# Unit + integration
go test ./...

# Verbose
go test ./... -v

# One package
go test ./internal/checker/...
```

Tests run entirely offline using mock HTTP handlers and mail service stubs.
