# Methodology

Planning, pricing and accounting, and solar-charging rules. The detailed state machine and accounting rules are in [the PRD](energy-trader-prd.md).

## Planning

The analyzer evaluates contiguous charge windows followed by valid contiguous discharge windows. Dynamic programming selects up to `MAX_CYCLES_PER_DAY` non-overlapping new pairs that maximize summed expected profit over the loaded planning horizon. Despite the environment variable's historical name, this is a horizon-wide planning limit, not a persisted calendar-day execution counter. Ended, retired, and final-minute windows are excluded before capped selection.

Recovery of stored energy is separate from new-cycle selection. Observed battery energy above minimum SOC, or an active solar session, can recover a historical pair's upcoming discharge without reserving its expired grid charge. This includes restarting partway through an uncommitted discharge. Recovery must finish before the first newly selected charge window, so it cannot crowd out new purchases or overlap them. Persisted grid commitments and live solar retention remain authoritative.

A pair's expected profit per input kWh is:

```text
discharge_average * BATTERY_EFFICIENCY - charge_average
```

A pair is eligible only when that result is positive and at least `MIN_PRICE_SPREAD`. Despite the environment variable's historical name, it is an efficiency-adjusted expected-profit threshold, not a raw price-spread threshold.

The analyzer uses fixed windows sized from usable capacity and configured power. Execution is more adaptive: it computes the grid input needed to reach 100% from current SOC, reserves the cheapest remaining 15-minute slices before the next charge deadline, and lowers assumed delivery capacity when measured charging power tapers. It excludes any grid slice whose price would reduce expected profit against the paired discharge average below `MIN_PRICE_SPREAD`; this can leave the target SOC infeasible. Automatic control does not start or refresh inside the final minute of a reserved window because ESPHome confirmation and battery-power verification need enough time to finish before the tariff boundary. Solar captured for an economics-limited reservation must satisfy that same price ceiling, which remains active between the charge deadline and paired discharge. It does not forecast future solar.

A grid charge persists its paired automatic cycle commitment before controlling the battery, then revalidates the same cycle and limits the control operation to the active reserved interval. The commitment therefore survives a restart without authorizing a late write or a different cycle. However, the service does not persist proof that the charge actually executed, nor partial active-session energy: after a restart it can restore a same-day discharge window without proof that the paired charge ran. A restored cycle that no longer meets the configured expected-profit floor remains eligible for its conservative discharge obligation but cannot resume grid charging.

A retained discharge remains executable when its current tariff sample is unavailable; resulting energy is accounted as unpriced instead of silently receiving the planned window price. Once a cycle completes, failed commitment-file cleanup remains fail-closed and cannot authorize another discharge while cleanup retries.

A confirmed solar charge admitted against a planned cycle retains that cycle in live state through its paired discharge. This solar pairing is not persisted across a restart.

An active grid or solar-paired automatic cycle keeps its current plan while refreshed plans are staged.

## Pricing And Accounting

NordPool EUR/MWh prices are converted to one all-in EUR/kWh rate:

```text
(wholesale price + energy tax) * (1 + VAT) + supplier fee
```

That same configured rate values import, discharge/export, and solar export opportunity cost. This assumes symmetric import/export value and does not model a separate feed-in tariff.

NordPool responses are validated against CET/CEST market-day boundaries, then assembled into the configured timezone's local calendar days. A local day can span adjacent market publications; until the next publication, only a contiguous known prefix is used. Incomplete calendars are refreshed every 15 minutes, including after midnight promotion, without erasing previously known coverage.

Energy is integrated from measured battery-power samples. New cross-midnight records retain exact per-day energy, tariff value, and unpriced-energy allocations; historical aggregate records without those allocations are split proportionally. P&L is cash flow, calculated as priced discharge value minus priced grid cost. It is not inventory-matched profit, does not deduct solar opportunity cost, and is incomplete when grid-charge or discharge energy cannot be priced. Telegram labels partial daily and cumulative figures as known cash flow and reports the unpriced quantity. The files and metrics are operational estimates, not revenue-grade metering.

Serialized session bounds are rounded outward to whole seconds so both sides of a fractional-second midnight crossing remain representable. Energy and tariff allocations still use the exact measured interval, not the rounded duration.

## Solar Charging

Solar charging is enabled only when `HOMEWIZARD_P1_URL` is an explicit meter URL or `auto`.

- A session starts after 30 seconds of sustained raw surplus above `SOLAR_MIN_SURPLUS_W`.
- During charging, effective surplus compensates for the AC-coupled feedback loop: `measured surplus + measured battery charge power`.
- An elapsed-time EMA smooths the target, with a five-second settling period after power changes.
- Surplus below `max(75 W, SOLAR_MIN_SURPLUS_W / 4)` enters a 60-second grace period at 75 W before stopping.
- Adaptive cooldowns reduce short-session cycling.
- Battery-full protection, grid reservations, discharge windows, and repeated telemetry failure override solar charging.
- Solar is used ahead of reserved grid energy only when its forgone export value is no greater than the marginal reserved grid price.
