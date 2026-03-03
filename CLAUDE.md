# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Energy market minitrader - a Go service that performs energy price arbitrage using a Marstek Venus E battery (5.12 kWh). It fetches NordPool day-ahead prices, identifies optimal charge/discharge windows, and controls the battery via ESPHome HTTP REST API.

## Hardware Constraints

- **Marstek Venus E battery**: 5.12 kWh capacity, 90% efficiency, 11% min discharge protection
- **Charge rate**: max 2500 W
- **Discharge rate**: 800-2500 W
- **HomeWizard P1 meter**: Solar surplus detection via HTTP REST API. Auto-discovers via mDNS (`_hwenergy._tcp`) then HTTP scan (`192.168.0.x/1.x`) when no URL configured.

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
  homewizard/            # HomeWizard P1 meter (solar surplus + auto-discovery)
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
- `HOMEWIZARD_P1_URL`: P1 meter URL (optional; empty = auto-discover via mDNS + HTTP scan)
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

## Solar Self-Consumption Charging

When the HomeWizard P1 meter is enabled, the service captures solar surplus by charging the battery instead of exporting to the grid. This runs independently of scheduled trading.

### How it works (`service/service.go: solarTick`)
- **Polling**: Every 1 second, reads P1 meter (`active_power_w`) and battery status
- **Surplus calculation**: `surplus = -activePowerW` (negative P1 = exporting to grid)
- **Start condition**: 3 consecutive readings above `SOLAR_MIN_SURPLUS_W` (default 100W)
- **Power tracking**: Charges at the detected surplus power, dynamically adjusted with 50W deadband
- **Priority**: Yields immediately to scheduled charge/discharge windows

### P1 meter feedback loop compensation
The Marstek Venus E is an AC-coupled battery — its charge power draws through the P1 meter. When the battery starts charging, the measured surplus drops by the charge amount:
```
Real surplus:       300W
Battery charges at: 300W
P1 meter sees:      300W - 300W = 0W  (looks like no surplus)
```
Without compensation, this causes oscillation (start→surplus drops→stop→surplus returns→start).

**Fix**: When already in `StateSolarCharging`, the stop-threshold and power adjustment use the **effective surplus**:
```
effectiveSurplus = measuredSurplus + currentChargePower
```

### Battery ramp-up cooldown
The battery takes ~3 seconds to ramp to a new power target. During ramp-up, `effectiveSurplus` overestimates reality (battery draws less than commanded), which can cause a power adjustment spiral. A **5-second cooldown** after any power change (start or adjust) prevents re-adjustment until the battery has settled.

### State machine
```
StateIdle → StateSolarCharging → StateIdle
  ↓                                  ↑
  └─ (scheduled window starts) ──────┘
```
Solar charging occupies its own state (`StateSolarCharging`) distinct from scheduled `StateCharging`/`StateDischarging`. Scheduled windows always take priority.

## Deployment

No CI/CD pipeline — manual deploy to `olivier` machine:
1. Make code changes locally, commit & push
2. SSH into olivier, `cd ~/Projects/marstek-energy-trading`, `git pull`
3. `make docker-build`
4. `docker stop energy-trader && docker rm energy-trader`
5. `docker run -d --name energy-trader --network=host --env-file .env -v $(pwd)/data:/app/data energy-trader:latest`

Logs: `docker logs --tail 100 energy-trader`

## Common Pitfalls

1. **Lock during I/O**: Always release mutex before network calls
2. **Float for prices**: Use decimal.Decimal
3. **State loss on restart**: Restore critical state from persisted data
4. **Timezone issues**: Use explicit `time.Location` for all time operations
5. **Non-atomic file writes**: Use temp file + rename pattern
6. **Notification spam**: Rate limit error notifications
7. **P1 meter feedback loop**: AC-coupled battery draw is visible on the P1 meter — always compensate with `effectiveSurplus = measured + chargePower` when checking thresholds during active charging
8. **Battery ramp-up transients**: Don't re-adjust power within 5s of a change — the battery hasn't reached the target yet and readings are unreliable
