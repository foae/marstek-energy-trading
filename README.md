# Energy Market Minitrader

An experimental Go service that schedules a Marstek Venus E home battery around NordPool day-ahead prices and captures qualified solar surplus measured by a HomeWizard P1 meter.

This project is for technically experienced owners of a compatible battery and ESPHome bridge who can validate the control entities, network isolation, tariffs, and fail-safe behavior on their own installation. It is not a general-purpose energy management system, a financial product, or safety-certified control software. Read [Safety](docs/safety.md) before enabling automatic operation.

## Capabilities

- Fetches 15-minute NordPool day-ahead prices for a configured bidding area.
- Selects a joint total-EUR plan from observed stored energy, optional following grid cycles, and holding energy for later known prices.
- Sells uncommitted observed inventory only in its best contiguous positive-export-price window in the known tariff horizon; it is not an arbitrary-slot or future-price guarantee.
- Recalculates grid charging from measured state of charge and reserves the cheapest remaining slots before the next charge deadline.
- Controls one Marstek Venus E through an ESPHome HTTP bridge, with read-back confirmation and measured-power start verification.
- Captures qualified solar surplus without a tariff veto, except for safety/fault handling, manual control, selected or active discharge, and a grid reservation.
- Records measured energy, scheduled-charge source attribution, cash flow, solar opportunity cost, and a separate opportunity-cost-adjusted metric in local JSON.
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

Export and solar opportunity cost are valued at the configured export tariff (`EXPORT_PRICE_MODE`), symmetric with the import rate by default and optionally wholesale-based. Planning, accounting, and solar-charging rules are detailed in [Methodology](docs/methodology.md), and the full state machine and accounting rules are in [the PRD](docs/energy-trader-prd.md).

### Stored energy and solar

The planner uses fresh observed stored DC energy exactly once when comparing total EUR: it can sell that inventory in one contiguous future window, let it reduce the first grid purchase, or hold it. An inventory sale is exempt from the grid-cycle allowance; later grid cycles still obey `MAX_CYCLES_PER_DAY` and their existing per-slice minimum-profit rule. It uses only the known tariff horizon—there is no solar forecast, invented future price, arbitrary battery-wear floor, or guarantee that the chosen sale is hindsight-optimal.

Qualified solar capture is unconditional outside safety/fault handling, manual override, automatic-discharge priority, and a current grid reservation. Solar stops at 99% SOC and can qualify again at 97%; a grid reservation may charge to 100%. Solar control retains AC-side surplus compensation, anti-cycling, power limits, and telemetry protections. A commanded AC discharge may offset household load before reaching the meter, so it is not a promise of metered export revenue.

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
