# Operations

Requirements, setup, and day-to-day operation of the service.

## Requirements

- Go 1.26.6 or newer, or Docker.
- A Marstek Venus E with a compatible ESPHome HTTP bridge on a trusted local network.
- ESPHome entities matching the names used in `clients/esphome/client.go`, including battery SOC/power sensors, forcible charge/discharge numbers, RS485 control mode, and forcible charge/discharge select.
- An `AC Power` sensor on the ESPHome bridge. It is required, not optional: trade and solar energy accounting and the solar P1 feedback compensation all read AC-side power, not only the round-trip efficiency sampler.
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

Edit `.env` before starting. `ESPHOME_URL` is required and intentionally has no default. Review import tariffs, `EXPORT_PRICE_MODE` and its signed `EXPORT_FEE_EUR_PER_KWH`, both battery efficiency settings, power, capacity, minimum SOC, timezone, and grid-cycle allowance. `BATTERY_CHARGE_EFFICIENCY` must be at least `BATTERY_EFFICIENCY`. See [Configuration](configuration.md) for the full reference.

### Upgrade Note

`MIN_PRICE_SPREAD` now means minimum expected profit after efficiency loss, not raw market-price spread. An unchanged value is therefore stricter than on older releases. Re-evaluate the setting before upgrading; for example, a 0.06 EUR/kWh raw spread can yield only 0.02 EUR/kWh after efficiency loss.

Run locally:

```bash
make test
make run
```

## Docker

```bash
make docker-build
make docker-run
```

`make docker-run` loads `.env`, persists `/app/data` in the `energy-trader-data` Docker volume, uses host networking for local-device discovery, and gives shutdown 95 seconds so the service can confirm a battery stop. Stop it with `docker stop energy-trader`; do not use `docker kill` during an active session.

### Data Migration

If upgrading from a version that stored state in the repository's `./data` bind mount, migrate it once before the first `make docker-run`:

```bash
docker stop -t 95 energy-trader  # if the old container is running
make docker-build
make docker-migrate-data
docker rm energy-trader        # remove the stopped old container, not its data
make docker-run
```

Wait for the old container to stop cleanly before migrating so trade history and Telegram state cannot change during the copy. The migration copies `./data` into `energy-trader-data`, sets ownership for the image's non-root user, cleans a partial copy if an operation fails, and refuses to overwrite a non-empty destination volume. Keep the original directory as a backup until `/status` confirms the expected history.

## HTTP API

With the default loopback binding:

```bash
curl http://127.0.0.1:8080/health
curl http://127.0.0.1:8080/status
curl http://127.0.0.1:8080/metrics
```

| Endpoint | Content |
|---|---|
| `GET /health` | Process liveness (`ok`) |
| `GET /metrics` | Prometheus battery SOC/state, measured efficiency, known cash flow, opportunity-adjusted P&L, and accounting completeness indicators |
| `GET /status` | Cached battery observations, measured efficiency, price availability, reservation, commitment/pending-plan state, next action, and full trade history |

The API is read-only. It has no authentication or TLS.

### Accounting Metrics

| Metric | Interpretation |
|---|---|
| `energy_trader_pnl_eur_total` | Known cumulative cash flow: priced discharge value minus priced grid cost |
| `energy_trader_cash_flow_unpriced_energy_kwh` | Grid charge and discharge energy excluded from known cash flow because its tariff was unavailable |
| `energy_trader_opportunity_adjusted_pnl_eur_total` | Known cash flow minus known signed forgone solar-export value; not counterfactual savings or inventory-matched profit |
| `energy_trader_unattributed_charge_energy_kwh` | Scheduled charge energy before the first successful P1 source observation |
| `energy_trader_opportunity_unpriced_energy_kwh` | Solar-attributed charge energy excluded from forgone-export valuation because its export tariff was unavailable |

These are gauges derived from recorded history, not monotonic counters. Read financial values together with the unpriced and unattributed energy gauges; a known-value total can be incomplete. Negative export tariffs produce negative opportunity cost, so subtracting that cost can increase the opportunity-adjusted figure. No metric is a utility bill or a guarantee of metered export.

`/status` exposes daily `pnl_eur`, `opportunity_adjusted_pnl_eur`, `cash_flow_unpriced_kwh`, `unpriced_kwh`, and `unattributed_charge_kwh` under `history.days`. History totals include `total_pnl_eur`, `total_opportunity_adjusted_pnl_eur`, and `total_unattributed_charge_kwh`. New scheduled-charge records use `charge_source_attribution` to distinguish source-split accounting from historical records; do not reinterpret old records as newly measured solar/grid splits.

With P1 disabled, scheduled charging retains the all-grid interpretation. With P1 enabled, energy before the first successful source observation is unattributed; after that, failed reads retain the last observed grid-import estimate. Source attribution is therefore an estimate across telemetry gaps, not continuous metering.

Measured efficiency is independent of these financial totals: `energy_trader_measured_efficiency_percent` is `NaN` until a valid measurement window completes, while `energy_trader_measured_efficiency_windows` and `energy_trader_rejected_efficiency_windows` expose accepted and rejected counts. See [Methodology](methodology.md#measured-efficiency).

## Telegram Commands

Commands are accepted only from the configured private `TELEGRAM_CHAT_ID`; group chats and other senders are ignored.

| Command | Behavior |
|---|---|
| `/status` | Show battery, price, trading state, and next action |
| `/discharge` | Start manual discharge at `DISCHARGE_POWER_W` |
| `/discharge 800` | Start manual discharge at a selected 800-2500 W |
| `/auto` | Stop manual discharge and return to automatic control |

Manual discharge stops at minimum SOC, on telemetry/control failure, or after two hours. Telegram is a convenience control path, not an independent safety channel.

## RS485 Diagnostics

If the ESPHome bridge exposes a restart button, set `ESPHOME_RESTART_BUTTON` to its web-server name, for example `Restart`. Verify its read endpoint while idle before allowing automatic restart. A manual restart-button POST may require `Content-Length: 0`.

The diagnostic script records ESPHome's `/events` stream and reconnects after bridge restarts:

```bash
LOGTAIL_TZ=Europe/Amsterdam scripts/esp32-logtail.sh \
  http://battery-bridge.local ./esp32-events.log
```

The script does not rotate its output. Use `logrotate` or another bounded-retention mechanism for unattended capture.

## Commitment Recovery

An invalid or corrupt `automatic-cycle-commitment.json` makes startup attempt to stop the battery and refuse to trade. A system clock more than 72 hours behind the commitment can also trigger this fail-closed check, so verify time synchronization before treating the file as invalid. If the connection or stop command also fails, a prior forced operation may remain active. Inspect the reported file and confirm the battery is physically idle before removing it from `DATA_DIR`; restarting then rebuilds the plan from current prices.

Changing `EXPORT_PRICE_MODE` while a commitment is persisted retains only the discharge obligation: the stored windows were priced under the previous mode, so the restored cycle is never re-authorized for further grid charging. Changing `BATTERY_EFFICIENCY` re-evaluates the restored cycle against the new expected-profit floor at startup, which can likewise demote it to discharge-only.

Uncommitted inventory sale windows are not durable commitments. After restart, the service confirms idle, restores the measured inventory allowance, and obtains fresh SOC and known tariffs before selecting one again. SOC cannot recreate spent or unmeasured inventory. This does not block qualified solar capture. A currently active inventory sale can account missing tariffs as unpriced but stops on a confirmed nonpositive export tariff.

If deletion of an expired or completed commitment fails, the service remains running but pins planning and retries fail-closed. `/status` reports the grid commitment, its durability, the staged-plan flag, and `automatic cycle commitment cleanup pending`; logs and Telegram report the filesystem error. Restore write access to `DATA_DIR` rather than deleting a live commitment blindly.

### Completed Discharge Recovery

`retired-discharge-windows.json` records completed automatic discharge windows so SOC rebound or restart cannot select them again. This is separate from an uncommitted inventory plan, which is not persisted. Old retirement markers are pruned before the current local day; retain the current markers when backing up or migrating `DATA_DIR`.

An unreadable, malformed, or invalid retirement file makes startup attempt a safe stop and refuse to trade. As with a commitment-file failure, failed battery communication can leave a prior forced operation active. Stop the service and confirm physical idle before repairing state. Preserve the original file, check filesystem permissions, and restore a valid backup where available; do not replace it with an empty list merely to bypass the error, because that removes completed-window protection.

If saving or pruning retirement markers fails at runtime, `/status` reports `completed sale persistence pending; automatic control blocked`. Automatic grid starts and discharge selection remain blocked while persistence is dirty. Restore write access to `DATA_DIR` and allow the normal retry path to clear the condition; do not restart repeatedly or delete markers to force a new plan.

### Measured Inventory Recovery

`inventory-ledger.json` stores the remaining measured DC allowance and an in-flight discharge marker. Preserve it with the other files in `DATA_DIR`. Startup first confirms physical idle. A missing file (including migration), an in-flight marker after an interrupted discharge, or changed capacity/minimum SOC initializes zero trusted inventory. Only subsequently measured service-controlled charging replenishes it; an unchanged or rebounding SOC cannot authorize another sale.

Malformed or unreadable inventory blocks startup. A publication failure blocks discharge; failed stop settlement retains the session and retries without authorizing another command. Restore filesystem access rather than inserting an SOC-derived balance. `/status` exposes `inventory_available_dc_kwh`, `inventory_persistence_blocked`, and `inventory_discharge_in_flight`. Manual discharge remains an explicit override but is debited and protected by the same durable marker.

### Automatic Control Deadlines

Automatic control does not start or refresh in the final minute of its window. A separate control-loop timer requests idle at the selected discharge endpoint, including a partial tariff slot, without needing a successful battery or tariff read. Failed stops retain ownership and use the throttled retry path. This is software scheduling, not a hardware command expiry or a guarantee that network I/O cannot delay physical stopping.
