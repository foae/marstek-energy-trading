# Energy Trader PRD

Product Requirements Document for the Marstek Energy Trading Bot.

## Overview

A Go service that performs energy price arbitrage using a Marstek Venus E battery. The service fetches NordPool day-ahead prices, identifies optimal charge/discharge windows, and controls the battery via an ESPHome HTTP REST API.

## Hardware

### Marstek Venus E Battery
- **Capacity**: 5.12 kWh
- **Efficiency**: 90% round-trip (10% energy loss per cycle)
- **Charge rate**: max 2500W
- **Discharge rate**: 800-2500W
- **Min SOC protection**: 11% (built-in)

### ESPHome Bridge (Default)
- **Protocol**: HTTP REST API
- **Default URL**: `http://192.168.1.50`
- **Endpoints**:
  - `GET /sensor/{name}` - Read sensor values (SOC, temperature, power)
  - `POST /number/{name}/set?value=X` - Set charge/discharge power
  - `POST /select/{name}/set?option=Y` - Set mode (charge/discharge/stop)
- **Entity names** (URL-encoded):
  - `Battery%20State%20Of%20Charge` - SOC percentage
  - `Forcible%20Charge%20Power` - Charge power setting
  - `Forcible%20Discharge%20Power` - Discharge power setting
  - `Forcible%20Charge%E2%81%84Discharge` - Mode select (charge/discharge/stop)

### Legacy UDP API (Preserved)
- **Protocol**: UDP JSON-RPC to `192.168.1.255:30000`
- **Documentation**: [docs/marstek-api.md](marstek-api.md)
- **Status**: Code preserved in `clients/marstek/` but not used by default

### NordPool API
- **Endpoint**: `https://dataportal-api.nordpoolgroup.com/api/DayAheadPriceIndices`
- **Resolution**: 15-minute intervals (96 data points/day)
- **Area**: NL (Netherlands)
- **Prices**: EUR/MWh (converted to EUR/kWh)

## Trading Strategy

### Sliding Window Algorithm

The analyzer finds optimal charge/discharge pairs using a sliding window approach:

1. **Calculate usable capacity** accounting for min SOC protection:
   ```
   usable_kWh = capacity_kWh * (1 - min_soc)
   ```
   Example: 5.12 kWh with 11% min SOC = 4.56 kWh usable

2. **Calculate window size** based on usable capacity and power:
   ```
   window_slots = ceil(usable_kWh / power_kW * 4)
   ```
   Example: 4.56 kWh at 2500W = ceil(1.82 * 4) = 8 slots (2 hours)

3. **Find most profitable cycle**: For each possible charge window position:
   - Calculate the average charge price for that window
   - Find the best discharge window after it (highest average price)
   - Calculate expected profit
   - Keep the pair with maximum profit

4. **Profitability check**: Only create a cycle if:
   - `discharge_avg > charge_avg / efficiency` (break-even)
   - `discharge_avg - charge_avg >= MIN_PRICE_SPREAD`

5. **Multiple cycles**: Repeat starting after the previous discharge window (up to `MAX_CYCLES_PER_DAY`)

### Average Price Tracking

During actual execution, the service tracks the **average price paid** across all slots in the charge window, not just the start price. This ensures accurate profitability calculations when deciding whether to discharge.

### Efficiency-Aware Trading

A trade is only profitable when:
```
discharge_price > charge_price / 0.90
```

Example: Charging at 0.10 EUR/kWh requires discharging at > 0.111 EUR/kWh to break even.

### Configurable Spread Threshold

Trades only execute when price spread exceeds `MIN_PRICE_SPREAD` (default: 0.05 EUR/kWh).

### Example Daily Pattern

```
Time    Price   Action
00:00   0.03    ─┐
01:00   0.04     │ Charge window (lowest avg)
02:00   0.05    ─┘
...
07:00   0.18    ─┐
08:00   0.22     │ Discharge window (highest avg after charge)
09:00   0.20    ─┘
...
13:00   0.05    ─┐
14:00   0.06     │ Second charge window (if profitable)
15:00   0.04    ─┘
...
18:00   0.25    ─┐
19:00   0.28     │ Second discharge window
20:00   0.24    ─┘
```

## Architecture

### Execution Model
- Continuous daemon process
- Main loop runs every minute
- Graceful shutdown on SIGINT/SIGTERM

### Data Persistence
- File-based JSON storage in `DATA_DIR`
- `trades.json` - trade history
- Uses `decimal` library for monetary precision

### Logging
- Structured JSON logs to stdout
- Configurable log level (debug/info/warn/error)

## HTTP API

### Endpoints

| Endpoint | Description |
|----------|-------------|
| `GET /health` | Liveness probe, returns "ok" |
| `GET /metrics` | Prometheus metrics |
| `GET /status` | Current state + full history (JSON) |

### Status Response

```json
{
  "current": {
    "state": "idle",
    "battery_soc": 75,
    "current_price_eur_kwh": 0.0854,
    "next_action": "waiting for next window"
  },
  "history": {
    "days": [
      {
        "date": "2026-02-02",
        "charged_kwh": "2.5",
        "discharged_kwh": "2.25",
        "charge_cycles": 1,
        "discharge_cycles": 1,
        "pnl_eur": "0.0325",
        "trades": [...]
      }
    ],
    "total_pnl_eur": "0.0325",
    "total_days": 1,
    "first_trade": "2026-02-02T08:00:00Z",
    "last_trade": "2026-02-02T18:30:00Z"
  }
}
```

## Telegram Integration

### Notifications (Outbound)

| Event | Message |
|-------|---------|
| Startup | "energy-trader started" |
| Trade start | "Charging started at 0.08 EUR/kWh (SOC: 45%)" |
| Trade end | "Charging completed. Energy: 2.5 kWh" |
| Error | "Battery unreachable" |
| Daily summary (23:59) | P&L, charged/discharged kWh, cycles, cumulative P&L |

### Commands (Inbound)

| Command | Response |
|---------|----------|
| `/status` | Current state, battery SOC, price, next action, P&L |

### Status Command Response

```
⏸️ Current Status

State: idle
Battery: 75%
Price: 0.0854 EUR/kWh
Next: waiting for next window

Today P&L: 0.0000 EUR
Total P&L: 0.0325 EUR
```

## Configuration

Load from `.env` file with fallback to environment variables.

| Variable | Default | Description |
|----------|---------|-------------|
| `SERVICE_NAME` | `energy-trader` | Service identifier |
| `LOG_LEVEL` | `info` | debug/info/warn/error |
| `HTTP_LISTEN_ADDR` | `:8080` | HTTP server address |
| `DATA_DIR` | `./data` | Data storage directory |
| `TZ` | `Europe/Amsterdam` | Timezone |
| `NORDPOOL_AREA` | `NL` | Price area code |
| `NORDPOOL_CURRENCY` | `EUR` | Currency |
| `MIN_PRICE_SPREAD` | `0.05` | Min spread to trade (EUR/kWh) |
| `BATTERY_EFFICIENCY` | `0.90` | Round-trip efficiency |
| `BATTERY_CAPACITY_KWH` | `5.12` | Battery capacity (kWh) |
| `BATTERY_MIN_SOC` | `0.11` | Minimum SOC (0.0-1.0) |
| `MAX_CYCLES_PER_DAY` | `2` | Max charge/discharge cycles per day |
| `ESPHOME_URL` | `http://192.168.1.50` | ESPHome device URL |
| `BATTERY_UDP_ADDR` | - | Legacy UDP address (optional) |
| `CHARGE_POWER_W` | `2500` | Charge power (watts) |
| `DISCHARGE_POWER_W` | `2500` | Discharge power (watts) |
| `PASSIVE_MODE_TIMEOUT_S` | `300` | Passive mode timeout |
| `TELEGRAM_BOT_TOKEN` | - | Telegram bot token |
| `TELEGRAM_CHAT_ID` | - | Telegram chat ID |

## Scheduling

| Time | Action |
|------|--------|
| Every 1 min | Check battery, execute trades |
| Every 5 sec | Poll Telegram commands |
| Every 15 min | Check if prices need fetching |
| 13:00 CET | Fetch next day's prices |
| 23:59 | Send daily summary |

## Project Structure

```
marstek-energy-trading/
├── cmd/trader/main.go           # Entry point
├── handler/handler.go           # HTTP endpoints
├── service/
│   ├── service.go               # Trading engine
│   ├── analyzer.go              # Price analysis
│   ├── recorder.go              # Trade recording (decimal)
│   └── interfaces.go            # BatteryController interface
├── clients/
│   ├── esphome/client.go        # ESPHome HTTP client (default)
│   ├── marstek/client.go        # Battery UDP (legacy, preserved)
│   ├── nordpool/client.go       # NordPool API
│   └── telegram/client.go       # Telegram bot
├── internal/config/config.go    # Configuration
├── docs/
│   ├── marstek-api.md           # Legacy UDP API docs
│   └── energy-trader-prd.md     # This file
├── Dockerfile
├── Makefile
└── .env.example
```

## Build & Run

```bash
make build          # Build binary
make run            # Run locally
make test           # Run tests
make docker-build   # Build Docker image
```

## Out of Scope (v1)

- HomeWizard P1 meter integration
- Web UI dashboard
- Multiple battery support
- Dynamic rate adjustment
- Database persistence (PostgreSQL/SQLite)
