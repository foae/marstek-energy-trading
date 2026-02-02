# Energy Market Minitrader

A Go service that performs energy price arbitrage using a Marstek Venus E battery. The service fetches NordPool day-ahead prices, identifies optimal charge/discharge windows, and controls the battery via its local UDP API.

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
| Local API | UDP to `192.168.1.255:30000` |

API documentation: [docs/marstek-api.md](docs/marstek-api.md) | [Original PDF](https://static-eu.marstekenergy.com/ems/resource/agreement/MarstekDeviceOpenApi.pdf)

### NordPool API
- **Endpoint**: `https://dataportal-api.nordpoolgroup.com/api/DayAheadPriceIndices`
- **Resolution**: 15-minute intervals
- **Prices**: EUR/MWh (converted to EUR/kWh internally)

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
| `MIN_PRICE_SPREAD` | `0.05` | Minimum EUR/kWh spread to trigger trading |
| `BATTERY_EFFICIENCY` | `0.90` | Round-trip efficiency (0.0-1.0) |
| `BATTERY_UDP_ADDR` | `192.168.1.255:30000` | Battery UDP address |
| `CHARGE_POWER_W` | `2500` | Charge power in watts |
| `DISCHARGE_POWER_W` | `2500` | Discharge power in watts |
| `TELEGRAM_BOT_TOKEN` | - | Optional: Telegram notifications |
| `TELEGRAM_CHAT_ID` | - | Optional: Telegram chat ID |

See `.env.example` for all options.

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

- **Single instance per host**: The battery client binds to UDP port 30000 (required by Marstek protocol). Only one instance can run per host.
- **Network requirements**: The service must be on the same network as the battery for UDP broadcast.

## Project Structure

```
cmd/trader/main.go       # Entry point
internal/config/         # Configuration (env parsing via caarlos0/env)
clients/
  nordpool/              # NordPool API client
  marstek/               # Battery UDP client
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
