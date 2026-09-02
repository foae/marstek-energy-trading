# Energy Market Minitrader

A Go service that performs energy price arbitrage using a Marstek Venus E battery. The service fetches NordPool day-ahead prices, identifies optimal charge/discharge windows, and controls the battery via an ESPHome REST API (with legacy UDP support available).

## Trading Strategy

The service analyzes NordPool 15-minute resolution prices to find:
- **Charge windows**: Time slots in the bottom 25% of the daily price range
- **Discharge windows**: Time slots in the top 25% of the daily price range

A trade is only executed when:
1. Price spread exceeds the configured minimum (`MIN_PRICE_SPREAD`)
2. Discharge price > charge price / efficiency (accounts for 10% energy loss)

Typical daily pattern: overnight cheap (charge) → morning peak (discharge) → afternoon dip (charge) → evening peak (discharge).

## Components

### Marstek Venus E Battery
Product website: https://www.marstek.nl/product/plug-and-charge-thuisbatterij-5-12-kwh/

| Spec | Value |
|------|-------|
| Capacity | 5.12 kWh |
| Min SOC protection | 11% |
| Round-trip efficiency | 90% |
| Max charge | 2500 W |
| Discharge range | 800-2500 W |
| Control API | ESPHome REST (default) or UDP |

### ESPHome Integration (Default)
The service uses an ESPHome device as a bridge to control the battery via HTTP REST API. This provides more reliable communication than the native UDP protocol.

- **Default endpoint**: `http://192.168.1.50`
- **Protocol**: HTTP REST with JSON responses

#### RS485 link freeze recovery
The bridge's Modbus/RS485 link to the battery can freeze mid-session: telemetry keeps returning the last values, control writes are silently dropped, and the battery keeps charging or discharging on the last accepted command.

The service handles this by:
- detecting frozen telemetry within ~2 minutes while a session is active, and on any control failure
- alerting via Telegram immediately, on a dedicated rate limiter so the alert is never swallowed by an unrelated error
- refreshing running commands by read-back only (no Modbus write unless the battery reports another mode/power)
- rebooting the bridge automatically when the ESPHome config exposes a restart button (`ESPHOME_RESTART_BUTTON`, at most once per 10 minutes). This device's web server keys entities by display name (`/button/Restart/press`), not by snake_case object id; the client tries both spellings, so either value works.

```yaml
button:
  - platform: restart
    name: "Restart"   # -> ESPHOME_RESTART_BUTTON=Restart
```

To capture the bridge's own debug log around the next freeze (the ESPHome web server streams it on `/events`), run `scripts/esp32-logtail.sh` on a machine that stays up, e.g. `nohup scripts/esp32-logtail.sh http://192.168.1.50 ~/esp32-events.log >/dev/null 2>&1 &`. It timestamps every log line in the trader's timezone, drops the routine Modbus chatter, and reconnects when the device reboots.

### Legacy UDP API (Optional)
Direct UDP control is available but not enabled by default. See [docs/marstek-api.md](docs/marstek-api.md) for protocol details.

### NordPool API
- **Endpoint**: `https://dataportal-api.nordpoolgroup.com/api/DayAheadPriceIndices`
- **Resolution**: 15-minute intervals
- **Prices**: EUR/MWh, converted internally to all-in EUR/kWh using `(NordPool + energy tax) × (1 + VAT) + supplier fee`

### HomeWizard P1 Energy Meter (planned)
Provides real-time house energy consumption. Future enhancement to pause charging when consumption exceeds ~17kWh total or ~5.7kWh per phase.

## Quick Start

```bash
# Clone and configure
cp .env.example .env
# Edit .env with your settings

# Build and run
make build
make run

# Or with Docker
make docker-build
docker run -d --net=host energy-trader
```

## Configuration

Copy `.env.example` to `.env`. Key settings:

| Variable | Default | Description |
|----------|---------|-------------|
| `NORDPOOL_CURRENCY` | `EUR` | Required currency for all-in pricing |
| `ENERGY_TAX_EUR_PER_KWH` | `0.09161` | Dutch 2026 energy tax for the first 10,000 kWh |
| `VAT_RATE` | `0.21` | VAT applied to NordPool price and energy tax |
| `SUPPLIER_FEE_EUR_PER_KWH` | `0.02` | Contract-specific per-kWh supplier fee, VAT-inclusive |
| `MIN_PRICE_SPREAD` | `0.05` | Minimum EUR/kWh spread to trigger trading |
| `BATTERY_EFFICIENCY` | `0.90` | Round-trip efficiency (0.0-1.0) |
| `ESPHOME_URL` | `http://192.168.1.50` | ESPHome device URL |
| `ESPHOME_RESTART_BUTTON` | - | Optional: ESPHome restart button name as used in the device web URLs (e.g. `Restart`), used to auto-recover a frozen RS485 link |
| `CHARGE_POWER_W` | `2500` | Charge power in watts |
| `DISCHARGE_POWER_W` | `2500` | Discharge power in watts |
| `TELEGRAM_BOT_TOKEN` | - | Optional: Telegram notifications |
| `TELEGRAM_CHAT_ID` | - | Optional: Telegram chat ID |

See `.env.example` for all options.

## Telegram Commands

Commands are accepted only from the configured private `TELEGRAM_CHAT_ID`; group chats are ignored for battery-control safety. At startup, the service publishes the supported commands to that chat's Telegram command menu.

| Command | Behavior |
|---------|----------|
| `/status` | Show battery, price, trading state, and next action |
| `/discharge` | Start manual discharge at `DISCHARGE_POWER_W` |
| `/discharge 800` | Start manual discharge at a chosen power from 800-2500 W |
| `/auto` | Stop the manual discharge and return control to automatic trading and solar charging |

Manual discharge stops automatically at the configured minimum SOC, when the battery API cannot be read or refreshed, or after two hours. Completed manual discharges are included in trade history and P&L.

## HTTP Endpoints

| Endpoint | Description |
|----------|-------------|
| `GET /health` | Liveness check |
| `GET /metrics` | Prometheus metrics |
| `GET /status` | Current state (SOC, price, next action) and trade history (JSON) |

## Logging

The service uses structured JSON logging via `slog`. Key log entries:
- **Info**: State changes (charging started/stopped, prices fetched, trading windows)
- **Debug**: Routine checks (battery status, passive mode refresh)
- **Warn**: Recoverable errors (API failures, notification failures)
- **Error**: Critical failures (battery unreachable)

To follow trading decisions: `docker logs -f <container> | jq 'select(.msg | startswith("decision"))'`

## Limitations

- **Network requirements**: The service must be able to reach the ESPHome device over HTTP.

## Project Structure

```
cmd/trader/main.go       # Entry point
internal/config/         # Configuration (env parsing via caarlos0/env)
clients/
  esphome/               # ESPHome HTTP client (default)
  marstek/               # Battery UDP client (legacy, preserved)
  nordpool/              # NordPool API client
  telegram/              # Telegram bot notifications
service/
  service.go             # Trading engine + main loop
  analyzer.go            # Price analysis + window detection
  recorder.go            # Trade/P&L recording (JSON files)
  interfaces.go          # Interfaces for testing
handler/                 # HTTP endpoints
data/                    # Runtime data (trades.json) - gitignored
```

## Development

```bash
make build              # Build binary
make run                # Run locally
make test               # Run all tests
make test-one TEST=TestName  # Run single test
make docker-build       # Build Docker image
```
