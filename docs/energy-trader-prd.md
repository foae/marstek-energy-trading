# Energy Trader PRD

Product Requirements Document for the Marstek Energy Trading Bot.

## Overview

A Go service that controls a Marstek Venus E battery with NordPool day-ahead prices and an optional HomeWizard P1 meter. It jointly compares observed stored inventory, profitable grid cycles, and holding energy using known tariffs; qualified solar surplus is captured without an economic price veto. Solar is not free profit: its forgone export value is recorded separately from cash flow.

## Hardware

### Marstek Venus E Battery
- **Capacity**: 5.12 kWh
- **Efficiency**: 90% round-trip (10% energy loss per cycle)
- **Charge rate**: max 2500W
- **Discharge rate**: 800-2500W
- **Min SOC protection**: 11% (built-in)

### ESPHome Bridge (Default)
- **Protocol**: HTTP REST API
- **URL**: Required through `ESPHOME_URL`; no device address is assumed
- **Endpoints**:
  - `GET /sensor/{name}` - Read sensor values (SOC, temperature, power)
  - `POST /number/{name}/set?value=X` - Set charge/discharge power
  - `POST /select/{name}/set?option=Y` - Set mode (charge/discharge/stop)
- **Entity names** (URL-encoded):
  - `Battery%20State%20Of%20Charge` - SOC percentage
  - `Forcible%20Charge%20Power` - Charge power setting
  - `Forcible%20Discharge%20Power` - Discharge power setting
  - `Forcible%20Charge%E2%81%84Discharge` - Mode select (charge/discharge/stop)

### HomeWizard P1 Meter (Optional)
- **Protocol**: HTTP REST API
- **Endpoint**: `GET /api/v1/data` - returns `active_power_w` (positive = import, negative = export)
- **Connectivity check**: `GET /api` - returns device info
- **Timeout**: 5 seconds
- **Purpose**: Detects solar surplus (grid export) for battery charging; captured solar is valued at the configured export tariff.
- **Auto-discovery**: Only when `HOMEWIZARD_P1_URL=auto`, the service attempts two discovery methods in order:
  1. **mDNS** (3s timeout): Browses `_hwenergy._tcp` on the local network. Filters for `product_type=HWE-P1` and `api_enabled=1` in TXT records.
  2. **HTTP scan** (30s timeout): Falls back to probing `GET /api` on `192.168.0.x` and `192.168.1.x` (64 concurrent workers, 500ms connect timeout). Checks `product_type=HWE-P1` in JSON response. Useful when mDNS is unavailable (e.g., Docker bridge networks).
  If both methods fail, P1 features are gracefully disabled. An empty value disables P1 support without scanning the LAN.

### Legacy UDP API (Preserved)
- **Protocol**: UDP JSON-RPC to a configured device address
- **Documentation**: [legacy-udp.md](legacy-udp.md)
- **Status**: Library code is preserved in `clients/marstek/` but is not wired into the executable

### NordPool API
- **Endpoint**: `https://dataportal-api.nordpoolgroup.com/api/DayAheadPriceIndices`
- **Resolution**: 15-minute intervals (96 data points/day)
- **Area**: NL (Netherlands)
- **Rates**: EUR/MWh wholesale data converted to one configured all-in EUR/kWh rate: `(NordPool + energy tax) × (1 + VAT) + supplier fee`. That rate prices grid import; discharge/export and solar opportunity cost use the configured export tariff (`EXPORT_PRICE_MODE`), symmetric with the import rate by default or wholesale-based with `EXPORT_FEE_EUR_PER_KWH`.

## Trading Strategy

### Joint inventory and grid planning

1. **Determine trusted inventory.** A durable DC ledger is replenished only by valid measured charging samples while the service controls charging; sample gaps up to 60 seconds are tolerated and longer gaps earn nothing. Fresh SOC supplies a downward cap:
   ```
   trusted_inventory_kWh = min(ledger_kWh, max(0, capacity_kWh * (observed_soc - min_soc)))
   ```
   Observed energy above that allowance is quarantined: it reduces available purchase capacity but cannot generate planned discharge revenue. Missing, interrupted, or configuration-mismatched ledgers start at zero; gaps longer than 60 seconds cannot earn credit. While a sale is in flight and the link check reports the link live, a SOC that has changed within the last 10 minutes can raise the allowance back up to the SOC cap and recompute the inventory deadline within the sale window; a SOC frozen by a dead link cannot.

2. **Evaluate grid pairs.** A contiguous discharge window is paired with the cheapest executable, individually eligible charge slices before it; the charge slices need not be contiguous. Expected profit per input kWh is `discharge_average * efficiency - charge_average`; it must be strictly positive and meet `MIN_PRICE_SPREAD`.

3. **Compare total-EUR alternatives using the initial inventory once.** The planner can hold inventory, leave it available to reduce the first grid purchase, or sell it in one contiguous known-positive-export-price window followed by non-overlapping grid cycles. The sale can be shorter than the available inventory or end partway through a tariff interval, but it must span at least one minute and deliver at least 5% of a full delivery, about 0.19 kWh AC on a 5.12 kWh battery, so sliver sales below about 17% integer SOC are not selected. It uses integrated prices and actual energy, not average-slot economics. Following grid reservations cannot start before the selected inventory sale ends.

4. **Select the plan.** Grid cycles are selected chronologically to maximize total EUR over the known tariff horizon and stay within `MAX_CYCLES_PER_DAY`; the inventory-only sale is exempt from that grid-cycle allowance. Equal values prefer fewer control sessions and then earlier choices. This is the best contiguous sale under known prices, not a guarantee of arbitrary-slot optimization, future prices, or hindsight revenue. Unknown or nonpositive export prices do not trigger an uncommitted inventory sale; remaining energy is held. There is no solar forecast or made-up battery-wear floor.

### SOC-aware grid reservations

For the next unfinished grid charge cycle, the service derives a deadline from its charge-window end and calculates the grid input needed to reach 100% from current SOC, dividing the SOC shortfall by the charge-side efficiency `BATTERY_CHARGE_EFFICIENCY` rather than by the round-trip figure, since the discharge-side loss is not paid on import. It considers eligible 15-minute price slices from the known tariff sets after the prior planned discharge and through that deadline, then reserves the latest slices priced within `CHARGE_DEFER_TOLERANCE_EUR_PER_KWH` of the cheapest allocation's average price, filling any remaining shortfall from the cheapest of the other slices, and sizes the reservation at `CHARGE_POWER_W` times `CHARGE_PLANNING_DERATE`. The earliest reserved piece starts one minute early, since the control loop ticks once a minute and is not aligned to it. While charging, the slice containing the current time is retained when it is within the tolerance or when finishing its remaining energy costs under one cent; a dear slot merely entered at a tariff boundary is used only if the shortfall needs it. Deferral is active only when a P1 meter is configured; without one the cheapest slices are reserved as before, under the same running-slice rule. Observed stored energy, including captured solar, reduces the required purchase; no solar is forecast. After a grid charge has run for 30 seconds, any lower observed charging power becomes the deliverability limit. Every grid slice must preserve the paired cycle's `MIN_PRICE_SPREAD` floor.

### Execution accounting and discharge

Executed charge and discharge energy is integrated from measured AC-power samples and priced across retained 15-minute rate slots. Energy without an applicable retained rate is explicitly unpriced. New split scheduled-charge records carry an explicit source-attribution marker and record separately attributed grid and solar portions: priced grid cost remains in cash flow, while forgone solar export is priced as opportunity cost. Records without that marker retain their legacy interpretation. Cash-flow P&L is priced discharge value minus priced grid cost. The separately reported opportunity-cost-adjusted metric also deducts priced solar opportunity cost; neither is inventory-matched trading profit.

Scheduled discharge starts in its planned window only with sufficient trusted inventory above the SOC minimum and time for a conservative stop. A durable in-flight marker precedes every discharge attempt. DC debit uses the greater of measured AC power divided by the discharge-side efficiency (`BATTERY_EFFICIENCY / BATTERY_CHARGE_EFFICIENCY`) and observed draw through confirmed idle; higher draw can shorten the automatic deadline. An active inventory sale can continue through a missing tariff, recording unpriced energy, but stops on confirmed nonpositive export value. Durable grid-cycle obligations retain their distinct tariff handling but do not bypass the inventory gate. `lastChargePrice` remains informational.

### Solar Self-Consumption

When a HomeWizard P1 meter is configured, the service detects grid export (solar surplus) and charges the battery, bridging brief dips at low power:

1. **Detection**: P1 meter is polled every 1 second. Negative `active_power_w` = exporting to grid = solar surplus.
2. **Start confirmation**: Requires 30 seconds of sustained surplus above `SOLAR_MIN_SURPLUS_W` (default: 100W). Failed readings or telemetry gaps longer than two seconds reset qualification; EMA smoothing uses elapsed time.
3. **Charging and compensation**: Battery charging is clamped to `CHARGE_POWER_W`. The AC-side feedback compensation is `effectiveSurplus = measuredSurplus + measuredACChargePower`; the P1 meter sees that AC draw, so DC battery power is not substituted. A five-second settling cooldown after power changes prevents a positive-feedback spiral.
4. **Low surplus and full battery**: EMA below `max(SOLAR_MIN_SURPLUS_W / 4, 75W)` starts a 60-second grace requesting 75W; recovery clears it. Adaptive cooldowns reduce short sessions. Solar stops at 99% SOC and can qualify again at 97%; a grid reservation can charge to 100%.
5. **Priority and pricing**: Safety/fault handling, manual control, selected or active automatic discharge, and current grid reservations take priority. Otherwise qualified solar capture has no tariff, forecast, reservation-economics, or grid-cycle-profit veto: missing, negative, or changing tariffs do not alone prevent or stop capture.
6. **Recording**: Measured AC energy, separately attributed grid input/cost, and forgone solar-export opportunity cost are recorded. Known rate slots price grid cost at the import rate and forgone export at the export rate; unavailable prices remain explicitly unpriced. New split scheduled-charge records carry source attribution; records without it retain their historical interpretation.

### Configurable Profit Threshold

Candidate cycles execute only when their expected profit after the configured round-trip efficiency loss meets `MIN_PRICE_SPREAD` (default: 0.05 EUR/kWh). The environment variable retains its historical name but no longer represents the raw average-price spread.

### Example Daily Pattern

```
Time    Price   Action
00:00   0.03    ─┐
01:00   0.04     │ Selected charge window
02:00   0.05    ─┘
...
07:00   0.18    ─┐
08:00   0.22     │ Selected later discharge window
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
- Main trading loop runs every minute
- Solar tick loop runs every 1 second (when P1 meter configured, nil channel when disabled)
- Graceful shutdown on SIGINT/SIGTERM
### Control confirmation and fail-safe

The ESPHome client first reads each select value and writes only when it differs. It accepts a mode change only after reading the requested value back: it polls for up to 35 seconds and may retry the write once after 15 seconds, the ESPHome publication interval. The service then verifies signed measured battery power before declaring a charge or discharge session active.

Stop intent is retained until an authoritative stop confirmation. The service keeps the active session and retries throttled stop requests instead of recording a completed trade or issuing a new forced action. A stop that failed on a dead RS485 link is retried no sooner than five minutes later, keeping the bus quiet, until the link check sees live telemetry again and restores the normal five-second cadence; the bridge is restarted only after five minutes of link-down and at most once per hour, and a restart does not trigger an immediate stop retry. Scheduled-session telemetry loss, and repeated solar P1 or battery telemetry failures, initiate that same fail-safe stop path.

### Data Persistence
- File-based JSON storage in `DATA_DIR`
- `trades.json` - trade history; the optional `telemetry_gap_s` field records seconds in which the RS485 link was detected down during the session, omitted when zero
- `automatic-cycle-commitment.json` - paired grid-cycle intent, persisted before charging; not proof that energy was purchased
- `retired-discharge-windows.json` - completed automatic discharge windows that must not be selected again after restart; markers older than the current local day are pruned
- `measured-efficiency.json` - completed measured AC-efficiency aggregates, not incomplete measurement windows
- Uses `decimal` library for monetary precision

Invalid retirement state blocks startup after a safe-stop attempt. Failed retirement writes block automatic control until persistence succeeds. Uncommitted inventory plans are rebuilt from fresh SOC and known tariffs; partial active-session energy is not persisted. See [Operations](operations.md#commitment-recovery) before repairing or removing state.

### Logging
- Structured JSON logs to stdout
- Configurable log level (debug/info/warn/error)

## HTTP API

### Endpoints

| Endpoint | Description |
|----------|-------------|
| `GET /health` | Liveness probe, returns "ok" |
| `GET /metrics` | Prometheus metrics |
| `GET /status` | Current state, reservation, commitment/pending-plan state, and full history (JSON) |

### Status Response

Illustrative subset of `/status` (trade details and other fields omitted). Accounting field semantics and Prometheus metric names are documented in [Operations](operations.md#accounting-metrics).

```json
{
  "current": {
    "state": "idle",
    "battery_available": true,
    "battery_soc": 75,
    "battery_power_w": 0,
    "current_price_eur_kwh": 0.0854,
    "current_price_known": true,
    "plan_pending": false,
    "plan_discharge_only": false,
    "commitment_type": "grid",
    "commitment_durable": true,
    "commitment_discharge_window_end": "2026-02-02T19:00:00+01:00",
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
        "opportunity_adjusted_pnl_eur": "0.0325",
        "cash_flow_unpriced_kwh": "0",
        "unpriced_kwh": "0",
        "unattributed_charge_kwh": "0"
      }
    ],
    "total_pnl_eur": "0.0325",
    "total_opportunity_adjusted_pnl_eur": "0.0325",
    "total_unattributed_charge_kwh": "0",
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
| Solar charge start | Solar charging state and starting SOC |
| Solar charge end | Battery, solar, and grid energy with known costs and explicit incomplete-value disclosure |
| Error | "Battery unreachable" |
| Daily summary (23:59) | P&L, charged/discharged kWh, solar kWh, cycles, cumulative P&L, and unpriced-energy disclosure when cash flow is incomplete |

### Commands (Inbound)

Commands are accepted only from the configured private `TELEGRAM_CHAT_ID`; group chats and other senders are ignored. At startup, the service registers the command menu for that private chat.

| Command | Response |
|---------|----------|
| `/status` | Current state, battery SOC, available/unavailable current price, next action, and complete or known-only cash flow |
| `/discharge` | Start manual discharge at `DISCHARGE_POWER_W` |
| `/discharge 800` | Start manual discharge at a chosen power from 800-2500 W |
| `/auto` | Stop manual discharge and return control to automatic trading and solar charging |

Manual discharge stops at the configured minimum SOC, when battery status or command refresh fails, or after two hours. It bypasses the automatic inventory-allowance gate but still debits the ledger and requires durable intent and confirmed-idle settlement. The completed discharge is included in trade history and P&L.

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

See [Configuration](configuration.md) for the canonical environment-variable reference, defaults, and validation requirements.

Planning uses `BATTERY_EFFICIENCY` for round-trip economics and `BATTERY_CHARGE_EFFICIENCY` for AC-to-stored input sizing. Export valuation is selected by `EXPORT_PRICE_MODE` and, in wholesale mode, the signed `EXPORT_FEE_EUR_PER_KWH`. `MAX_CYCLES_PER_DAY` limits new grid cycles over the known horizon; inventory-only sales are exempt.

## Scheduling

| Time | Action |
|------|--------|
| Every 1 sec | Solar tick: read P1 meter, manage solar charging (when enabled) |
| Every 1 min | Check battery, execute trades |
| Selected automatic discharge endpoint | Request idle, including partial-slot endpoints, independently of the minute tick |
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
│   ├── planner.go               # Joint stored-inventory and grid-cycle optimization
│   ├── charging_reservations.go # SOC-aware executable grid slices
│   ├── recorder.go              # Trade recording (decimal)
│   ├── charge_accounting.go     # Scheduled-charge source attribution and value metrics
│   └── interfaces.go            # BatteryController interface
├── clients/
│   ├── esphome/client.go        # ESPHome HTTP client (default)
│   ├── homewizard/              # HomeWizard P1 meter (solar surplus + mDNS discovery)
│   │   ├── client.go            # HTTP client for P1 data/device info
│   │   └── discover.go          # Opt-in discovery (mDNS + HTTP scan fallback)
│   ├── marstek/client.go        # Battery UDP (legacy, preserved)
│   ├── nordpool/client.go       # NordPool API
│   └── telegram/client.go       # Telegram bot
├── internal/config/config.go    # Configuration
├── docs/
│   ├── legacy-udp.md            # Legacy UDP client notes
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

## Safety and Accounting Boundaries

- The ESPHome backend has no battery-side command expiry. A process, host, network, or bridge failure can leave the last forced command active until the battery's BMS intervenes or control is restored.
- Startup and graceful shutdown attempt a confirmed stop, but abrupt termination cannot guarantee one. Container shutdown must allow at least 95 seconds.
- The HTTP API, ESPHome API, and HomeWizard local API have no authentication in this design and must remain on trusted networks. Status and metrics reveal household and financial data.
- The paired cycle for purchased grid energy is persisted before issuing a charge command and restored after restart. Partial active-session energy measurements are not persisted, so abrupt termination can still under-report a session.
- Export uses the configured export tariff, symmetric with the import rate by default. P&L is operational cash-flow estimation, not inventory-matched profit or revenue-grade metering.
- The repository does not provide the ESPHome firmware configuration or an independent hardware watchdog.

## Out of Scope (v1)

- Web UI dashboard
- Multiple battery support
- Dynamic rate adjustment
- Database persistence (PostgreSQL/SQLite)
