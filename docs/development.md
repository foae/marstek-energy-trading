# Development

## Commands

```bash
make build
make test
make test-one TEST=TestName
go vet ./...
shellcheck scripts/*.sh
```

See the `Makefile` for the full target list, including Docker build, run, and data-migration targets.

## Project Layout

```text
cmd/trader/                 Entry point and dependency wiring
internal/config/            Environment parsing and validation
clients/esphome/            Default battery-control backend
clients/homewizard/         P1 client and opt-in discovery
clients/marstek/            Preserved, unwired UDP client
clients/nordpool/           Day-ahead price client and all-in pricing
clients/telegram/           Notifications and private-chat commands
service/                    Planning, reservations, control, and recording
handler/                    Read-only HTTP health, metrics, and status
scripts/                    ESPHome diagnostic tooling
docs/                       Detailed methodology and legacy notes
```

## Contributing

See [CONTRIBUTING.md](../CONTRIBUTING.md) before submitting changes. The project is available under the [MIT License](../LICENSE).
