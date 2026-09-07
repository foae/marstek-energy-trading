# AGENTS.md

Guidance for AI coding agents working with code in this repository.

## Project Overview

Energy market minitrader - a Go service that performs energy price arbitrage using a Marstek Venus E battery (5.12 kWh). It fetches NordPool day-ahead prices, identifies optimal charge/discharge windows, and controls the battery via ESPHome HTTP REST API.

## Hardware Constraints

- **Marstek Venus E battery**: 5.12 kWh capacity, 90% efficiency, 11% min discharge protection
- **Charge rate**: max 2500 W
- **Discharge rate**: 800-2500 W
- **HomeWizard P1 meter**: Solar surplus detection via HTTP REST API. Discovery is opt-in with `HOMEWIZARD_P1_URL=auto` (mDNS, then an HTTP scan of `192.168.0.x/1.x`).

## API Documentation

- **ESPHome REST API**: configured with the required `ESPHOME_URL` - HTTP REST with JSON responses. Used for battery control.
- **Marstek UDP API**: JSON-RPC protocol. Legacy library code only; see [docs/legacy-udp.md](docs/legacy-udp.md).
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
- `ESPHOME_URL`: required ESPHome device URL
- `HOMEWIZARD_P1_URL`: P1 meter URL (optional; empty = disabled, `auto` = opt-in discovery)
- `MIN_PRICE_SPREAD`: Minimum expected profit in EUR/kWh after efficiency loss (historical name)
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
- Passive mode refresh is verify-first: read the reported mode/power back and re-write only on mismatch (no Modbus writes otherwise)
- Select writes retry once after 15 s because the select value only updates on the next Modbus poll
- `CheckLink` staleness check runs every tick while a session is active (charging, discharging, manual discharging, solar charging)

### Legacy UDP Protocol (Marstek)
- Validate response ID matches request ID (loop until match or timeout)
- Client binds to port 30000 (protocol requirement) - only one instance per host
- Close connection on shutdown
- Code preserved in `clients/marstek/` but not wired into main.go

## Trading Algorithm

The analyzer (`service/analyzer.go`) selects globally optimal non-overlapping charge→discharge cycles over the known today/tomorrow horizon, subject to expected profit after efficiency loss and cycle limits. Active automatic cycles retain their plan while refreshed plans are staged; a grid charge persists and revalidates its paired cycle before deadline-bounded battery control so the commitment survives restart without authorizing a different cycle. Restored cycles that fail the current profit floor retain only their discharge obligation.

Execution (`service/service.go`, `service/charging_reservations.go`):
- Reserve the cheapest remaining known grid slots to reach 100% by the next cheap-window deadline, recalculating from actual SOC with zero future solar forecast. Exclude every grid slice that would individually violate the configured expected-profit floor.
- Do not start or refresh automatic control in the final minute of its window; bind ESPHome control and battery-power verification to the active window deadline.
- Solar replaces reserved grid energy only when its forgone export value is no greater than the marginal reservation price. If economics makes the reservation infeasible, solar must still satisfy the paired cycle's per-slice expected-profit ceiling; that ceiling remains active between the charge deadline and paired discharge. Import/export use the same tariff.
- Measured taper reduces available delivery capacity; expose infeasibility and attempt best effort rather than guaranteeing 100%.
- `lastChargePrice` is restored for information only, never a discharge gate.

## Solar Self-Consumption Charging

When the HomeWizard P1 meter is enabled, the service captures solar surplus by charging the battery instead of exporting to the grid. This runs independently of scheduled trading.

### How it works (`service/service.go: solarTick`)
- **Polling**: Every 1 second, reads P1 meter (`active_power_w`) and battery status
- **Surplus calculation**: `surplus = -activePowerW` (negative P1 = exporting to grid)
- **Start condition**: 30 seconds of sustained raw surplus above `SOLAR_MIN_SURPLUS_W` (default 100W). Failed reads or telemetry gaps over two seconds reset qualification.
- **EMA smoothing**: Elapsed-time smoothing equivalent to alpha=0.05 at one-second intervals; power targets and stop thresholds use the EMA, not raw readings.
- **Stop condition**: EMA below `max(SOLAR_MIN_SURPLUS_W / 4, 75W)` starts a 60-second low-surplus grace timer. During grace, request 75W; recovery clears the timer. Stop if insufficient surplus persists through grace. Battery-full protection, scheduled windows, and repeated telemetry failures override grace.
- **Adaptive restart cooldown**: After a session stops, no new session can start until `solarCooldownUntil`. The cooldown is computed at stop time based on why/how long the session ran:
  - Legitimate stop (battery full, yielding to scheduled window) OR session ≥ 10 min: `solarRestartCooldown` (60 s baseline).
  - "Surplus gone" stop on a marginal session (< 10 min): `solarShortSessionCooldown` (5 min).
  - After `solarShortSessionBackoffCount` (3) consecutive short surplus-gone sessions: `solarLongBackoffCooldown` (15 min).
  The consecutive-short counter resets on any legitimate stop OR any session that runs past the short threshold.
- **Power floor**: During low-surplus grace, request 75W, bypassing the normal power deadband but retaining the five-second settling interval. At zero surplus, 60 seconds at this floor imports 1.25Wh; this excludes EMA settling and is not a whole-house import bound.
- **Power tracking**: Charges at the EMA-smoothed surplus power, dynamically adjusted with 50W deadband outside low-surplus grace.
- **Priority**: Active grid reservations and discharge windows override solar charging.
- **Fault priority**: Read battery status and evaluate full/window stops before reading P1. Failed adjustments immediately request a confirmed stop; retained failure state retries only through the throttled stop path, with a five-minute cooldown after success.
- **Energy accounting**: Integrate measured battery input and the grid-attributable portion (`min(batteryChargePower, max(netGridImport, 0))`) separately. Price grid intervals at their applicable slot, exposing missing-price energy rather than labelling it free solar. Estimates retain last observed power across telemetry gaps; historical unsplit records remain all-solar.

### Scheduled window priority
Solar charging and scheduled trading never conflict — three rules enforce strict priority:
1. **Yield on entry**: Stop and record solar before immediately starting a reserved grid charge or scheduled discharge.
2. **Block during reservation**: Solar cannot start during a grid reservation or discharge window, or when exporting now and importing reserved cheaper energy is preferable.
3. **Resume**: Outside those constraints, sustained qualifying surplus can start solar again.

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
effectiveSurplus = measuredSurplus + measuredBatteryChargePower
```

### Battery ramp-up cooldown
The battery takes ~3 seconds to ramp to a new power target. Compensation uses measured battery power rather than the command target. A **5-second cooldown** after any power change (start or adjust) prevents re-adjustment until the battery has settled.

### State machine
```
StateIdle → StateSolarCharging → StateIdle
  ↓                                  ↑
  └─ (scheduled window starts) ──────┘
```
Solar charging occupies its own state (`StateSolarCharging`) distinct from scheduled `StateCharging`/`StateDischarging`. Scheduled windows always take priority.

## Local Operations

If `AGENTS.local.md` exists, read it before making changes. It contains ignored maintainer-only deployment instructions. Public contributors must not commit, push, deploy, or operate hardware unless the repository owner explicitly requests it.

## Common Pitfalls

1. **Lock during I/O**: Always release mutex before network calls
2. **Float for prices**: Use decimal.Decimal
3. **State loss on restart**: Restore critical state from persisted data
4. **Timezone issues**: Use explicit `time.Location` for all time operations
5. **Non-atomic file writes**: Use temp file + rename pattern
6. **Notification spam**: Rate limit error notifications
7. **P1 meter feedback loop**: AC-coupled battery draw is visible on the P1 meter — compensate with measured battery charge power, not the command target, during active charging.
8. **Battery ramp-up transients**: Don't re-adjust power within 5s of a change — the battery hasn't reached the target yet and readings are unreliable
9. **Solar micro-cycling**: Anti-cycling constants (`solarLowSurplusGrace`, `solarRestartCooldown`, `solarShortSessionCooldown`, `solarLongBackoffCooldown`, `solarShortSessionThreshold`, `solarShortSessionBackoffCount`, `solarStartQualification`, `solarMinChargePowerW`, `solarEMAAlpha`) are package-level implementation details, not env vars.
10. **RS485 link freeze**: the ESPHome Modbus hub can wedge mid-session; only an ESP32 reboot recovers it. Never add write retries faster than the poll interval; keep Modbus write volume minimal; keep the staleness check and the restart-button path working.
