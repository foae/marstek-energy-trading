# Operations

Requirements, setup, and day-to-day operation of the service.

## Requirements

- Go 1.26.6 or newer, or Docker.
- A Marstek Venus E with a compatible ESPHome HTTP bridge on a trusted local network.
- ESPHome entities matching the names used in `clients/esphome/client.go`, including battery SOC/power sensors, forcible charge/discharge numbers, RS485 control mode, and forcible charge/discharge select.
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

Edit `.env` before starting. `ESPHOME_URL` is required and intentionally has no default. Review the tariff, power, capacity, minimum-SOC, timezone, and cycle settings for your installation. See [Configuration](configuration.md) for the full reference.

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
| `GET /metrics` | Prometheus battery SOC, state, known cumulative cash flow, and cumulative unpriced cash-flow energy |
| `GET /status` | Cached battery state, current price, reservation, commitment/pending-plan state, next action, and full trade history |

The API is read-only. It has no authentication or TLS.

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

If deletion of an expired or completed commitment fails, the service remains running but pins planning and retries fail-closed. `/status` reports the grid commitment, its durability, the staged-plan flag, and `automatic cycle commitment cleanup pending`; logs and Telegram report the filesystem error. Restore write access to `DATA_DIR` rather than deleting a live commitment blindly.
