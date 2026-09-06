# Energy Market Minitrader

An experimental Go service that schedules a Marstek Venus E home battery around NordPool day-ahead prices and can capture solar surplus measured by a HomeWizard P1 meter.

This project is for technically experienced owners of a compatible battery and ESPHome bridge who can validate the control entities, network isolation, tariffs, and fail-safe behavior on their own installation. It is not a general-purpose energy management system, a financial product, or safety-certified control software.

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

## Methodology

### Planning

The analyzer evaluates every contiguous charge window followed by every valid contiguous discharge window. Dynamic programming selects up to `MAX_CYCLES_PER_DAY` non-overlapping pairs that maximize summed expected profit over the loaded planning horizon. Despite the environment variable's historical name, this is a horizon-wide planning limit, not a persisted calendar-day execution counter.

A pair is eligible only when both conditions hold:

1. `discharge_average > charge_average / BATTERY_EFFICIENCY`
2. `discharge_average - charge_average >= MIN_PRICE_SPREAD`

The analyzer uses fixed windows sized from usable capacity and configured power. Execution is more adaptive: it computes the grid input needed to reach 100% from current SOC, reserves the cheapest remaining 15-minute slices before the next charge deadline, and lowers assumed delivery capacity when measured charging power tapers. It does not forecast future solar.

An active automatic cycle keeps its current plan while refreshed plans are staged. After a restart, the service can reconstruct a same-day discharge window from retained market prices, but it does not persist proof that the paired charge actually ran.

### Pricing And Accounting

NordPool EUR/MWh prices are converted to one all-in EUR/kWh rate:

```text
(wholesale price + energy tax) * (1 + VAT) + supplier fee
```

That same configured rate values import, discharge/export, and solar export opportunity cost. This assumes symmetric import/export value and does not model a separate feed-in tariff.

Energy is integrated from measured battery-power samples. P&L is cash flow, calculated as priced discharge value minus priced grid cost. It is not inventory-matched profit, does not deduct solar opportunity cost, and is incomplete when a session contains unpriced energy. The files and metrics are operational estimates, not revenue-grade metering.

### Solar Charging

Solar charging is enabled only when `HOMEWIZARD_P1_URL` is an explicit meter URL or `auto`.

- A session starts after 30 seconds of sustained raw surplus above `SOLAR_MIN_SURPLUS_W`.
- During charging, effective surplus compensates for the AC-coupled feedback loop: `measured surplus + measured battery charge power`.
- An elapsed-time EMA smooths the target, with a five-second settling period after power changes.
- Low surplus enters a 60-second, 75 W grace period before stopping.
- Adaptive cooldowns reduce short-session cycling.
- Battery-full protection, grid reservations, discharge windows, and repeated telemetry failure override solar charging.
- Solar is used ahead of reserved grid energy only when its forgone export value is no greater than the marginal reserved grid price.

See [the PRD](docs/energy-trader-prd.md) for the detailed state machine and accounting rules.

## Safety Model

The default ESPHome control path:

- reads a select before writing it and confirms changed select values from subsequent ESPHome publications;
- verifies signed measured battery power before declaring charge or discharge active;
- keeps stop intent pending until the stop is confirmed;
- retries failed stops without starting a conflicting action;
- checks telemetry staleness throughout active sessions;
- attempts to stop the battery during startup and graceful shutdown.

These measures do not create an independent fail-safe. The ESPHome backend has no battery-side command expiry: `PASSIVE_MODE_TIMEOUT_S` controls the service's refresh cadence but is not a hardware dead-man timer. If the process, host, network, or bridge fails while a forced command is active, the battery can continue until its own BMS limit or until control is restored. Use the software only after verifying the battery's built-in protections and provide independent supervision where required.

## Requirements

- Go 1.26.6 or newer, or Docker.
- A Marstek Venus E with a compatible ESPHome HTTP bridge on a trusted local network.
- ESPHome entities matching the names used in `clients/esphome/client.go`, including battery SOC/power sensors, forcible charge/discharge numbers, RS485 control mode, and forcible charge/discharge select.
- Network access to NordPool's data portal API.
- Optional HomeWizard P1 meter with its local API enabled.
- Optional Telegram bot and private chat.

The repository does not include a complete ESPHome firmware configuration. Confirm every entity and power-sign convention while the battery is attended and idle before enabling automatic operation.

## Setup

```bash
git clone https://github.com/foae/marstek-energy-trading.git
cd marstek-energy-trading
cp .env.example .env
```

Edit `.env` before starting. `ESPHOME_URL` is required and intentionally has no default. Review the tariff, power, capacity, minimum-SOC, timezone, and cycle settings for your installation.

Run locally:

```bash
make test
make run
```

Run with Docker:

```bash
make docker-build
make docker-run
```

`make docker-run` loads `.env`, persists `/app/data` in the `energy-trader-data` Docker volume, uses host networking for local-device discovery, and gives shutdown 95 seconds so the service can confirm a battery stop. Stop it with `docker stop energy-trader`; do not use `docker kill` during an active session.

If upgrading from a version that stored state in the repository's `./data` bind mount, migrate it once before the first `make docker-run`:

```bash
docker stop -t 95 energy-trader  # if the old container is running
make docker-build
make docker-migrate-data
```

Wait for the old container to stop cleanly before migrating so trade history and Telegram state cannot change during the copy. The migration copies `./data` into `energy-trader-data`, sets ownership for the image's non-root user, cleans a partial copy if an operation fails, and refuses to overwrite a non-empty destination volume. Keep the original directory as a backup until `/status` confirms the expected history.

## Configuration

| Variable | Default | Description |
|---|---:|---|
| `SERVICE_NAME` | `energy-trader` | Service name used in logs and notifications |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |
| `HTTP_LISTEN_ADDR` | `127.0.0.1:8080` | HTTP listen address; use `:8080` only behind a trusted network boundary |
| `DATA_DIR` | `./data` | Trade history and Telegram update-offset directory |
| `TZ` | `Europe/Amsterdam` | Scheduling and daily-accounting timezone |
| `NORDPOOL_AREA` | `NL` | NordPool bidding area |
| `NORDPOOL_CURRENCY` | `EUR` | Must remain EUR for all-in pricing |
| `ENERGY_TAX_EUR_PER_KWH` | `0.09161` | Example Dutch 2026 energy tax; verify before use |
| `VAT_RATE` | `0.21` | VAT applied to wholesale price and energy tax |
| `SUPPLIER_FEE_EUR_PER_KWH` | `0.02` | VAT-inclusive supplier fee |
| `MIN_PRICE_SPREAD` | `0.05` | Minimum raw average spread in EUR/kWh |
| `BATTERY_EFFICIENCY` | `0.90` | Round-trip efficiency in `(0, 1]` |
| `BATTERY_CAPACITY_KWH` | `5.12` | Nominal battery capacity |
| `BATTERY_MIN_SOC` | `0.11` | Minimum SOC fraction |
| `MAX_CYCLES_PER_DAY` | `2` | Maximum cycles selected over the loaded planning horizon |
| `ESPHOME_URL` | required | Absolute URL of the ESPHome bridge |
| `ESPHOME_RESTART_BUTTON` | empty | Optional restart-button name exposed by ESPHome |
| `CHARGE_POWER_W` | `2500` | Charge target, 75-2500 W |
| `DISCHARGE_POWER_W` | `2500` | Discharge target, 800-2500 W |
| `PASSIVE_MODE_TIMEOUT_S` | `300` | Refresh interval basis; not a hardware safety timeout |
| `HOMEWIZARD_P1_URL` | empty | Empty disables P1; URL selects a meter; `auto` opts into discovery and LAN scanning |
| `SOLAR_MIN_SURPLUS_W` | `100` | Sustained raw surplus needed to qualify solar charging |
| `TELEGRAM_BOT_TOKEN` | empty | Optional bot token |
| `TELEGRAM_CHAT_ID` | empty | Private chat authorized for notifications and commands |

See [.env.example](.env.example) for a copyable configuration.

## Usage

With the default loopback binding:

```bash
curl http://127.0.0.1:8080/health
curl http://127.0.0.1:8080/status
curl http://127.0.0.1:8080/metrics
```

| Endpoint | Content |
|---|---|
| `GET /health` | Process liveness (`ok`) |
| `GET /metrics` | Prometheus battery SOC, state, and cumulative P&L |
| `GET /status` | Cached battery state, current price, reservation, next action, and full trade history |

The API is read-only. It has no authentication or TLS.

### Telegram Commands

Commands are accepted only from the configured private `TELEGRAM_CHAT_ID`; group chats and other senders are ignored.

| Command | Behavior |
|---|---|
| `/status` | Show battery, price, trading state, and next action |
| `/discharge` | Start manual discharge at `DISCHARGE_POWER_W` |
| `/discharge 800` | Start manual discharge at a selected 800-2500 W |
| `/auto` | Stop manual discharge and return to automatic control |

Manual discharge stops at minimum SOC, on telemetry/control failure, or after two hours. Telegram is a convenience control path, not an independent safety channel.

### RS485 Diagnostics

If the ESPHome bridge exposes a restart button, set `ESPHOME_RESTART_BUTTON` to its web-server name, for example `Restart`. Verify its read endpoint while idle before allowing automatic restart. A manual restart-button POST may require `Content-Length: 0`.

The diagnostic script records ESPHome's `/events` stream and reconnects after bridge restarts:

```bash
LOGTAIL_TZ=Europe/Amsterdam scripts/esp32-logtail.sh \
  http://battery-bridge.local ./esp32-events.log
```

The script does not rotate its output. Use `logrotate` or another bounded-retention mechanism for unattended capture.

## Security And Privacy

- Keep ESPHome, HomeWizard, and this service on a trusted local network. Their local HTTP APIs are normally unauthenticated.
- The service HTTP API binds to loopback by default because `/status` and `/metrics` disclose household energy and financial data. If exposing it, use a firewall or authenticated reverse proxy and configure `HTTP_LISTEN_ADDR` explicitly.
- `HOMEWIZARD_P1_URL=auto` first uses mDNS and then actively probes `192.168.0.0/24` and `192.168.1.0/24`. Use an explicit URL on shared, VPN, or differently addressed networks.
- Keep `.env`, `data/`, diagnostic logs, and container build contexts private. The repository's `.gitignore` and `.dockerignore` exclude them by default.
- Trade history and Telegram offsets are stored locally with owner-only permissions. They can reveal occupancy and energy-use patterns.
- Telegram bot tokens are credentials. Rotate a token immediately if it appears in logs, shell history, an image cache, or a commit.
- Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md).

## Limitations

- No independent hardware watchdog or forced-command expiry is provided by the ESPHome backend.
- Active plans, active-session energy, and daily-summary delivery state are not persisted. A crash can lose partial-session accounting or reconstruct a discharge without proof that its paired charge ran.
- The ESPHome bridge configuration and firmware compatibility matrix are not included.
- Solar power-number writes use ESPHome's optimistic number state; measured power is observed on subsequent ticks rather than transactionally confirmed for every adjustment.
- A single serialized control loop performs battery, meter, price, and Telegram I/O. Slow network calls can delay other checks.
- Only one battery, one P1 meter, EUR pricing, and 15-minute NordPool products are supported.
- Default tax, VAT, fee, area, timezone, battery capacity, and efficiency values are examples for one Dutch setup and will become stale.
- Import and export are valued symmetrically; installations with a separate feed-in tariff need code changes.
- P&L and energy are estimates, not billing, tax, warranty, or investment records.
- Runtime history grows without retention and is rewritten atomically after every completed trade.
- The legacy UDP package is unauthenticated, spoofable on an untrusted LAN, and not wired into the service.

## Development

```bash
make build
make test
make test-one TEST=TestName
go vet ./...
shellcheck scripts/*.sh
```

Project layout:

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

See [CONTRIBUTING.md](CONTRIBUTING.md) before submitting changes. The project is available under the [MIT License](LICENSE).
