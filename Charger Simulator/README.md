# Charger Simulator — Slice 2

One virtual OCPP 1.6J AC/DC charger driven from a browser. Configure it like
you would a real charger via the WiFi-config app — OCPP URL, charge-point ID,
admin/admin — then plug in, start, stop, throttle power, and pull the e-stop,
all with a live view of the OCPP transcript.

> **Status:** slice 2 of N. Single charger, full session lifecycle, UI-driven
> control, embedded dashboard. No DB, no orchestrator, no multi-charger yet.

---

## What's new since slice 1

- **Embedded dashboard** at `http://localhost:8080`. All-Go: HTML/CSS/JS are
  baked into the worker binary via `embed.FS`. No Node, no build step.
- **HTTP + WebSocket API** on the worker drives the charger. The browser uses
  it; you can too, via curl/Postman, for headless tests.
- **Discrete UI commands**: Plug In Gun, Start Charging, Stop Charging,
  Emergency Stop, Clear Fault, Load + / Load − / slider. Each maps 1:1 to a
  dedicated actor command.
- **Live event stream**: every OCPP send/recv, state transition, and lifecycle
  event flows to the dashboard via WS. Auto-scrolls; pauses when you scroll up.
- **Auto-exit / scripted CLI scenario removed.** What that did is now a
  button click in the UI (or one `curl` call).

---

## Prerequisites

- **Go 1.22 or newer.** Download: https://go.dev/dl/
- A modern browser (anything from 2023+).

You don't need Docker, a database, or anything else.

---

## Build

```powershell
go mod tidy
go build -o bin\worker.exe   .\cmd\worker
go build -o bin\stub-cms.exe .\cmd\stub-cms
```

---

## Run

Open **two terminals** in the project root.

### Terminal 1 — stub CMS (unchanged from slice 1)

```powershell
.\bin\stub-cms.exe --addr :9000
```

### Terminal 2 — worker (now serves the dashboard)

```powershell
.\bin\worker.exe --http-addr :8080
```

Output:
```
INFO msg=dashboard ready url=http://localhost:8080
```

### Open the dashboard

Browse to **http://localhost:8080**.

You'll see a configuration card. The defaults match the stub CMS exactly:

| Field          | Default                          |
| -------------- | -------------------------------- |
| OCPP URL       | `ws://localhost:9000/ocpp`       |
| Charge Point ID| `SIM-CP-0001`                    |
| Username       | `admin`                          |
| Password       | `admin`                          |
| Vendor / Model | TobOR Sim / Virtual AC 22kW      |
| Max kW         | 22                               |
| Phases         | 3                                |
| Voltage        | 400                              |
| Heartbeat (s)  | 30                               |
| MeterValues (s)| 5                                |

Click **Connect**. The form hides; you'll see:

1. A **status card** showing the charger ID, CMS URL, connection pill
   (`Booting → Online`), connector pill (`Available`), transaction info,
   session duration.
2. A **live meter grid** with Energy, Power, Voltage, Current.
3. A **power limit** row with a slider plus `− 1 kW` / `+ 1 kW` buttons.
4. A **controls** row:
   - **Plug In Gun** — sets connector to `Preparing` (only enabled in Available).
   - **Start Charging** — sends `Authorize` then `StartTransaction`, sets `Charging`.
   - **Stop Charging** — sends `StopTransaction` (reason Local), back to `Available`.
   - **EMERGENCY STOP** — `StopTransaction` (reason EmergencyStop), connector → `Faulted`.
   - **Clear Fault** — only visible when Faulted; resets to `Available`.
   - **Disconnect** — tears down the charger; UI returns to the config form.
5. A **live OCPP log** at the bottom — every send/recv with latency, plus
   lifecycle events. Use the `clear` button to wipe it.

Pointing the simulator at a real CMS is identical — just change the OCPP URL,
username/password, and CP ID in the form.

---

## HTTP API (drive it from curl / CI)

All endpoints are on the worker's `--http-addr` port (default `:8080`).

| Method | Path                  | Body                            | Effect |
| ------ | --------------------- | ------------------------------- | ------ |
| GET    | `/api/state`          | —                               | 200 StateSnapshot, 404 if not configured |
| GET    | `/api/config`         | —                               | 200 ChargerConfig (password redacted), 404 if not configured |
| POST   | `/api/configure`      | configure JSON (see below)      | Start a new charger; replaces any existing one |
| POST   | `/api/disconnect`     | —                               | Stop and clear |
| POST   | `/api/plug-in`        | —                               | `c.PlugIn(1)` |
| POST   | `/api/start-charging` | `{"id_tag":"…"}`               | `c.StartCharging(idTag, 1)` |
| POST   | `/api/stop-charging`  | —                               | `c.StopCharging(1)` |
| POST   | `/api/emergency-stop` | —                               | `c.EmergencyStop(1)` |
| POST   | `/api/clear-fault`    | —                               | `c.ClearFault(1)` |
| POST   | `/api/set-power`      | `{"watts":11000}`              | `c.SetPower(watts)`, clamped to [0, configured max] |
| GET    | `/api/events`         | (WebSocket)                     | Live stream of all events |

### Configure JSON body

```json
{
  "cms_url":     "ws://localhost:9000/ocpp",
  "cp_id":       "SIM-CP-0001",
  "user":        "admin",
  "pass":        "admin",
  "vendor":      "TobOR Sim",
  "model":       "Virtual AC 22kW",
  "fw_version":  "sim-0.1.0",
  "max_kw":      22,
  "phases":      3,
  "voltage":     400,
  "heartbeat_s": 30,
  "meter_s":     5
}
```

### Headless example

```powershell
# Start a charger
curl.exe -X POST http://localhost:8080/api/configure -H "content-type: application/json" -d "{\"cms_url\":\"ws://localhost:9000/ocpp\",\"cp_id\":\"SIM-CP-0099\",\"max_kw\":22,\"phases\":3,\"voltage\":400}"

# Plug in, start, watch meter, stop
curl.exe -X POST http://localhost:8080/api/plug-in
curl.exe -X POST http://localhost:8080/api/start-charging -H "content-type: application/json" -d "{\"id_tag\":\"TESTRFID0001\"}"
curl.exe http://localhost:8080/api/state
curl.exe -X POST http://localhost:8080/api/stop-charging
```

### Event stream JSON

Three event kinds flow over `GET /api/events` (WebSocket):

```json
{ "time":"...", "kind":"state", "state": { /* StateSnapshot */ } }
{ "time":"...", "kind":"ocpp",  "dir":"out", "ocpp_type":"CALL",       "action":"MeterValues", "msg_id":"…", "payload":"…" }
{ "time":"...", "kind":"ocpp",  "dir":"in",  "ocpp_type":"CALLRESULT", "action":"MeterValues", "msg_id":"…", "payload":"{}", "latency_ms":12 }
{ "time":"...", "kind":"log",   "level":"info", "msg":"transaction started", "fields": {"tx_id":1001, "id_tag":"…", "meter_start_wh":0} }
```

---

## Project layout

```
charger-simulator/
├── cmd/
│   ├── worker/          # main: HTTP+WS server + dashboard
│   └── stub-cms/        # main: local CMS for testing
└── internal/
    ├── ocpp/j16/        # OCPP 1.6-J wire codec, message types, enums
    ├── transport/ws/    # WebSocket client (coder/websocket)
    ├── actor/           # Charger actor — single-goroutine state owner
    ├── sim/meter/       # Meter value calculation engine
    ├── config/          # Per-charger config struct
    └── api/             # NEW: HTTP+WS server + embedded dashboard
        ├── server.go
        └── web/
            ├── web.go     # //go:embed declarations
            ├── index.html
            ├── styles.css
            └── app.js
```

---

## Key files to read

- Actor: [internal/actor/charger.go](internal/actor/charger.go) — the public command set, FSMs, snapshot, event stream.
- API: [internal/api/server.go](internal/api/server.go) — routes, WebSocket fan-out, charger lifecycle.
- Dashboard JS: [internal/api/web/app.js](internal/api/web/app.js) — event-driven render, button wiring.
- Meter engine: [internal/sim/meter/meter.go](internal/sim/meter/meter.go) — energy integration, per-phase V/I, mutable power limit.
- Stub CMS: [cmd/stub-cms/main.go](cmd/stub-cms/main.go).

---

## What's next (slice 3 preview)

- **Multiple chargers per worker** — `/api/configure` becomes `/api/chargers` (POST creates one, GET lists). Dashboard becomes a fleet grid.
- **Reconnect with exponential backoff** when the CMS WS drops.
- **Behavior profiles** — quirk presets per OEM (Delta / Exicom / Bharat AC001).
- **Charging curves** — CC-CV taper, multi-segment DC curves, SoC-aware.
- **Scenario builder** — record + replay scripted runs.
- **Persistence** — Postgres + Timescale for OCPP audit + session history.
