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

The analyzer compares executable total-EUR grid cycles, an initial contiguous trusted-inventory sale followed by grid cycles, and holding inventory over known today/tomorrow prices. A durable DC ledger permits only measured service-controlled charge energy to be sold; SOC caps it downward. SOC can raise the allowance back toward that cap only while a sale is in flight, the in-session link check reports the link live, and SOC has changed within the last 10 minutes (`inventorySOCLiveWindow`); that also recomputes the inventory deadline, bounded by the window end. A SOC frozen by a dead link never moves and the link check has already invalidated the allowance, so it cannot replenish. Observed but untrusted energy remains quarantined: it reduces purchase capacity without earning discharge revenue. Inventory sales need positive export value but are exempt from the grid-cycle cap and profit floor. A sale candidate must deliver at least 5% of a full delivery (`minimumInventorySaleShare`) in addition to the existing one-minute minimum window: about 0.19 kWh AC on the 5.12 kWh battery, roughly five minutes at 2200 W, so integer SOC must be about 17% or more before a sale is selected. This suppresses one-minute sliver sales at 12-13% SOC that cost Modbus writes for about a cent. Equal value prefers fewer sessions, then earlier execution. This is bounded contiguous-sale optimization, not arbitrary-slot dispatch or hindsight optimality. Active sales retain their tariff endpoint but higher measured draw can shorten the inventory deadline. Grid commitments are revalidated before deadline-bounded control and never bypass the trusted-inventory gate.

Execution (`service/service.go`, `service/charging_reservations.go`):
- Reserve the cheapest remaining known grid slots to reach 100% by the next cheap-window deadline, recalculating from actual SOC with zero future solar forecast. Exclude every grid slice that would individually violate the configured expected-profit floor.
- Use AC-side energies: purchases cover DC shortfall divided by charge efficiency; discharge delivers usable DC energy times round-trip efficiency divided by charge efficiency. A grid cycle fills to 100%; reservations cannot precede a selected inventory sale's end.
- Extend a running, truncated reservation slice to its 15-minute tariff boundary when displacing that energy onto cheaper reserved slices costs under one cent, so falling prices cannot force a stop/start at every boundary.
- Do not start or refresh automatic control in the final minute of its window; bind ESPHome control and battery-power verification to the active window deadline.
- Persist an in-flight inventory marker before every discharge attempt, including manual override; debit conservatively through confirmed idle. Automatic deadlines reserve 45 seconds for normal stopping, not a hardware expiry. Credit tolerates sample gaps up to 60 seconds (`inventoryCreditGapTolerance`); a longer gap earns nothing. Debit divides AC watts by the discharge-side efficiency (round-trip over charge-side, `BATTERY_EFFICIENCY / BATTERY_CHARGE_EFFICIENCY`) and takes the maximum with the measured DC draw. Startup confirms idle before restore; missing, interrupted, or configuration-mismatched inventory starts at zero. Corrupt inventory or failed publication blocks discharge.
- Capture qualified solar without a tariff, opportunity-cost, reservation-feasibility or paired-profit veto. Selected sales and active grid reservations retain control priority. Reserved charging already consumes available solar, so preserve the command and record the measured source split instead of relabelling the session. Inventory sales continue through missing tariffs as unpriced energy but stop on a confirmed nonpositive export tariff; grid obligations remain distinct.
- While the in-session link check reports the RS485 link down during a charge or discharge session, measured AC power books zero energy instead of integrating the last reading the bridge keeps serving. Energy through the moment of detection is settled at the last reading, and the affected seconds are recorded on the trade as `telemetry_gap_s` (optional field in `trades.json`, omitted when zero). The same gate covers the one-second scheduled-charge sampler and solar charging accounting. The reboot gate applies on every path that can reach the restart, including a failed stop or start; graceful shutdown retries the stop every 15 seconds inside its 60-second budget instead of waiting out the five minutes.
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
- **Energy accounting**: Integrate measured battery input from AC power and the grid-attributable portion (`min(acChargePower, max(netGridImport, 0))`) separately; control compensation uses the same measured AC charge power. Price grid intervals at their applicable slot, exposing missing-price energy rather than labelling it free solar. Estimates retain last observed power across telemetry gaps; historical unsplit records remain all-solar.
- **Scheduled-charge accounting**: The existing one-second meter loop also samples AC input and P1 during reserved charges. New split records distinguish grid cost, forgone solar export and energy whose source is unknown before the first valid P1 sample. Legacy scheduled records remain all-grid. Cash-flow and opportunity-cost-adjusted metrics are separate; commanded discharge is not guaranteed metered export.

### Scheduled window priority
Solar charging and scheduled trading never conflict — three rules enforce strict priority:
1. **Yield on entry**: Stop and record solar before immediately starting a reserved grid charge or scheduled discharge.
2. **Block during reservation**: A separate solar session cannot start during an active grid reservation or discharge window. No tariff veto applies outside those controls.
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
effectiveSurplus = measuredSurplus + measuredACChargePower
```

### Battery ramp-up cooldown
The battery takes ~3 seconds to ramp to a new power target. Compensation uses measured AC charge power rather than the command target. A **5-second cooldown** after any power change (start or adjust) prevents re-adjustment until the battery has settled.

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
7. **P1 meter feedback loop**: AC-coupled battery draw is visible on the P1 meter — compensate with measured AC charge power (what the meter actually sees), not the DC battery power and not the command target, during active charging.
8. **Battery ramp-up transients**: Don't re-adjust power within 5s of a change — the battery hasn't reached the target yet and readings are unreliable
9. **Solar micro-cycling**: Anti-cycling constants (`solarLowSurplusGrace`, `solarRestartCooldown`, `solarShortSessionCooldown`, `solarLongBackoffCooldown`, `solarShortSessionThreshold`, `solarShortSessionBackoffCount`, `solarStartQualification`, `solarMinChargePowerW`, `solarEMAAlpha`) are package-level implementation details, not env vars.
10. **RS485 link freeze**: the link can wedge mid-session, but the stall is usually battery-side, not ESP32-side. The Venus E's own Modbus side stalls under load and recovers after a few minutes of bus silence; continued enable/stop write bursts keep it dead for hours, and ESP32 reboots rarely help. Keep the bus quiet while the link is down: read only ESPHome's cached HTTP values, retry a failed stop no sooner than `batteryLinkDownStopRetryInterval` (5 min), and reboot the bridge only after `bridgeRestartAfterLinkDown` (5 min down) and at most once per `bridgeRestartMinInterval` (1 hour). A reboot must not trigger an immediate stop retry. Live telemetry seen by the in-session link check returns the retry to the normal 5-second cadence. Never add write retries faster than the link-down interval; keep the staleness check and the restart-button path working.
