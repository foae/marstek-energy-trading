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
| `MIN_PRICE_SPREAD` | `0.05` | Minimum expected profit after efficiency loss in EUR/kWh (historical name) |
| `BATTERY_EFFICIENCY` | `0.90` | Round-trip efficiency in `(0, 1]` |
| `BATTERY_CAPACITY_KWH` | `5.12` | Nominal battery capacity |
| `BATTERY_MIN_SOC` | `0.11` | Minimum SOC fraction |
| `MAX_CYCLES_PER_DAY` | `2` | Maximum new cycles selected over the loaded planning horizon; stored-energy discharge recovery does not consume this allowance |
| `ESPHOME_URL` | required | Absolute URL of the ESPHome bridge |
| `ESPHOME_RESTART_BUTTON` | empty | Optional restart-button name exposed by ESPHome |
| `CHARGE_POWER_W` | `2500` | Charge target, 75-2500 W |
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

That same configured rate values import, discharge/export, and solar export opportunity cost. This assumes symmetric import/export value and does not model a separate feed-in tariff; installations with a separate feed-in tariff need code changes. `NORDPOOL_CURRENCY` must remain EUR for all-in pricing.

The default tax, VAT, fee, area, timezone, battery capacity, and efficiency values are examples for one Dutch setup and will become stale. Verify them for your installation before use.

Despite the environment variable's historical name, `MIN_PRICE_SPREAD` is an efficiency-adjusted expected-profit threshold in EUR/kWh, not a raw price-spread threshold. See [Methodology](methodology.md) for how it is applied.
Setting it to zero still rejects exact break-even cycles because expected profit must be strictly positive.
