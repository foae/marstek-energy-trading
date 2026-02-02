# Marstek Device Open API Documentation

> **Version**: Rev 1.0
> **Protocol**: UDP over LAN
> **Default Port**: 30000
> **Format**: JSON-RPC style

## Overview

The Marstek Open API enables local network communication with Marstek energy storage devices (Venus C, Venus D, Venus E). This API allows querying device status, battery information, solar PV data, and configuring operating modes.

## Prerequisites

Before using the API:

1. Device must be powered on and connected to your home network (WiFi or Ethernet)
2. Device must be bound via the Marstek mobile app
3. **Open API feature must be enabled in the Marstek app**
4. UDP port must be configured (default: 30000, recommended: 49152-65535)

## Protocol Specification

### Transport

| Property | Value |
|----------|-------|
| Protocol | UDP |
| Default Port | 30000 |
| Discovery | UDP Broadcast |
| Encoding | UTF-8 JSON |

**Important**: The client must bind to port 30000 as the source port. The device responds to the source port of incoming requests.

### Request Format

```json
{
  "id": <number|string>,
  "method": "<string>",
  "params": { <object> }
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `id` | number \| string | Yes | Client-defined request identifier, echoed in response |
| `method` | string | Yes | API method name (e.g., `"Marstek.GetDevice"`) |
| `params` | object | Yes | Method-specific parameters |

### Response Format (Success)

```json
{
  "id": <number|string>,
  "src": "<string>",
  "result": { <object> }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | number \| string | Echoed request identifier |
| `src` | string | Device identifier (format: `"{Model}-{MAC}"`) |
| `result` | object | Method-specific response data |

### Response Format (Error)

```json
{
  "id": <number|string>,
  "src": "<string>",
  "error": {
    "code": <number>,
    "message": "<string>"
  }
}
```

### Error Codes

| Code | Message | Description |
|------|---------|-------------|
| -32700 | Parse error | Invalid JSON received |
| -32600 | Invalid Request | JSON is not a valid request object |
| -32601 | Method not found | Method does not exist or is unavailable |
| -32602 | Invalid params | Invalid method parameters |
| -32603 | Internal error | Internal JSON-RPC error |
| -32000 to -32099 | Server error | Implementation-defined errors |

---

## API Methods

### 1. Marstek (Device Discovery)

#### 1.1 Marstek.GetDevice

Discover Marstek devices on the local network via UDP broadcast.

**Request Parameters:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `ble_mac` | string | Yes | BLE MAC to filter specific device, or `"0"` for all devices |

**Response Fields:**

| Field | Type | Description |
|-------|------|-------------|
| `device` | string | Device model (e.g., `"VenusC"`, `"VenusD"`, `"VenusE"`) |
| `ver` | number | Firmware version |
| `ble_mac` | string | Bluetooth MAC address |
| `wifi_mac` | string | WiFi MAC address |
| `wifi_name` | string | Connected WiFi network name |
| `ip` | string | Device IP address |

**Example Request:**

```json
{
  "id": 0,
  "method": "Marstek.GetDevice",
  "params": {
    "ble_mac": "0"
  }
}
```

**Example Response:**

```json
{
  "id": 0,
  "src": "VenusC-123456789012",
  "result": {
    "device": "VenusC",
    "ver": 111,
    "ble_mac": "123456789012",
    "wifi_mac": "123456789012",
    "wifi_name": "MY_HOME",
    "ip": "192.168.1.11"
  }
}
```

---

### 2. WiFi

#### 2.1 Wifi.GetStatus

Query device network information.

**Request Parameters:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `id` | number | Yes | Instance ID (use `0`) |

**Response Fields:**

| Field | Type | Description |
|-------|------|-------------|
| `id` | number | Instance ID |
| `wifi_mac` | string | WiFi MAC address |
| `ssid` | string \| null | Connected WiFi name |
| `rssi` | number | WiFi signal strength (dBm) |
| `sta_ip` | string \| null | Device IP address |
| `sta_gate` | string \| null | Gateway IP |
| `sta_mask` | string \| null | Subnet mask |
| `sta_dns` | string \| null | DNS server |

**Example Request:**

```json
{
  "id": 1,
  "method": "Wifi.GetStatus",
  "params": {
    "id": 0
  }
}
```

**Example Response:**

```json
{
  "id": 1,
  "src": "VenusC-mac",
  "result": {
    "id": 0,
    "ssid": "Home",
    "rssi": -59,
    "sta_ip": "192.168.137.41",
    "sta_gate": "192.168.137.1",
    "sta_mask": "255.255.255.0",
    "sta_dns": "192.168.137.1"
  }
}
```

---

### 3. Bluetooth

#### 3.1 BLE.GetStatus

Query Bluetooth connection status.

**Request Parameters:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `id` | number | Yes | Instance ID (use `0`) |

**Response Fields:**

| Field | Type | Description |
|-------|------|-------------|
| `id` | number | Instance ID |
| `state` | string | Bluetooth state (e.g., `"connect"`) |
| `ble_mac` | string | Bluetooth MAC address |

**Example Request:**

```json
{
  "id": 1,
  "method": "BLE.GetStatus",
  "params": {
    "id": 0
  }
}
```

**Example Response:**

```json
{
  "id": 1,
  "src": "VenusC-123456789012",
  "result": {
    "id": 0,
    "state": "connect",
    "ble_mac": "123456789012"
  }
}
```

---

### 4. Battery

#### 4.1 Bat.GetStatus

Query battery information and status.

**Request Parameters:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `id` | number | Yes | Instance ID (use `0`) |

**Response Fields:**

| Field | Type | Unit | Description |
|-------|------|------|-------------|
| `id` | number | - | Instance ID |
| `soc` | number | % | State of charge |
| `charg_flag` | boolean | - | Charging permitted |
| `dischrg_flag` | boolean | - | Discharging permitted |
| `bat_temp` | number \| null | °C | Battery temperature |
| `bat_capacity` | number \| null | Wh | Remaining capacity |
| `rated_capacity` | number \| null | Wh | Rated capacity |

**Example Request:**

```json
{
  "id": 1,
  "method": "Bat.GetStatus",
  "params": {
    "id": 0
  }
}
```

**Example Response:**

```json
{
  "id": 1,
  "src": "VenusC-mac",
  "result": {
    "id": 0,
    "soc": 98,
    "charg_flag": true,
    "dischrg_flag": true,
    "bat_temp": 25.0,
    "bat_capacity": 2508.0,
    "rated_capacity": 2560.0
  }
}
```

---

### 5. PV (Photovoltaic)

> **Note**: Only available on Venus D

#### 5.1 PV.GetStatus

Query solar panel information.

**Request Parameters:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `id` | number | Yes | Instance ID (use `0`) |

**Response Fields:**

| Field | Type | Unit | Description |
|-------|------|------|-------------|
| `id` | number | - | Instance ID |
| `pv_power` | number | W | Solar charging power |
| `pv_voltage` | number | V | Solar voltage |
| `pv_current` | number | A | Solar current |

**Example Request:**

```json
{
  "id": 1,
  "method": "PV.GetStatus",
  "params": {
    "id": 0
  }
}
```

**Example Response:**

```json
{
  "id": 1,
  "src": "VenusC-mac",
  "result": {
    "id": 0,
    "pv_power": 580.0,
    "pv_voltage": 40.0,
    "pv_current": 12.0
  }
}
```

---

### 6. ES (Energy System)

#### 6.1 ES.GetStatus

Query energy system status and statistics.

**Request Parameters:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `id` | number \| null | No | Instance ID |

**Response Fields:**

| Field | Type | Unit | Description |
|-------|------|------|-------------|
| `id` | number \| null | - | Instance ID |
| `bat_soc` | number \| null | % | Total battery SOC |
| `bat_cap` | number \| null | Wh | Total battery capacity |
| `pv_power` | number \| null | W | Solar charging power |
| `ongrid_power` | number \| null | W | Grid-tied power |
| `offgrid_power` | number \| null | W | Off-grid power |
| `bat_power` | number \| null | W | Battery power |
| `total_pv_energy` | number \| null | Wh | Total solar energy generated |
| `total_grid_output_energy` | number \| null | Wh | Total energy exported to grid |
| `total_grid_input_energy` | number \| null | Wh | Total energy imported from grid |
| `total_load_energy` | number \| null | Wh | Total load/off-grid energy consumed |

**Example Request:**

```json
{
  "id": 1,
  "method": "ES.GetStatus",
  "params": {
    "id": 0
  }
}
```

**Example Response:**

```json
{
  "id": 1,
  "src": "VenusC-mac",
  "result": {
    "id": 0,
    "bat_soc": 98,
    "bat_cap": 2560,
    "pv_power": 0,
    "ongrid_power": 100,
    "offgrid_power": 0,
    "bat_power": 0,
    "total_pv_energy": 0,
    "total_grid_output_energy": 844,
    "total_grid_input_energy": 1607,
    "total_load_energy": 0
  }
}
```

---

#### 6.2 ES.GetMode

Get current operating mode.

**Request Parameters:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `id` | number \| null | No | Instance ID |

**Response Fields:**

| Field | Type | Unit | Description |
|-------|------|------|-------------|
| `id` | number \| null | - | Instance ID |
| `mode` | string | - | Operating mode: `"Auto"`, `"AI"`, `"Manual"`, `"Passive"` |
| `ongrid_power` | number \| null | W | Grid-tied power |
| `offgrid_power` | number \| null | W | Off-grid power |
| `bat_soc` | number \| null | % | Battery SOC |

**Example Request:**

```json
{
  "id": 0,
  "method": "ES.GetMode",
  "params": {
    "id": 0
  }
}
```

**Example Response:**

```json
{
  "id": 0,
  "src": "VenusC-mac",
  "result": {
    "id": 0,
    "mode": "Passive",
    "ongrid_power": 100,
    "offgrid_power": 0,
    "bat_soc": 98
  }
}
```

---

#### 6.3 ES.SetMode

Configure operating mode.

**Request Parameters:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `id` | number | Yes | Instance ID |
| `config` | object | Yes | Mode configuration |

**Config Object:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `mode` | string | Yes | `"Auto"`, `"AI"`, `"Manual"`, or `"Passive"` |
| `auto_cfg` | object | For Auto | Auto mode config |
| `ai_cfg` | object | For AI | AI mode config |
| `manual_cfg` | object | For Manual | Manual mode config |
| `passive_cfg` | object | For Passive | Passive mode config |

**auto_cfg Object:**

| Field | Type | Description |
|-------|------|-------------|
| `enable` | number | 1 = ON, set another mode = OFF |

**ai_cfg Object:**

| Field | Type | Description |
|-------|------|-------------|
| `enable` | number | 1 = ON, set another mode = OFF |

**manual_cfg Object:**

| Field | Type | Description |
|-------|------|-------------|
| `time_num` | number | Time period slot (0-9 for Venus C/E) |
| `start_time` | string | Start time `"HH:MM"` |
| `end_time` | string | End time `"HH:MM"` |
| `week_set` | number | Bitmask for days (bit 0=Mon, bit 6=Sun). 127 = all days |
| `power` | number | Power setting in Watts |
| `enable` | number | 1 = ON, 0 = OFF |

**passive_cfg Object:**

| Field | Type | Unit | Description |
|-------|------|------|-------------|
| `power` | number | W | Power setting (positive = discharge, negative = charge) |
| `cd_time` | number | s | Countdown timer in seconds |

**Response Fields:**

| Field | Type | Description |
|-------|------|-------------|
| `id` | number | Instance ID |
| `set_result` | boolean | `true` = success, `false` = failure |

**Example: Auto Mode**

```json
{
  "id": 1,
  "method": "ES.SetMode",
  "params": {
    "id": 0,
    "config": {
      "mode": "Auto",
      "auto_cfg": {
        "enable": 1
      }
    }
  }
}
```

**Example: AI Mode**

```json
{
  "id": 1,
  "method": "ES.SetMode",
  "params": {
    "id": 0,
    "config": {
      "mode": "AI",
      "ai_cfg": {
        "enable": 1
      }
    }
  }
}
```

**Example: Manual Mode**

```json
{
  "id": 1,
  "method": "ES.SetMode",
  "params": {
    "id": 0,
    "config": {
      "mode": "Manual",
      "manual_cfg": {
        "time_num": 1,
        "start_time": "08:30",
        "end_time": "20:30",
        "week_set": 127,
        "power": 100,
        "enable": 1
      }
    }
  }
}
```

**Example: Passive Mode**

```json
{
  "id": 1,
  "method": "ES.SetMode",
  "params": {
    "id": 0,
    "config": {
      "mode": "Passive",
      "passive_cfg": {
        "power": 100,
        "cd_time": 300
      }
    }
  }
}
```

**Example Response:**

```json
{
  "id": 1,
  "src": "Venus-mac",
  "result": {
    "id": 0,
    "set_result": true
  }
}
```

---

### 7. EM (Energy Meter)

#### 7.1 EM.GetStatus

Query energy meter / CT (Current Transformer) data.

**Request Parameters:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `id` | number \| null | No | Instance ID |

**Response Fields:**

| Field | Type | Unit | Description |
|-------|------|------|-------------|
| `id` | number \| null | - | Instance ID |
| `ct_state` | number \| null | - | CT status: 0 = not connected, 1 = connected |
| `a_power` | number \| null | W | Phase A power |
| `b_power` | number \| null | W | Phase B power |
| `c_power` | number \| null | W | Phase C power |
| `total_power` | number \| null | W | Total power |

**Example Request:**

```json
{
  "id": 1,
  "method": "EM.GetStatus",
  "params": {
    "id": 0
  }
}
```

**Example Response:**

```json
{
  "id": 1,
  "src": "VenusC-mac",
  "result": {
    "id": 0,
    "ct_state": 1,
    "a_power": 150,
    "b_power": 0,
    "c_power": 0,
    "total_power": 150
  }
}
```

---

## Device Compatibility

| Component | Venus C | Venus D | Venus E |
|-----------|---------|---------|---------|
| Marstek (Discovery) | Yes | Yes | Yes |
| WiFi | Yes | Yes | Yes |
| Bluetooth | Yes | Yes | Yes |
| Battery | Yes | Yes | Yes |
| PV | No | Yes | No |
| ES | Yes | Yes | Yes |
| EM | Yes | Yes | Yes |

---

## Implementation Notes

### UDP Client Requirements

1. **Source Port**: Client MUST bind to port 30000 (or the configured API port)
2. **Broadcast**: For discovery, send to subnet broadcast address (e.g., `192.168.1.255`)
3. **Timeout**: Recommended 2-5 second timeout for responses
4. **Keep-alive**: For Passive mode, resend `ES.SetMode` before `cd_time` expires

### Week Bitmask Calculation

The `week_set` field uses a 7-bit bitmask:

| Day | Bit | Value |
|-----|-----|-------|
| Monday | 0 | 1 |
| Tuesday | 1 | 2 |
| Wednesday | 2 | 4 |
| Thursday | 3 | 8 |
| Friday | 4 | 16 |
| Saturday | 5 | 32 |
| Sunday | 6 | 64 |

Examples:
- Monday only: `1`
- Weekdays (Mon-Fri): `31`
- Weekend (Sat-Sun): `96`
- All days: `127`

### Power Values

- **Positive power**: Discharging (battery to grid/load)
- **Negative power**: Charging (grid to battery)

### Passive Mode

Passive mode enables external control systems (e.g., Home Assistant) to directly control battery charge/discharge:

- Set `power` to desired charge/discharge rate
- Set `cd_time` (countdown) as a safety timeout
- Device reverts to previous mode when countdown expires
- Resend command periodically to maintain control
