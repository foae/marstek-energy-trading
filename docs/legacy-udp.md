# Legacy Marstek UDP Client

The `clients/marstek` package preserves an experimental client for Marstek's LAN JSON-RPC API. The executable does not wire this client: production control always uses the ESPHome HTTP backend. `BATTERY_UDP_ADDR` is retained for library work and has no effect on `cmd/trader`.

Protocol details and device compatibility are documented by Marstek in the [Marstek Device Open API PDF](https://static-eu.marstekenergy.com/ems/resource/agreement/MarstekDeviceOpenApi.pdf). Marstek owns that documentation and its trademarks; this repository does not redistribute the vendor document.

Implementation-specific notes:

- UDP requests and responses use JSON with an `id`, `method`, and `params` object.
- Discovery uses `Marstek.GetDevice` with `{"ble_mac":"0"}`.
- The client binds the configured local UDP port because devices reply to the request's source port.
- Responses are accepted only when their ID matches the request ID.
- Passive-mode commands require periodic refresh before their countdown expires.
- The preserved client binds on all interfaces and has no authentication or cryptographic response validation. Do not expose it to an untrusted network.
- The fallback battery-status path cannot reliably distinguish omitted firmware flags from explicit interlocks. It must be hardened before being offered as a runtime backend.
