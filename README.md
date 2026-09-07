# Energy Market Minitrader

An experimental Go service that schedules a Marstek Venus E home battery around NordPool day-ahead prices and can capture solar surplus measured by a HomeWizard P1 meter.

This project is for technically experienced owners of a compatible battery and ESPHome bridge who can validate the control entities, network isolation, tariffs, and fail-safe behavior on their own installation. It is not a general-purpose energy management system, a financial product, or safety-certified control software. Read [Safety](docs/safety.md) before enabling automatic operation.

## Capabilities

- Fetches 15-minute NordPool day-ahead prices for a configured bidding area.
- Builds globally optimized, chronological charge-to-discharge plans over the known today/tomorrow horizon.
- Recalculates grid charging from measured state of charge and reserves the cheapest remaining slots before the next charge deadline.
- Controls one Marstek Venus E through an ESPHome HTTP bridge, with read-back confirmation and measured-power start verification.
- Captures solar surplus from an optional HomeWizard P1 meter while giving scheduled grid reservations and discharge windows priority.
- Records measured energy, priced and unpriced energy, estimated grid-attributable solar-session input, opportunity cost, and cash-flow P&L in local JSON.
- Exposes health, Prometheus metrics, and status/history endpoints.
- Sends optional Telegram notifications and accepts authorized private-chat status/manual-discharge commands.
- Detects a frozen ESPHome-to-battery RS485 link during active sessions and can restart the bridge when a restart button is configured.

The preserved `clients/marstek` UDP package is not wired into the executable. See [Legacy UDP Client](docs/legacy-udp.md).

## Economics

A charge/discharge pair is eligible only when its expected profit per input kWh is positive and at least `MIN_PRICE_SPREAD`:

```text
discharge_average * BATTERY_EFFICIENCY - charge_average
```

Every price uses one configured all-in EUR/kWh rate:

```text
(wholesale price + energy tax) * (1 + VAT) + supplier fee
```

Import, export, and solar opportunity cost are valued symmetrically with that rate; a separate feed-in tariff is not modeled. Planning, accounting, and solar-charging rules are detailed in [Methodology](docs/methodology.md), and the full state machine and accounting rules are in [the PRD](docs/energy-trader-prd.md).

## Quick Start

```bash
git clone https://github.com/foae/marstek-energy-trading.git
cd marstek-energy-trading
cp .env.example .env
```

Edit `.env` before starting. `ESPHOME_URL` is required and intentionally has no default. Review the tariff, power, capacity, minimum-SOC, timezone, and cycle settings for your installation; see [Configuration](docs/configuration.md).

```bash
make test
make run
```

Or with Docker:

```bash
make docker-build
make docker-run
```

`make docker-run` loads `.env`, persists `/app/data` in the `energy-trader-data` Docker volume, and gives shutdown 95 seconds so the service can confirm a battery stop. Requirements, migration from older versions, and operational details are in [Operations](docs/operations.md).

## Commands and Endpoints

| Command | Purpose |
|---|---|
| `make build` | Build the binary |
| `make test` | Run all tests |
| `make test-one TEST=TestName` | Run a single test |
| `make docker-build` | Build the Docker image |
| `make docker-run` | Run the Docker image |

The HTTP API binds to loopback by default and is read-only, without authentication or TLS:

| Endpoint | Content |
|---|---|
| `GET /health` | Process liveness (`ok`) |
| `GET /metrics` | Prometheus battery SOC, state, and cumulative P&L |
| `GET /status` | Cached battery state, current price, reservation, commitment/pending-plan state, next action, and full trade history |

## Documentation

| Document | Content |
|---|---|
| [Configuration](docs/configuration.md) | Full settings reference and pricing caveat |
| [Methodology](docs/methodology.md) | Planning, pricing and accounting, solar charging |
| [Operations](docs/operations.md) | Requirements, setup, Docker and data migration, endpoints, Telegram, RS485 diagnostics |
| [Safety](docs/safety.md) | Control safety model, security and privacy, limitations |
| [Development](docs/development.md) | Build commands, project layout, contributing |
| [Product Requirements](docs/energy-trader-prd.md) | Detailed state machine and accounting rules |
| [Legacy UDP Client](docs/legacy-udp.md) | Preserved, unwired Marstek UDP client |

See [SECURITY.md](SECURITY.md) for vulnerability reporting and [CONTRIBUTING.md](CONTRIBUTING.md) before submitting changes. The project is available under the [MIT License](LICENSE).
