# Contributing

Contributions are welcome through GitHub issues and pull requests.

Before submitting a change:

1. Explain behavioral and battery-safety implications.
2. Add tests for changed decision or control paths.
3. Run `go vet ./...`, `go test -race ./...`, and `shellcheck scripts/*.sh`.
4. Keep credentials, household telemetry, local paths, and deployment details out of commits and fixtures.
5. Update the README or PRD when configuration, methodology, or limitations change.

For vulnerabilities or unsafe control behavior, use the private process in [SECURITY.md](SECURITY.md), not a public issue.
