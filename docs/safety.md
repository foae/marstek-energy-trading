# Safety

Control safety model, security and privacy, and known limitations. Read this before enabling automatic operation.

## Control Safety Model

The default ESPHome control path:

- reads a select before writing it and confirms changed select values from subsequent ESPHome publications;
- verifies signed measured battery power before declaring charge or discharge active;
- limits a new grid-charge control operation to the exact reserved interval and revalidates the same paired cycle after persistence;
- keeps stop intent pending until the stop is confirmed;
- retries failed stops without starting a conflicting action;
- checks telemetry staleness throughout active sessions;
- attempts to stop the battery during startup and graceful shutdown.

These measures do not create an independent fail-safe. The ESPHome backend has no battery-side command expiry: `PASSIVE_MODE_TIMEOUT_S` controls the service's refresh cadence but is not a hardware dead-man timer. If the process, host, network, or bridge fails while a forced command is active, the battery can continue until its own BMS limit or until control is restored. Use the software only after verifying the battery's built-in protections and provide independent supervision where required.

An ESPHome control operation has a 45-second overall budget, while each changed select can consume up to 35 seconds of confirmation polling. If both selects need their full confirmation path, the operation can time out after writes were attempted. The service treats that outcome as unknown and retains any persisted discharge commitment.

## Security And Privacy

- Keep ESPHome, HomeWizard, and this service on a trusted local network. Their local HTTP APIs are normally unauthenticated.
- The service HTTP API binds to loopback by default because `/status` and `/metrics` disclose household energy and financial data. If exposing it, use a firewall or authenticated reverse proxy and configure `HTTP_LISTEN_ADDR` explicitly.
- `HOMEWIZARD_P1_URL=auto` first uses mDNS and then actively probes `192.168.0.0/24` and `192.168.1.0/24`. Use an explicit URL on shared, VPN, or differently addressed networks.
- Keep `.env`, `data/`, diagnostic logs, and container build contexts private. The repository's `.gitignore` and `.dockerignore` exclude them by default.
- Trade history and Telegram offsets are stored locally with owner-only permissions. They can reveal occupancy and energy-use patterns.
- Telegram bot tokens are credentials. Rotate a token immediately if it appears in logs, shell history, an image cache, or a commit.
- Report vulnerabilities privately as described in [SECURITY.md](../SECURITY.md).

## Limitations

- No independent hardware watchdog or forced-command expiry is provided by the ESPHome backend.
- Refreshed planning candidates, solar cycle retention, active-session energy, and daily-summary delivery state are not persisted. The paired grid-cycle commitment is persisted before the charge command, so it records intent rather than proof of execution; a crash after persistence but before control can restore a discharge obligation for energy that was never purchased, and a later crash can lose partial-session accounting.
- An invalid commitment file blocks startup rather than guessing. A valid restored cycle that fails the current profit floor retains its discharge obligation but cannot resume grid charging. Confirm the battery is physically idle before following the recovery procedure in [Operations](operations.md).
- The ESPHome bridge configuration and firmware compatibility matrix are not included.
- Solar power-number writes use ESPHome's optimistic number state; measured power is observed on subsequent ticks rather than transactionally confirmed for every adjustment.
- A single serialized control loop performs battery, meter, price, and Telegram I/O. Slow network calls can delay other checks.
- Only one battery, one P1 meter, EUR pricing, and 15-minute NordPool products are supported.
- Default tax, VAT, fee, area, timezone, battery capacity, and efficiency values are examples for one Dutch setup and will become stale.
- Import and export are valued symmetrically; installations with a separate feed-in tariff need code changes.
- P&L and energy are estimates, not billing, tax, warranty, or investment records.
- Runtime history grows without retention and is rewritten atomically after every completed trade.
- The legacy UDP package is unauthenticated, spoofable on an untrusted LAN, and not wired into the service. See [Legacy UDP Client](legacy-udp.md).
