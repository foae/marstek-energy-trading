# Methodology

Planning, pricing and accounting, and solar-charging rules. The detailed state machine and accounting rules are in [the PRD](energy-trader-prd.md).

## Planning

The analyzer evaluates contiguous discharge windows paired with the cheapest executable, individually eligible grid charge slices before each discharge. Those charge slices need not be contiguous. Dynamic programming selects up to `MAX_CYCLES_PER_DAY` non-overlapping new grid pairs that maximize their contribution to the joint total-EUR plan over the loaded known tariff horizon. Despite the environment variable's historical name, this is a horizon-wide grid-cycle planning limit, not a persisted calendar-day execution counter. Ended, retired, and final-minute windows are excluded before selection.

The service maintains a durable, measured-only DC inventory allowance. Only valid measured DC charging samples during service-controlled grid or solar charging add energy; credit tolerates sample gaps up to 60 seconds, and a longer gap earns nothing. SOC lowers this allowance. It can raise it back up to the SOC cap only while a sale is in flight, the link check reports the link live, and SOC has changed within the last 10 minutes, which also recomputes the inventory deadline within the sale window; a SOC frozen by a dead link cannot replenish it. The planner compares total EUR alternatives that spend the trusted allowance once: hold it, let it offset the first grid purchase, or sell it in one contiguous discharge window and then select chronological grid cycles after that sale. Observed energy beyond the trusted allowance remains quarantined: it reduces the space available for purchases but is excluded from discharge revenue. Reservations for following cycles cannot begin before the inventory sale ends. Inventory-only sales are exempt from the grid-cycle allowance; grid cycles remain subject to it.

An inventory sale uses a contiguous future window with known positive export prices, bounded by the observed energy, discharge power, efficiency, and available tariff intervals. It can end inside a tariff slot or sell only part of inventory when that improves the combined sale-and-refill value, or when the available horizon limits delivery. A candidate must span at least one minute and deliver at least 5% of a full delivery, about 0.19 kWh AC on a 5.12 kWh battery and roughly five minutes at 2200 W, so sliver sales below about 17% integer SOC are not selected. Endpoint selection includes executable charging-allocation transitions and retired-window boundaries. The planner values actual energy and tariff intervals rather than an unweighted average. Equal total-EUR choices prefer fewer control sessions and then earlier choices. This is the best contiguous sale within known prices, not globally optimal arbitrary-slot dispatch or a prediction of future tariffs. It does not invent a battery-wear floor, solar forecast, or future price; nonpositive or unknown export intervals retain unsold inventory.

A grid pair's expected profit per input kWh is:

```text
discharge_average * BATTERY_EFFICIENCY - charge_average
```

A grid pair is eligible only when that result is positive and at least `MIN_PRICE_SPREAD`. Despite the environment variable's historical name, it is an efficiency-adjusted expected-profit threshold, not a raw price-spread threshold.

The analyzer sizes grid purchases from the remaining DC shortfall to 100% divided by `BATTERY_CHARGE_EFFICIENCY`; initial stored energy therefore reduces the first grid purchase. A full discharge covers usable DC capacity times `BATTERY_EFFICIENCY` divided by `BATTERY_CHARGE_EFFICIENCY`. Both are AC-side energies divided by configured power to obtain duration. Execution recomputes the input needed from current SOC, reserves the cheapest remaining executable 15-minute slices before the charge deadline, and lowers assumed delivery capacity when measured charging power tapers. Every selected grid slice must preserve the paired cycle's `MIN_PRICE_SPREAD` floor.

A grid charge persists its paired automatic cycle commitment before controlling the battery, then revalidates the same cycle and limits control to the active reserved interval. The commitment survives a restart without authorizing a late write or a different cycle. It records intent, not proof that the charge executed or its partial active-session energy. A restored cycle that no longer meets the configured expected-profit floor remains eligible for its conservative discharge obligation but cannot resume grid charging.

An active inventory sale continues through temporarily missing tariff samples, accounting that energy as unpriced; it stops when a confirmed current export tariff is nonpositive. This continuity rule is distinct from durable grid commitments, but both kinds of automatic sale require trusted inventory. Startup confirms idle before restoring the inventory ledger. Missing, interrupted, or configuration-mismatched inventory starts at zero, never reconstructed from SOC; corrupt inventory blocks startup. Unstarted sale windows are rebuilt from the remaining trusted allowance and known prices. Failed commitment-file cleanup remains fail-closed.

Before any discharge command, including manual discharge, the service durably marks inventory in flight. Debit begins before the attempted command and continues through confirmed idle, using the greater of measured AC power divided by the discharge-side efficiency (round-trip efficiency over charge efficiency) and observed DC discharge power. Automatic control stops at the earlier of the tariff endpoint and the inventory deadline, reserving 45 seconds for normal stop latency. Higher observed DC demand can shorten that deadline, never extend it. Failed stops retain the existing throttled retry path; failed settlement blocks further control. Manual override bypasses automatic allowance gating but still debits and persists the ledger. No software margin guarantees stopping when the bridge is unreachable.

An active grid cycle or inventory sale retains its current control plan while refreshed plans are staged. Historical discharge recovery and solar-pair retention are not planning paths.

## Pricing And Accounting

NordPool EUR/MWh prices are converted to one all-in EUR/kWh rate:

```text
(wholesale price + energy tax) * (1 + VAT) + supplier fee
```

Exported energy is valued by `EXPORT_PRICE_MODE`. In the default `symmetric` mode that same all-in rate values import, discharge/export, and solar export opportunity cost. In `wholesale` mode, discharge/export and the forgone solar export opportunity cost are instead valued at `wholesale price + EXPORT_FEE_EUR_PER_KWH` (no energy tax, no VAT), which approximates Dutch pricing after net metering ends in 2027; grid import keeps the all-in rate. Discharge is then valued entirely at the export rate, which is conservative: it ignores the share of discharge that offsets house load and is therefore worth the import rate.

NordPool responses are validated against CET/CEST market-day boundaries, then assembled into the configured timezone's local calendar days. A local day can span adjacent market publications; until the next publication, only a contiguous known prefix is used. Incomplete calendars are refreshed every 15 minutes, including after midnight promotion, without erasing previously known coverage.

Energy is integrated from measured AC-power samples, so cash flow includes inverter conversion losses. Time in which the in-session link check reports the RS485 link down books zero energy rather than integrating the bridge's last cached reading, with energy settled at that reading and the affected seconds recorded on the trade as `telemetry_gap_s`; the one-second scheduled-charge sampler and solar charging accounting use the same gate. During the up to two minutes before the link check can declare a freeze, a SOC that stopped moving still counts as live, so an in-flight sale can over-run its budget by at most that detection window. An allowance raised by a live SOC during a sale is persisted at settlement: energy the battery demonstrably held and delivered is no longer quarantined afterwards. New split scheduled-charge records carry an explicit source-attribution marker and preserve separately attributed solar and grid portions; their grid cost and solar forgone-export opportunity cost are priced independently, with unavailable prices explicitly unpriced. Records without that marker retain their legacy interpretation. Cross-midnight records retain exact per-day energy, tariff value, and unpriced-energy allocations; historical aggregate records without those allocations are split proportionally. P&L is cash flow, calculated as priced discharge value minus priced grid cost. A separate opportunity-cost-adjusted metric deducts the priced solar opportunity cost; neither metric is inventory-matched profit. Both are incomplete when their required energy cannot be priced. Telegram labels partial daily and cumulative figures as known values and reports the unpriced quantity. The files and metrics are operational estimates, not billing records.

Serialized session bounds are rounded outward to whole seconds so both sides of a fractional-second midnight crossing remain representable. Energy and tariff allocations still use the exact measured interval, not the rounded duration.

## Measured Efficiency

Telegram plans, daily summaries and status messages report measured operational AC round-trip efficiency, not the configured planning assumption. Logs, `/status` (`measured_efficiency`) and Prometheus (`energy_trader_measured_efficiency_percent`, with accepted/rejected window counts) expose the same aggregate. Until a valid window completes, the percentage is unavailable (`null` in JSON, `NaN` in Prometheus).

An independent read-only worker samples ESPHome SOC and AC power every five seconds, including solar charging and standby. It integrates AC input and output between opposite crossings of the same integer-SOC boundary, approximating each crossing at the sample midpoint. A window must rise at least 20 SOC points and consume at least 0.5 kWh before returning to its starting boundary. The reported percentage is total accepted AC output divided by total accepted AC input, multiplied by 100; windows are energy-weighted, not averaged percentages.

Failed reads, detected stale telemetry, gaps longer than 30 seconds, skipped SOC levels and backwards timestamps discard incomplete windows. Windows exceeding 72 hours or producing more output than input are rejected. Completed aggregates persist atomically in `measured-efficiency.json`; incomplete windows never bridge a restart. This remains a sampled operational estimate, not calibrated metering: integer SOC, sensor publication delay and undetected telemetry faults limit accuracy.

`BATTERY_EFFICIENCY` remains the separate configured planning assumption and is not automatically adjusted. Trade-energy accounting integrates ESPHome's AC-power measurement, so cash-flow energy estimates include AC conversion losses. Solar surplus compensation also uses measured AC charge power: the P1 meter sees the AC draw, not a DC battery estimate. Battery-power verification remains a separate measured-control check. Trade records are not used to calculate this AC efficiency, and records carrying the legacy `measured_battery_power` basis remain DC-based.

## Solar Charging

Solar charging is enabled only when `HOMEWIZARD_P1_URL` is an explicit meter URL or `auto`.

- A session starts after 30 seconds of sustained raw surplus above `SOLAR_MIN_SURPLUS_W`.
- During charging, effective surplus compensates for the AC-coupled feedback loop: `measured surplus + measured AC charge power`, the draw the P1 meter actually sees.
- An elapsed-time EMA smooths the target, with a five-second settling period after power changes.
- Surplus below `max(75 W, SOLAR_MIN_SURPLUS_W / 4)` enters a 60-second grace period at 75 W before stopping.
- Adaptive cooldowns reduce short-session cycling.
- Solar stops at 99% SOC and can qualify again at 97%; a grid reservation may charge to 100%.
- Outside safety/fault handling, manual override, selected or active automatic discharge, and a current grid reservation, qualified solar capture has no tariff, forecast, reservation-economics, or grid-cycle-profit veto. A tariff change or missing tariff therefore does not stop an active solar session.
