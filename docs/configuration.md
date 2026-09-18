# Configuration

All settings are environment variables, typically provided through a `.env` file. See [.env.example](../.env.example) for a copyable configuration.

| Variable | Default | Description |
|---|---:|---|
| `SERVICE_NAME` | `energy-trader` | Service name used in logs and notifications |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |
| `HTTP_LISTEN_ADDR` | `127.0.0.1:8080` | HTTP listen address; use `:8080` only behind a trusted network boundary |
| `DATA_DIR` | `./data` | Required non-empty trade history, automatic cycle commitment, and Telegram update-offset directory |
| `TZ` | `Europe/Amsterdam` | Scheduling and daily-accounting timezone |
| `NORDPOOL_AREA` | `NL` | NordPool bidding area |
| `NORDPOOL_CURRENCY` | `EUR` | Must remain EUR for all-in pricing |
| `ENERGY_TAX_EUR_PER_KWH` | `0.09161` | Example Dutch 2026 energy tax; verify before use |
| `VAT_RATE` | `0.21` | VAT applied to wholesale price and energy tax |
| `SUPPLIER_FEE_EUR_PER_KWH` | `0.02` | VAT-inclusive supplier fee |
| `EXPORT_PRICE_MODE` | `symmetric` | `symmetric` values export at the all-in import rate; `wholesale` values it at the wholesale price plus `EXPORT_FEE_EUR_PER_KWH` |
| `EXPORT_FEE_EUR_PER_KWH` | `0` | Signed per-kWh adjustment to the export rate, applied only in `wholesale` mode; negative models a feed-in cost |
| `MIN_PRICE_SPREAD` | `0.05` | Minimum expected profit after efficiency loss in EUR/kWh (historical name) |
| `BATTERY_EFFICIENCY` | `0.90` | Round-trip efficiency in `(0, 1]` |
| `BATTERY_CHARGE_EFFICIENCY` | `0.95` | Charging (AC input to stored) efficiency in `(0, 1]`, at least `BATTERY_EFFICIENCY`; the discharge efficiency is `BATTERY_EFFICIENCY / BATTERY_CHARGE_EFFICIENCY` |
| `BATTERY_CAPACITY_KWH` | `5.12` | Nominal battery capacity |
| `BATTERY_MIN_SOC` | `0.11` | Minimum SOC fraction |
| `MAX_CYCLES_PER_DAY` | `2` | Maximum new grid cycles selected over the loaded planning horizon; an inventory-only sale is exempt from this allowance |
| `ESPHOME_URL` | required | Absolute URL of the ESPHome bridge |
| `ESPHOME_RESTART_BUTTON` | empty | Optional restart-button name exposed by ESPHome |
| `CHARGE_POWER_W` | `2500` | Charge target, 75-2500 W |
| `CHARGE_DEFER_TOLERANCE_EUR_PER_KWH` | `0.01` | Latest slices priced at most this much above the cheapest allocation's average are preferred, so solar can fill the battery first; the earliest reserved piece starts one minute early because the control loop ticks once a minute. While charging, the slice containing the current time is kept when it is within this tolerance or when finishing it costs under one cent. Only active when a P1 meter is configured; without one the cheapest slices are reserved as before, under the same running-slice rule |
| `CHARGE_PLANNING_DERATE` | `0.90` | Reservation and plan sizing assume `CHARGE_POWER_W` times this factor, absorbing charge taper |
| `INVENTORY_SALE_MIN_GAIN_EUR` | `0.02` | An inventory sale must beat the no-sale plan by at least this much |
| `DISCHARGE_POWER_W` | `2500` | Discharge target, 800-2500 W |
| `PASSIVE_MODE_TIMEOUT_S` | `300` | Refresh interval basis; not a hardware safety timeout |
| `HOMEWIZARD_P1_URL` | empty | Empty disables P1; URL selects a meter; `auto` opts into discovery and LAN scanning |
| `SOLAR_MIN_SURPLUS_W` | `100` | Sustained raw surplus needed to qualify solar charging |
| `TELEGRAM_BOT_TOKEN` | empty | Optional bot token |
| `TELEGRAM_CHAT_ID` | empty | Private chat authorized for notifications and commands |

## Pricing Caveat

NordPool EUR/MWh prices are converted to one all-in EUR/kWh rate:

```text
(wholesale price + energy tax) * (1 + VAT) + supplier fee
```

By default (`EXPORT_PRICE_MODE=symmetric`) that same rate values import, discharge/export, and solar export opportunity cost. With `EXPORT_PRICE_MODE=wholesale`, discharge, export, and forgone solar export are instead valued at `wholesale price + EXPORT_FEE_EUR_PER_KWH` (no energy tax, no VAT), which approximates Dutch pricing after net metering ends in 2027. In that mode discharge is valued entirely at the export rate; this is conservative, because it ignores the share of discharge that offsets house load and is therefore worth the import rate. `NORDPOOL_CURRENCY` must remain EUR for all-in pricing.

The default tax, VAT, fee, area, timezone, battery capacity, and efficiency values are examples for one Dutch setup and will become stale. Verify them for your installation before use.

Despite the environment variable's historical name, `MIN_PRICE_SPREAD` is an efficiency-adjusted expected-profit threshold in EUR/kWh, not a raw price-spread threshold. See [Methodology](methodology.md) for how it is applied.
Setting it to zero still rejects exact break-even cycles because expected profit must be strictly positive.

`MIN_PRICE_SPREAD` applies to grid purchases, not to qualified solar capture or an inventory-only sale. The latter needs a known positive export price but has no arbitrary wear-cost floor. The planner does not forecast solar or tariffs beyond its known horizon.
