# Energy Trader PRD

Product Requirements Document for the Marstek Energy Trading Bot.

## Overview

A Go service that performs energy price arbitrage using a Marstek Venus E battery. The service fetches NordPool day-ahead prices, builds a global plan of profitable charge/discharge cycles, and controls the battery via an ESPHome HTTP REST API. A HomeWizard P1 meter can direct solar surplus into the battery only when doing so is no more expensive than its forgone export value relative to reserved grid energy; it is not free profit.

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
- **Purpose**: Detects solar surplus (grid export) for battery charging; captured solar is valued at the configured all-in export opportunity rate.
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
- **Rates**: EUR/MWh wholesale data converted to one configured all-in EUR/kWh rate: `(NordPool + energy tax) × (1 + VAT) + supplier fee`. That same rate prices grid import, discharge/export valuation, and solar opportunity cost; no separate feed-in tariff is modeled.

## Trading Strategy

### Global cycle planning

1. **Calculate usable capacity** accounting for min SOC protection:
   ```
   usable_kWh = capacity_kWh * (1 - min_soc)
   ```
   Example: 5.12 kWh with 11% min SOC = 4.56 kWh usable.

2. **Calculate contiguous window sizes** from usable capacity and configured charge/discharge power:
   ```
   window_slots = ceil(usable_kWh / power_kW * 4)
   ```
   Example: 4.56 kWh at 2500W = 8 slots (2 hours).

3. **Evaluate all chronological candidate pairs.** Each charge window must end before its discharge window begins. Expected profit per input kWh is `discharge_avg * efficiency - charge_avg`. A pair is eligible only when that result is positive and at least `MIN_PRICE_SPREAD`.

4. **Select the global plan.** Dynamic programming maximizes the summed expected profit of up to `MAX_CYCLES_PER_DAY` non-overlapping chronological pairs. It is not a bottom/top-quartile heuristic and does not greedily select one cycle before considering later cycles.

### SOC-aware grid reservations

For the next unfinished charge cycle, the service derives a deadline from its charge-window end and calculates the grid input needed to reach 100% from current SOC, including a conservative charging-efficiency estimate. It considers eligible 15-minute price slices from the known today and tomorrow tariff sets after the prior planned discharge and through that deadline, then reserves the cheapest slices first. It forecasts no future solar: any solar already reflected in measured SOC reduces the reservation. After a grid charge has run for 30 seconds, any lower observed charging power becomes the deliverability limit. A grid slice is excluded when its individual price would reduce expected profit against the paired discharge average below `MIN_PRICE_SPREAD`. Automatic control does not start or refresh in the final minute of a reserved window, reserving a bounded interval for ESPHome confirmation and battery-power verification before the tariff boundary. When delivery capacity, time, or that economic bound prevents a full charge, the service reserves an eligible best-effort subset and marks the reservation infeasible.

For a feasible reservation, solar begins only if its current all-in export opportunity cost is no greater than the marginal (highest-priced) selected grid slice. Otherwise the service exports the expensive solar now and retains the cheaper grid reservation. If economic exclusions cause the reservation shortfall, solar must still satisfy the same per-slice expected-profit ceiling; that ceiling remains active between the charge deadline and paired discharge. Infeasibility caused by time or taper even with all slices available permits solar capture regardless. With no deadline and no pending paired discharge, solar is captured. A feasible or economics-limited reservation with no known current tariff does not start solar.

### Execution accounting and discharge

Executed charge and discharge energy is integrated from measured battery-power samples and priced across retained 15-minute rate slots. Energy without an applicable retained rate is explicitly unpriced. Solar sessions separately record estimated grid input/cost and the all-in opportunity cost of solar not exported. Daily and total P&L are cash flow (priced discharge revenue less priced grid cost), not inventory-matched trading profit; solar opportunity cost is reported separately and is not deducted from that P&L.

Scheduled discharge starts in its planned window when SOC is above its configured minimum. `lastChargePrice` remains informational logging only and does not gate discharge.

### Solar Self-Consumption

When a HomeWizard P1 meter is configured, the service detects grid export (solar surplus) and charges the battery, bridging brief dips at low power:

1. **Detection**: P1 meter is polled every 1 second. Negative `active_power_w` = exporting to grid = solar surplus.
2. **Start confirmation**: Requires 30 seconds of sustained surplus above `SOLAR_MIN_SURPLUS_W` (default: 100W). Failed readings or telemetry gaps longer than two seconds reset qualification; EMA smoothing uses elapsed time.
3. **Charging**: Battery charges at the detected surplus power (clamped to `CHARGE_POWER_W`). Power is dynamically adjusted with a 50W deadband to avoid flapping.
4. **P1 feedback compensation**: During charging, `effectiveSurplus = measuredSurplus + measuredBatteryChargePower`; an EMA (alpha 0.05) smooths the result. Measured rather than requested battery power avoids treating an unachieved command as available surplus.
5. **Ramp-up cooldown**: After starting or adjusting charge power, a 5-second cooldown prevents re-adjustment while the battery ramps to the new target (~3s). This avoids a positive feedback spiral where transient over-estimation of effective surplus causes the target power to spiral upward.
6. **Low-surplus, economic choice, and failures**: EMA below `max(SOLAR_MIN_SURPLUS_W / 4, 75W)` starts a 60-second grace requesting 75W; recovery immediately clears it. Grace expiry stops charging. Surplus-loss sessions under ten minutes get a five-minute cooldown; three consecutive marginal sessions get fifteen minutes. Longer sessions and legitimate stops reset the streak and use sixty seconds. Battery-full, active-reservation, and discharge-window checks precede P1 reads. For a feasible reservation, solar starts only when the current export opportunity cost is no greater than the marginal reserved grid price; otherwise expensive solar is exported and cheaper grid energy remains reserved. An economics-limited reservation still applies the paired cycle's per-slice price ceiling to solar, including after the charge deadline while its discharge remains pending; infeasibility caused only by time or taper permits solar capture regardless. Failed adjustments and repeated telemetry failure request a confirmed stop; an unconfirmed stop retains the session and is retried.
7. **Scheduled priority**: An active grid reservation or discharge window stops solar charging before its scheduled action begins. Solar does not start during either, then can resume once the window ends if the economic rule permits it.
8. **Recording**: `solar_charge` records measured battery energy, separate estimated grid energy/cost, and solar opportunity cost. Grid input is `min(measuredBatteryChargePower, max(netGridImport, 0))`, integrated between samples. Solar energy is the remainder. Known rate slots price grid cost and the forgone-export opportunity cost; unavailable rates are explicitly unpriced. Legacy records without split fields remain all-solar.

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

Stop intent is retained until an authoritative stop confirmation. The service keeps the active session and retries throttled stop requests instead of recording a completed trade or issuing a new forced action. Scheduled-session telemetry loss, and repeated solar P1 or battery telemetry failures, initiate that same fail-safe stop path.

### Data Persistence
- File-based JSON storage in `DATA_DIR`
- `trades.json` - trade history
- `automatic-cycle-commitment.json` - the discharge pairing for grid energy already purchased
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
| `GET /status` | Current state, reservation, commitment/pending-plan state, and full history (JSON) |

### Status Response

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

Manual discharge stops at the configured minimum SOC, when battery status or command refresh fails, or after two hours. The completed discharge is included in trade history and P&L.

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
| `HTTP_LISTEN_ADDR` | `127.0.0.1:8080` | HTTP server address; API has no authentication or TLS |
| `DATA_DIR` | `./data` | Data storage directory |
| `TZ` | `Europe/Amsterdam` | Timezone |
| `NORDPOOL_AREA` | `NL` | Price area code |
| `NORDPOOL_CURRENCY` | `EUR` | Currency |
| `MIN_PRICE_SPREAD` | `0.05` | Minimum expected profit after efficiency loss (EUR/kWh; historical name) |
| `BATTERY_EFFICIENCY` | `0.90` | Round-trip efficiency |
| `BATTERY_CAPACITY_KWH` | `5.12` | Battery capacity (kWh) |
| `BATTERY_MIN_SOC` | `0.11` | Minimum SOC (0.0-1.0) |
| `MAX_CYCLES_PER_DAY` | `2` | Max cycles selected over the loaded planning horizon |
| `ESPHOME_URL` | required | ESPHome device URL |
| `BATTERY_UDP_ADDR` | - | Unwired legacy library configuration |
| `CHARGE_POWER_W` | `2500` | Charge power (watts) |
| `DISCHARGE_POWER_W` | `2500` | Discharge power (watts) |
| `PASSIVE_MODE_TIMEOUT_S` | `300` | Service refresh basis; not a battery-side command expiry |
| `HOMEWIZARD_P1_URL` | - | Empty disables P1; URL selects a meter; `auto` opts into discovery and LAN scanning |
| `SOLAR_MIN_SURPLUS_W` | `100` | Min surplus watts to start solar charging |
| `TELEGRAM_BOT_TOKEN` | - | Telegram bot token (enables notifications and command registration) |
| `TELEGRAM_CHAT_ID` | - | Private Telegram chat allowed to issue commands |

## Scheduling

| Time | Action |
|------|--------|
| Every 1 sec | Solar tick: read P1 meter, manage solar charging (when enabled) |
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
- Import and export use one symmetric configured tariff. P&L is operational cash-flow estimation, not inventory-matched profit or revenue-grade metering.
- The repository does not provide the ESPHome firmware configuration or an independent hardware watchdog.

## Out of Scope (v1)

- Web UI dashboard
- Multiple battery support
- Dynamic rate adjustment
- Database persistence (PostgreSQL/SQLite)
