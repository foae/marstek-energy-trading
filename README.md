# Energy Market Minitrader

An experimental Go service that schedules a Marstek Venus E home battery around NordPool day-ahead prices and captures qualified solar surplus measured by a HomeWizard P1 meter.

This project is for technically experienced owners of a compatible battery and ESPHome bridge who can validate the control entities, network isolation, tariffs, and fail-safe behavior on their own installation. It is not a general-purpose energy management system, a financial product, or safety-certified control software. Read [Safety](docs/safety.md) before enabling automatic operation.

## Capabilities

- Fetches 15-minute NordPool day-ahead prices for a configured bidding area.
- Compares holding observed stored energy, selling it, or using it to reduce a grid purchase in a joint total-EUR plan.
- Can sell part or all of uncommitted inventory in one contiguous positive-export-price window, including a partial tariff slot, followed by profitable grid cycles.
- Recalculates grid charging from measured state of charge and reserves eligible slices as late as the price tolerance allows before the next charge deadline, so solar can fill the battery first; these slices need not be contiguous. Deferral requires a configured P1 meter; without one the cheapest slices are reserved as before.
- Controls one Marstek Venus E through an ESPHome HTTP bridge, with read-back confirmation and measured-power start verification.
- Captures qualified solar surplus without a tariff veto, except for safety/fault handling, manual control, selected or active discharge, and a grid reservation.
- Records measured energy, scheduled-charge source attribution, cash flow, solar opportunity cost, and a separate opportunity-cost-adjusted metric in local JSON.
- Sends optional Telegram notifications and accepts authorized private-chat status/manual-discharge commands.
- Detects a frozen ESPHome-to-battery RS485 link during active sessions, then keeps the bus quiet: it stops writing except for one throttled stop retry every five minutes, and restarts the bridge only after five minutes down and at most once per hour, when a restart button is configured.

The preserved `clients/marstek` UDP package is not wired into the executable. See [Legacy UDP Client](docs/legacy-udp.md).

## Economics

A grid charge/discharge pair is eligible only when its expected profit per input kWh is positive and at least `MIN_PRICE_SPREAD`:

```text
average export rate * BATTERY_EFFICIENCY - average import rate
```

Grid import uses the configured all-in EUR/kWh rate:

```text
(wholesale price + energy tax) * (1 + VAT) + supplier fee
```

Export and forgone solar export use `EXPORT_PRICE_MODE`: `symmetric` uses the import rate; `wholesale` uses wholesale price plus the signed `EXPORT_FEE_EUR_PER_KWH`, without energy tax or VAT. Check these settings against your electricity contract rather than assuming the defaults match it.

`BATTERY_EFFICIENCY` is the round-trip planning assumption. `BATTERY_CHARGE_EFFICIENCY` describes AC input converted to stored energy; the discharge-side efficiency is their ratio. Grid reservations divide the observed energy shortfall by charge-side efficiency, not round-trip efficiency. Measured AC efficiency is reported separately and does not automatically change these settings.

### Stored energy and solar

The planner uses fresh observed stored DC energy above minimum SOC exactly once when comparing total EUR: it can sell that inventory in one contiguous future window, let it reduce the first grid purchase, or hold it. An inventory-only sale needs a known positive export rate, but is exempt from `MIN_PRICE_SPREAD` and the grid-cycle allowance. `MAX_CYCLES_PER_DAY` limits new grid cycles over the loaded planning horizon, not a persisted calendar-day count.

Sale timing and quantity are selected together with later grid cycles, rather than maximizing the sale alone. The planner uses only known tariffs: there is no solar forecast, invented future price, arbitrary battery-wear floor, or guarantee of arbitrary-slot or hindsight-optimal dispatch.

Qualified solar capture has no tariff veto: missing, negative, or expensive tariffs alone do not prevent it. Safety/fault handling, manual control, automatic-discharge priority, and a current grid reservation still take precedence. Qualification requires sustained surplus; AC-side feedback compensation, smoothing, cooldowns, and telemetry protections remain active. Solar stops at 99% SOC and can qualify again at 97%; grid reservations target 100%, but taper or insufficient eligible charging time can make that target infeasible.

### Accounting and restart behavior

Energy accounting uses measured AC power. While the RS485 link is detected down during a scheduled charge or discharge, that time books zero energy rather than integrating the bridge's last cached reading, and the affected seconds are recorded on the trade as `telemetry_gap_s`. With P1 observations, scheduled charging distinguishes grid input from solar input; energy before the first usable source observation is explicitly unattributed. Cash-flow P&L is priced discharge value minus priced grid cost. The separate opportunity-adjusted figure subtracts signed forgone solar-export value. Neither is inventory-matched profit or a bill: discharge may offset household load rather than reach the meter, and missing prices or source attribution leave estimates incomplete.

Grid-cycle intent is persisted before charging; completed discharge windows are persisted to prevent their reuse after restart. Uncommitted inventory plans are rebuilt from fresh SOC and known tariffs. Persistence errors can block automatic control, and partial active-session energy is not crash-durable. Back up `DATA_DIR` and follow [recovery guidance](docs/operations.md#commitment-recovery) rather than deleting state to unblock trading.

See [Methodology](docs/methodology.md) for planning and accounting details and [Safety](docs/safety.md) for control limitations. ESPHome forced commands have no independent expiry; stopping the process is not a substitute for confirming the battery stopped.

## Quick Start

You need Go 1.26.6 or newer and Make, or Docker and Make; a compatible ESPHome bridge with the required battery sensors and **AC Power** sensor; and access to NordPool. A HomeWizard P1 meter is optional but required for surplus capture. The repository does not supply a complete ESPHome firmware configuration—verify entities and power signs while the battery is attended and idle.

```bash
git clone https://github.com/foae/marstek-energy-trading.git
cd marstek-energy-trading
cp .env.example .env
```

Edit `.env` before starting. `ESPHOME_URL` is required and intentionally has no default. Review import/export tariffs, both efficiency settings, power, capacity, minimum SOC, timezone, and cycle allowance against [Configuration](docs/configuration.md). Set `HOMEWIZARD_P1_URL` to the meter URL to enable solar capture; empty disables it, while `auto` opts into mDNS discovery and a LAN scan.

The run commands below enable real battery control; they are not a simulation. Do not run a second controller against the same battery.

```bash
make test
make run
```

Or with Docker:

```bash
make docker-build
make docker-run
```

`make docker-run` uses host networking, loads `.env`, persists `/app/data` in the `energy-trader-data` Docker volume, and gives shutdown 95 seconds so the service can confirm a battery stop. Use `docker stop energy-trader`, not `docker kill`, during a session. For a local run, use Ctrl-C and allow graceful shutdown. Follow [Operations](docs/operations.md) for existing-data migration and platform/network requirements.

After startup, inspect `/status` and logs for fresh battery observations, the selected plan, and any blocked-control reason. `/health` returning `ok` proves only process liveness, not battery connectivity or safe operation.

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
| `GET /metrics` | Prometheus SOC/state, measured efficiency, known cash flow, opportunity-adjusted P&L, and unpriced/unattributed energy |
| `GET /status` | Cached battery observations, measured efficiency, price availability, reservation, commitment/pending-plan state, next action, and full trade history |

The [metrics reference](docs/operations.md#accounting-metrics) explains the accounting completeness indicators; do not interpret a known-value P&L gauge alone as total profit.

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
