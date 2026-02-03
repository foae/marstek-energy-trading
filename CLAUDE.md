# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Energy market minitrader - a Go service that performs energy price arbitrage using a Marstek Venus E battery (5.12 kWh). It fetches NordPool day-ahead prices, identifies optimal charge/discharge windows, and controls the battery via ESPHome HTTP REST API.

## Hardware Constraints

- **Marstek Venus E battery**: 5.12 kWh capacity, 90% efficiency, 11% min discharge protection
- **Charge rate**: max 2500 W
- **Discharge rate**: 800-2500 W
- **HomeWizard P1 meter**: Planned feature, not yet implemented

## API Documentation

- **ESPHome REST API**: `http://192.168.1.50` (default) - HTTP REST with JSON responses. Used for battery control.
- **Marstek UDP API**: `192.168.1.255:30000` - JSON-RPC protocol. Legacy, see [docs/marstek-api.md](docs/marstek-api.md)
- **NordPool API**: `https://dataportal-api.nordpoolgroup.com/api/DayAheadPriceIndices` - 15-min resolution

## Build Commands

```bash
make build              # build binary
make run                # run locally
make test               # run all tests
make test-one TEST=TestName  # run single test
make docker-build       # build Docker image
```

## Project Structure

```
cmd/trader/main.go       # Entry point
internal/config/         # Configuration (env parsing via caarlos0/env)
clients/
  esphome/               # ESPHome HTTP client (default battery backend)
  marstek/               # Battery UDP client (legacy, preserved but unwired)
  nordpool/              # NordPool API client (timezone-aware)
  telegram/              # Telegram notifications
service/
  service.go             # Trading engine + main loop
  analyzer.go            # Price analysis + window detection
  recorder.go            # Trade/P&L recording (atomic JSON writes)
  interfaces.go          # Interfaces for dependency injection
handler/                 # HTTP health/metrics/status
```

## HTTP Endpoints

- `GET /health` - liveness check
- `GET /metrics` - Prometheus metrics
- `GET /status` - current state + trade history (JSON)

## Configuration

Copy `.env.example` to `.env`. Key settings:
- `ESPHOME_URL`: ESPHome device URL (default `http://192.168.1.50`)
- `MIN_PRICE_SPREAD`: Minimum EUR/kWh spread to trigger trading
- `BATTERY_EFFICIENCY`: Round-trip efficiency (default 0.90)
- `CHARGE_POWER_W` / `DISCHARGE_POWER_W`: Power rates in watts
- `TZ`: Timezone for scheduling (default Europe/Amsterdam)

## Code Conventions

### Monetary Values
- **Use `decimal.Decimal` (shopspring/decimal) for all prices and monetary calculations**
- Never use float64 for prices - floating point errors accumulate in financial calculations
- Convert to float64 only at API boundaries (JSON output, external APIs)

### Concurrency
- Use `sync.RWMutex` for state protection
- **Release locks during network I/O** - don't hold mutex while calling battery/API
- Pattern: copy values needed, unlock, do I/O, lock, update state

### Error Handling
- Rate limit error notifications (15 min cooldown) to avoid spam
- Use `slog.Warn` for recoverable errors, `slog.Error` for critical
- Don't crash on transient failures - log and continue

### Logging Guidelines
- Use `slog` with JSON handler for structured logging
- **Use contextual logging**: Create a logger with context at function start, reuse it throughout
  ```go
  l := slog.With("action", "charge", "price_eur_kwh", price, "soc", soc)
  l.Info("starting charge session")
  // ... later in the same function
  l.Error("failed to start charging", "error", err)
  ```
- **Info level**: State changes (charging/discharging start/stop), price fetches, trading windows
- **Debug level**: Routine checks (battery polls, passive mode refresh)
- **Warn level**: Recoverable issues (API failures, notification errors)
- **Error level**: Critical failures (battery unreachable)
- Decision logs should include: price, SOC, thresholds, reason
- Enrich logger context as you progress: `l = l.With("new_field", value)`

### Testing
- Use interfaces (`PriceProvider`, `BatteryController`) for dependency injection
- Mock external dependencies in integration tests
- Test actual behavior (trading decisions), not implementation details (tickers)
- Integration tests should cover full decision paths with realistic scenarios

### File I/O
- Use atomic writes for data files: write to temp file, then `os.Rename()`
- Prevents corruption if process crashes during write

### ESPHome HTTP Protocol
- Stateless HTTP REST - each request is independent
- Sensor values: GET `/sensor/{name}` returns JSON with `state` field
- Commands: POST `/number/{name}/set?value=X` or `/select/{name}/set?option=Y`
- No auto-timeout on charge/discharge - service's refresh loop re-sends commands
- Entity names use URL encoding (spaces as `%20`, Unicode division slash as `%E2%81%84`)

### Legacy UDP Protocol (Marstek)
- Validate response ID matches request ID (loop until match or timeout)
- Client binds to port 30000 (protocol requirement) - only one instance per host
- Close connection on shutdown
- Code preserved in `clients/marstek/` but not wired into main.go

## Trading Algorithm

The analyzer (`service/analyzer.go`) creates a `TradingPlan` from daily prices:
1. Find min/max prices for the day
2. Charge windows = price slots in bottom 25% of range
3. Discharge windows = price slots in top 25% of range
4. Profitable if: spread >= MIN_PRICE_SPREAD AND maxPrice > minPrice/efficiency

Execution (`service/service.go`):
- Every minute, check if current time is in a window
- Track `lastChargePrice` for per-trade profitability check
- Restore `lastChargePrice` from trade history on startup (state recovery)

## Common Pitfalls

1. **Lock during I/O**: Always release mutex before network calls
2. **Float for prices**: Use decimal.Decimal
3. **State loss on restart**: Restore critical state from persisted data
4. **Timezone issues**: Use explicit `time.Location` for all time operations
5. **Non-atomic file writes**: Use temp file + rename pattern
6. **Notification spam**: Rate limit error notifications
