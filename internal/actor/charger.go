// Package actor implements the Charger actor: a single goroutine that owns
// all per-charger state, runs the connector + transaction state machines,
// and drives OCPP-J traffic against one CMS.
//
// Design rules (unchanged from slice 1):
//   - Single goroutine owns all state. No mutexes on the hot path.
//   - External callers (dashboard API, scenarios, future orchestrator) push
//     Commands onto a buffered channel; the actor processes them serially.
//   - WebSocket Send is called only from the actor goroutine.
//   - Outbound CALLs register a pendingCall keyed by message ID; inbound
//     CALLRESULT/CALLERROR look it up and invoke the callback synchronously.
//
// Slice 2 additions:
//   - Public command set is split into discrete user actions so each maps
//     1:1 to a dashboard button: PlugIn, StartCharging, StopCharging,
//     EmergencyStop, ClearFault, SetPower.
//   - An atomic StateSnapshot is published after every state-changing
//     operation so the HTTP layer can serve GET /api/state in O(1).
//   - An Events channel emits structured events for the dashboard's live
//     log: OCPP send/recv (with latency), state changes, and significant
//     lifecycle events.
package actor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/tobor/charger-simulator/internal/config"
	"github.com/tobor/charger-simulator/internal/ocpp/j16"
	"github.com/tobor/charger-simulator/internal/session"
	"github.com/tobor/charger-simulator/internal/sim/meter"
	"github.com/tobor/charger-simulator/internal/transport/ws"
)

// ---------------------------------------------------------------------------
// charge-point lifecycle (independent of connector status)
// ---------------------------------------------------------------------------

type cpState int

const (
	cpBooting cpState = iota
	cpOnline
	cpDisconnected
	// cpOffline is an OPERATOR-INDUCED outage (simulated wifi loss / power cut).
	// Unlike cpDisconnected — which means the charger process is gone — an
	// offline charger is still configured and can be brought back with GoOnline.
	// While offline the WebSocket to the CMS is dropped (so the CMS observes the
	// disconnect) and all user actions, including StartCharging, are rejected.
	cpOffline
	// cpReconnecting is an UNEXPECTED drop (CMS shut down / restarted / network
	// loss) that the charger is auto-recovering from with exponential backoff.
	// The process and all session state stay alive; on a successful reboot any
	// interrupted session is closed out with a StopTransaction.
	cpReconnecting
)

func (s cpState) String() string {
	switch s {
	case cpBooting:
		return "Booting"
	case cpOnline:
		return "Online"
	case cpDisconnected:
		return "Disconnected"
	case cpOffline:
		return "Offline"
	case cpReconnecting:
		return "Reconnecting"
	}
	return "Unknown"
}

// ---------------------------------------------------------------------------
// Commands — the actor's public verbs.
// Each maps to a dashboard button. All are goroutine-safe via Charger methods.
// ---------------------------------------------------------------------------

type Command interface{ isCommand() }

// CmdPlugIn: simulate the user plugging the gun into the connector.
// Effect: Available → Preparing. No transaction yet. Emits StatusNotification.
type CmdPlugIn struct{ ConnectorID int }

func (CmdPlugIn) isCommand() {}

// CmdStartCharging: simulate the user starting the session (RFID swipe).
// Effect: (Available|Preparing) → Charging, via Authorize + StartTransaction.
// If Available, an implicit Preparing transition is emitted first so the CMS
// observes a spec-correct sequence.
type CmdStartCharging struct {
	ConnectorID int
	IdTag       string
}

func (CmdStartCharging) isCommand() {}

// CmdStopCharging: normal user-initiated stop. Reason=Local.
type CmdStopCharging struct{ ConnectorID int }

func (CmdStopCharging) isCommand() {}

// CmdUnplug: simulate the user pulling the gun out BEFORE charging started.
// Effect: Preparing → Available (StatusNotification only, no transaction).
// If the connector is already Charging, use CmdStopCharging instead — this
// command is the symmetric undo of CmdPlugIn.
type CmdUnplug struct{ ConnectorID int }

func (CmdUnplug) isCommand() {}

// CmdEmergencyStop: safety stop. Reason=EmergencyStop.
// If a transaction is active, it is stopped first. The connector is then
// transitioned to Faulted with errorCode=OtherError and info=EmergencyStop
// so the CMS can distinguish from a normal stop.
type CmdEmergencyStop struct{ ConnectorID int }

func (CmdEmergencyStop) isCommand() {}

// CmdClearFault: reset a Faulted connector back to Available.
type CmdClearFault struct{ ConnectorID int }

func (CmdClearFault) isCommand() {}

// CmdSetPower: change the EVSE-imposed power ceiling for the active session.
// Clamped to [0, configured max]. Takes effect from the next MeterValue sample.
type CmdSetPower struct{ Watts float64 }

func (CmdSetPower) isCommand() {}

// CmdSetMeterFault arms (or clears) an energy-meter fault on one connector.
// Mode=meter.FaultNone restores a healthy meter. Takes effect from the next
// MeterValue sample; it never touches the connector status (silent fault).
type CmdSetMeterFault struct {
	ConnectorID int
	Mode        meter.Fault
}

func (CmdSetMeterFault) isCommand() {}

// CmdGoOffline simulates the charger losing its uplink (wifi drop / power cut).
// Effect: the WebSocket to the CMS is closed so the CMS observes a disconnect,
// the CP enters the Offline state, and every user action is rejected until
// CmdGoOnline. Connector/session state is preserved in memory.
type CmdGoOffline struct{}

func (CmdGoOffline) isCommand() {}

// CmdGoOnline ends a simulated outage: it re-dials the CMS, re-sends
// BootNotification and re-announces each connector's current status.
type CmdGoOnline struct{}

func (CmdGoOnline) isCommand() {}

// ---------------------------------------------------------------------------
// Event stream — drives the dashboard's live updates.
// ---------------------------------------------------------------------------

// EventKind enumerates the kinds of event a Charger emits.
type EventKind string

const (
	EventState EventKind = "state" // a state snapshot changed
	EventOCPP  EventKind = "ocpp"  // an OCPP frame was sent or received
	EventLog   EventKind = "log"   // a notable lifecycle log line
)

// Event is the discriminated union of everything the dashboard cares about.
type Event struct {
	Time time.Time `json:"time"`
	Kind EventKind `json:"kind"`

	// kind=state
	State *StateSnapshot `json:"state,omitempty"`

	// kind=ocpp
	Dir       string `json:"dir,omitempty"`       // "out" | "in"
	OCPPType  string `json:"ocpp_type,omitempty"` // "CALL" | "CALLRESULT" | "CALLERROR"
	Action    string `json:"action,omitempty"`
	MsgID     string `json:"msg_id,omitempty"`
	Payload   string `json:"payload,omitempty"`
	LatencyMs int64  `json:"latency_ms,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
	ErrorDesc string `json:"error_desc,omitempty"`

	// kind=log
	Level  string         `json:"level,omitempty"`
	Msg    string         `json:"msg,omitempty"`
	Fields map[string]any `json:"fields,omitempty"`
}

// ConnectorSnapshot is the per-connector view exposed to the dashboard.
type ConnectorSnapshot struct {
	ID                  int     `json:"id"`
	Status              string  `json:"status"`
	ErrorCode           string  `json:"error_code,omitempty"`
	TransactionID       int     `json:"tx_id,omitempty"`
	IdTag               string  `json:"id_tag,omitempty"`
	EnergyWh            int     `json:"energy_wh"`
	PowerW              float64 `json:"power_w"`
	VoltageV            float64 `json:"voltage_v"`
	CurrentA            float64 `json:"current_a"`
	SoC                 float64 `json:"soc,omitempty"` // DC only
	SessionStartUnix    int64   `json:"session_start_unix,omitempty"`
	SessionEnergyWh     int     `json:"session_energy_wh,omitempty"`
	MaxPowerWConfigured float64 `json:"max_power_w_configured"`
	MaxPowerWCurrent    float64 `json:"max_power_w_current"`
	MeterFault          string  `json:"meter_fault,omitempty"` // "" / "none" = healthy
}

// StateSnapshot is the dashboard's view of the charger. Updated atomically
// by the actor on every state change; readable from any goroutine via State().
//
// The top-level Status/Tx/Energy/Power/Voltage/Current/SoC fields mirror
// connector 1 so single-connector dashboards (slice 2/3) and curl scripts that
// read those fields keep working unchanged. The Connectors array is the
// authoritative per-connector data — slice 4+ frontends should read from it.
type StateSnapshot struct {
	CPID          string `json:"cp_id"`
	Vendor        string `json:"vendor"`
	Model         string `json:"model"`
	CMSURL        string `json:"cms_url"`
	ChargerType   string `json:"charger_type"`   // "AC" | "DC"
	NumConnectors int    `json:"num_connectors"` // 1 or 2
	Phases        int    `json:"phases"`         // AC only; 0 for DC
	NominalV      float64 `json:"nominal_v"`

	Connected bool   `json:"connected"`
	CPState   string `json:"cp_state"`

	// Per-connector state — authoritative.
	Connectors []ConnectorSnapshot `json:"connectors"`

	// ---- Back-compat mirror of connector 1 (slice 2/3 fields) ----
	ConnectorStatus     string  `json:"connector_status"`
	ConnectorErrorCode  string  `json:"connector_error_code,omitempty"`
	TransactionID       int     `json:"tx_id,omitempty"`
	IdTag               string  `json:"id_tag,omitempty"`
	EnergyWh            int     `json:"energy_wh"`
	PowerW              float64 `json:"power_w"`
	VoltageV            float64 `json:"voltage_v"`
	CurrentA            float64 `json:"current_a"`
	SoC                 float64 `json:"soc,omitempty"` // DC only
	SessionStartUnix    int64   `json:"session_start_unix,omitempty"`
	SessionEnergyWh     int     `json:"session_energy_wh,omitempty"`
	MaxPowerWConfigured float64 `json:"max_power_w_configured"`
	MaxPowerWCurrent    float64 `json:"max_power_w_current"`
	MeterFault          string  `json:"meter_fault,omitempty"`

	UpdatedAt int64 `json:"updated_at_unix"`
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

type connector struct {
	id        int
	status    j16.ChargePointStatus
	errorCode j16.ChargePointErrorCode
	txID      int // 0 = no active tx

	// startPending is true from the moment a StartCharging request begins its
	// Authorize → StartTransaction handshake until that handshake resolves. It
	// guards against a second StartCharging slipping in while the connector is
	// still Preparing (a double-tap / concurrent-request race) and spawning a
	// duplicate transaction.
	startPending bool

	// Per-session state — cleared on StopTransaction or EmergencyStop.
	sessionStartedAt time.Time
	sessionStartWh   int
	sessionIdTag     string

	// Each connector has its OWN meter integrator so dual-gun chargers track
	// two independent sessions in parallel.
	meterEng *meter.Engine

	// meterFault injects an energy-meter fault into this connector's
	// MeterValues (missing / empty / zeroed measurands). meter.FaultNone =
	// healthy. It is a reporting filter only — meterEng keeps integrating.
	meterFault meter.Fault
}

type pendingCall struct {
	action   string
	sentAt   time.Time
	onResult func(json.RawMessage)
	onError  func(code, desc string)
}

// Charger is the runtime for one virtual EVSE. State that USED to be charger-
// level (one meter engine, one session) is now per-connector since slice 4
// introduced dual-gun support.
type Charger struct {
	cfg config.ChargerConfig
	log *slog.Logger

	ws *ws.Client

	cpState      cpState
	bootInterval time.Duration

	// offline is true while an operator-induced outage is active (see
	// CmdGoOffline). It lets the Run loop distinguish an intentional WS drop
	// from an unexpected one so it keeps the actor alive for a later reconnect.
	offline bool

	connectors map[int]*connector
	pending    map[string]*pendingCall

	cmdCh chan Command

	events   chan Event
	snapshot atomic.Pointer[StateSnapshot]

	// store persists open transactions so they can be closed out with a
	// StopTransaction after any CMS-link interruption, including a full process
	// restart. May be nil (persistence + interrupted-session recovery disabled).
	store session.Store
}

// NewCharger constructs but does not start the actor.
func NewCharger(cfg config.ChargerConfig, log *slog.Logger, store session.Store) *Charger {
	if log == nil {
		log = slog.Default()
	}
	log = log.With("cp_id", cfg.CPID)

	wsClient := ws.New(cfg.CMSURL, log.With("layer", "ws"))
	wsClient.BasicUser = cfg.BasicAuthUser
	wsClient.BasicPass = cfg.BasicAuthPass

	// Each connector gets its own meter engine so the integrator state for
	// connector 1's session doesn't bleed into connector 2's.
	num := cfg.NumConnectors
	if num < 1 {
		num = 1
	}
	if num > 2 {
		num = 2
	}
	conns := make(map[int]*connector, num)
	for i := 1; i <= num; i++ {
		conns[i] = &connector{
			id:         i,
			status:     j16.StatusAvailable,
			errorCode:  j16.ErrorNoError,
			meterEng:   newEngineForCfg(cfg),
			meterFault: meter.FaultNone,
		}
	}

	c := &Charger{
		cfg:          cfg,
		log:          log,
		ws:           wsClient,
		cpState:      cpBooting,
		bootInterval: cfg.HeartbeatInterval,
		connectors:   conns,
		pending:      make(map[string]*pendingCall),
		cmdCh:        make(chan Command, 32),
		events:       make(chan Event, 256),
		store:        store,
	}
	// Publish an initial snapshot so dashboards reading State() before Run starts
	// get something sensible.
	c.publishSnapshot()
	return c
}

// newEngineForCfg constructs a meter.Engine matching the AC/DC profile in cfg.
// One engine per connector; the actor never shares engines.
func newEngineForCfg(cfg config.ChargerConfig) *meter.Engine {
	mode := meter.ModeAC
	if cfg.IsDC() {
		mode = meter.ModeDC
	}
	return &meter.Engine{
		Type:               mode,
		MaxPowerW:          cfg.MaxPowerW,
		NominalV:           cfg.NominalVoltageV,
		Phases:             cfg.Phases,
		PowerFactor:        cfg.PowerFactor,
		PhaseSagOhm:        0.02,
		BatteryCapacityKWh: cfg.BatteryCapacityKWh,
		InitialSoC:         cfg.InitialSoC,
	}
}

// ---------------------------------------------------------------------------
// Public command methods (goroutine-safe)
// ---------------------------------------------------------------------------

func (c *Charger) PlugIn(connectorID int) { c.enqueue(CmdPlugIn{ConnectorID: connectorID}) }

func (c *Charger) StartCharging(idTag string, connectorID int) {
	c.enqueue(CmdStartCharging{ConnectorID: connectorID, IdTag: idTag})
}

func (c *Charger) StopCharging(connectorID int) {
	c.enqueue(CmdStopCharging{ConnectorID: connectorID})
}

func (c *Charger) Unplug(connectorID int) {
	c.enqueue(CmdUnplug{ConnectorID: connectorID})
}

func (c *Charger) EmergencyStop(connectorID int) {
	c.enqueue(CmdEmergencyStop{ConnectorID: connectorID})
}

func (c *Charger) ClearFault(connectorID int) {
	c.enqueue(CmdClearFault{ConnectorID: connectorID})
}

func (c *Charger) SetPower(watts float64) { c.enqueue(CmdSetPower{Watts: watts}) }

// SetMeterFault arms or clears an energy-meter fault on one connector.
func (c *Charger) SetMeterFault(connectorID int, mode meter.Fault) {
	c.enqueue(CmdSetMeterFault{ConnectorID: connectorID, Mode: mode})
}

// GoOffline simulates a wifi/power outage: drops the CMS link and rejects
// charging until GoOnline.
func (c *Charger) GoOffline() { c.enqueue(CmdGoOffline{}) }

// GoOnline ends a simulated outage by reconnecting to the CMS.
func (c *Charger) GoOnline() { c.enqueue(CmdGoOnline{}) }

func (c *Charger) enqueue(cmd Command) {
	select {
	case c.cmdCh <- cmd:
	default:
		c.log.Warn("cmd channel full, dropping", "cmd", fmt.Sprintf("%T", cmd))
	}
}

// ---------------------------------------------------------------------------
// Public observers
// ---------------------------------------------------------------------------

// Events returns the read end of the event stream. Buffered; if a consumer
// falls behind, the actor drops new events rather than blocking.
func (c *Charger) Events() <-chan Event { return c.events }

// State returns the latest published snapshot. Safe from any goroutine.
func (c *Charger) State() StateSnapshot {
	if s := c.snapshot.Load(); s != nil {
		return *s
	}
	return StateSnapshot{}
}

// ---------------------------------------------------------------------------
// Run loop
// ---------------------------------------------------------------------------

// Run blocks until ctx is cancelled or the WS connection ends.
func (c *Charger) Run(ctx context.Context) error {
	c.log.Info("connecting", "url", c.cfg.CMSURL)
	if err := c.ws.Connect(ctx); err != nil {
		c.emitLog("error", "ws connect failed", map[string]any{"err": err.Error()})
		return fmt.Errorf("ws connect: %w", err)
	}
	defer func() {
		c.ws.Close()
		c.cpState = cpDisconnected
		c.publishSnapshot()
		c.emitLog("info", "charger disconnected", nil)
	}()

	if err := c.sendBoot(ctx); err != nil {
		return fmt.Errorf("send boot: %w", err)
	}

	hb := time.NewTicker(c.cfg.HeartbeatInterval)
	defer hb.Stop()
	mv := time.NewTicker(c.cfg.MeterInterval)
	defer mv.Stop()

	inbound := c.ws.Inbound()

	for {
		select {
		case <-ctx.Done():
			c.log.Info("shutting down", "reason", ctx.Err())
			return nil

		case data, ok := <-inbound:
			if !ok {
				if c.offline {
					// We dropped the link on purpose (GoOffline). Stop selecting
					// on the dead channel and stay alive so GoOnline can revive
					// us. A nil channel blocks forever in select.
					inbound = nil
					continue
				}
				// Unexpected drop (CMS shut down / restarted / network loss) but
				// our process is fine. Keep all session state and auto-reconnect
				// with backoff; a successful reboot closes out any interrupted
				// session via recoverInterruptedSessions.
				c.log.Warn("ws closed by remote — auto-reconnecting")
				c.onUnexpectedDrop()
				inbound = c.reconnectWithBackoff(ctx)
				if inbound == nil {
					return nil // ctx cancelled during backoff → clean shutdown
				}
				continue
			}
			if c.offline {
				// Late frames buffered before the link dropped — ignore them.
				continue
			}
			c.handleInbound(ctx, data)

		case <-hb.C:
			if c.cpState == cpOnline {
				c.sendHeartbeat(ctx)
			}

		case <-mv.C:
			// No telemetry unless we're fully online (skips Booting/Offline).
			if c.cpState != cpOnline {
				continue
			}
			// Sweep every connector — each dual-gun charger may have two
			// independent active sessions.
			for id := range c.connectors {
				if c.isCharging(id) {
					c.sendMeterValues(ctx, id)
				}
			}

		case cmd := <-c.cmdCh:
			// GoOnline is handled here (not in handleCommand) because it must
			// swap the loop-local inbound channel after the reconnect.
			if _, ok := cmd.(CmdGoOnline); ok {
				inbound = c.handleGoOnline(ctx)
				continue
			}
			c.handleCommand(ctx, cmd)
		}
	}
}

func (c *Charger) isCharging(id int) bool {
	cn, ok := c.connectors[id]
	return ok && cn.status == j16.StatusCharging
}

// ---------------------------------------------------------------------------
// Outbound calls
// ---------------------------------------------------------------------------

func (c *Charger) call(
	ctx context.Context,
	action string,
	payload any,
	onResult func(json.RawMessage),
	onError func(code, desc string),
) error {
	msgID := uuid.NewString()
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", action, err)
	}
	frame := j16.Call{MessageID: msgID, Action: action, Payload: payloadBytes}
	raw, err := frame.Marshal()
	if err != nil {
		return fmt.Errorf("frame %s: %w", action, err)
	}
	c.pending[msgID] = &pendingCall{
		action:   action,
		sentAt:   time.Now(),
		onResult: onResult,
		onError:  onError,
	}
	c.log.Info("→ out", "action", action, "msg_id", msgID, "payload", string(payloadBytes))
	c.emit(Event{
		Kind: EventOCPP, Dir: "out", OCPPType: "CALL",
		Action: action, MsgID: msgID, Payload: string(payloadBytes),
	})
	if err := c.ws.Send(ctx, raw); err != nil {
		delete(c.pending, msgID)
		return err
	}
	return nil
}

func (c *Charger) sendBoot(ctx context.Context) error {
	req := j16.BootNotificationReq{
		ChargePointVendor:       c.cfg.Vendor,
		ChargePointModel:        c.cfg.Model,
		ChargePointSerialNumber: c.cfg.CPID,
		FirmwareVersion:         c.cfg.FirmwareVersion,
	}
	return c.call(ctx, "BootNotification", req,
		func(payload json.RawMessage) {
			var resp j16.BootNotificationResp
			if err := json.Unmarshal(payload, &resp); err != nil {
				c.log.Error("boot resp decode", "err", err)
				return
			}
			switch resp.Status {
			case j16.RegAccepted:
				c.cpState = cpOnline
				if resp.Interval > 0 {
					c.bootInterval = time.Duration(resp.Interval) * time.Second
				}
				c.log.Info("boot accepted",
					"interval", c.bootInterval,
					"cms_time", resp.CurrentTime.Format(time.RFC3339))
				c.emitLog("info", "boot accepted", map[string]any{
					"interval": c.bootInterval.String(),
				})
				c.publishSnapshot()
				c.sendStatus(ctx, 0, j16.StatusAvailable, j16.ErrorNoError)
				// Re-announce each connector's CURRENT status — EXCEPT any that
				// still holds a live txID. Those had their session interrupted by
				// the outage; recoverInterruptedSessions closes them out with a
				// StopTransaction and drives them back to Available, so we skip
				// re-announcing a stale Charging here.
				for id, cn := range c.connectors {
					if cn.txID != 0 {
						continue
					}
					c.sendStatus(ctx, id, cn.status, cn.errorCode)
				}
				// Close out any session interrupted by an outage (offline button,
				// CMS restart, or process restart). Covers all three with one
				// store-driven path; a no-op on a clean first boot.
				c.recoverInterruptedSessions(ctx)
			case j16.RegPending:
				c.log.Warn("boot pending — CMS asked us to wait")
				c.emitLog("warn", "boot pending", nil)
			case j16.RegRejected:
				c.log.Error("boot rejected by CMS")
				c.emitLog("error", "boot rejected", nil)
			}
		},
		func(code, desc string) {
			c.log.Error("boot error", "code", code, "desc", desc)
			c.emitLog("error", "boot error", map[string]any{"code": code, "desc": desc})
		},
	)
}

func (c *Charger) sendHeartbeat(ctx context.Context) {
	_ = c.call(ctx, "Heartbeat", j16.HeartbeatReq{},
		func(payload json.RawMessage) {
			var resp j16.HeartbeatResp
			_ = json.Unmarshal(payload, &resp)
			c.log.Debug("heartbeat ok", "cms_time", resp.CurrentTime.Format(time.RFC3339))
		}, nil)
}

func (c *Charger) sendStatus(ctx context.Context, connectorID int, status j16.ChargePointStatus, errCode j16.ChargePointErrorCode) {
	now := time.Now().UTC()
	req := j16.StatusNotificationReq{
		ConnectorId: connectorID,
		Status:      status,
		ErrorCode:   errCode,
		Timestamp:   &now,
	}
	if connectorID == 0 {
		// connectorId=0 = whole charge point; we don't track its connector entry.
	} else if cn, ok := c.connectors[connectorID]; ok {
		cn.status = status
		cn.errorCode = errCode
	}
	c.publishSnapshot()
	_ = c.call(ctx, "StatusNotification", req, nil, nil)
}

func (c *Charger) sendStatusWithInfo(ctx context.Context, connectorID int, status j16.ChargePointStatus, errCode j16.ChargePointErrorCode, info string) {
	now := time.Now().UTC()
	req := j16.StatusNotificationReq{
		ConnectorId: connectorID,
		Status:      status,
		ErrorCode:   errCode,
		Info:        info,
		Timestamp:   &now,
	}
	if cn, ok := c.connectors[connectorID]; ok {
		cn.status = status
		cn.errorCode = errCode
	}
	c.publishSnapshot()
	_ = c.call(ctx, "StatusNotification", req, nil, nil)
}

func (c *Charger) sendAuthorize(ctx context.Context, connectorID int, idTag string, onAccepted func()) {
	// clearStart releases the in-flight start guard for this connector.
	clearStart := func() {
		if cn, ok := c.connectors[connectorID]; ok {
			cn.startPending = false
		}
	}
	_ = c.call(ctx, "Authorize", j16.AuthorizeReq{IdTag: idTag},
		func(payload json.RawMessage) {
			var resp j16.AuthorizeResp
			if err := json.Unmarshal(payload, &resp); err != nil {
				c.log.Error("authorize decode", "err", err)
				clearStart()
				return
			}
			if resp.IdTagInfo.Status == j16.AuthAccepted {
				c.log.Info("authorize accepted", "id_tag", idTag)
				c.emitLog("info", "authorize accepted", map[string]any{"id_tag": idTag})
				onAccepted()
			} else {
				c.log.Warn("authorize denied", "id_tag", idTag, "status", resp.IdTagInfo.Status)
				c.emitLog("warn", "authorize denied", map[string]any{
					"id_tag": idTag, "status": string(resp.IdTagInfo.Status),
				})
				// Leave the connector exactly as it was (gun still plugged →
				// Preparing). We never silently change the physical plug state.
				clearStart()
				c.publishSnapshot()
			}
		},
		func(code, desc string) {
			c.log.Warn("authorize error", "code", code, "desc", desc)
			c.emitLog("warn", "authorize error", map[string]any{"code": code, "desc": desc})
			clearStart()
			c.publishSnapshot()
		})
}

func (c *Charger) sendStartTransaction(ctx context.Context, connectorID int, idTag string) {
	cn, ok := c.connectors[connectorID]
	if !ok {
		c.log.Warn("startTx: unknown connector", "connector", connectorID)
		return
	}
	now := time.Now().UTC()
	meterStart := int(cn.meterEng.CurrentWh())
	req := j16.StartTransactionReq{
		ConnectorId: connectorID,
		IdTag:       idTag,
		MeterStart:  meterStart,
		Timestamp:   now,
	}
	cn.sessionStartedAt = now
	cn.sessionStartWh = meterStart
	cn.sessionIdTag = idTag

	_ = c.call(ctx, "StartTransaction", req,
		func(payload json.RawMessage) {
			var resp j16.StartTransactionResp
			if err := json.Unmarshal(payload, &resp); err != nil {
				c.log.Error("startTx decode", "err", err)
				return
			}
			if resp.IdTagInfo.Status != j16.AuthAccepted {
				c.log.Warn("startTx denied", "status", resp.IdTagInfo.Status)
				c.emitLog("warn", "startTx denied", map[string]any{"status": string(resp.IdTagInfo.Status)})
				// Gun stays plugged (Preparing); the user may retry or unplug.
				cn.startPending = false
				c.publishSnapshot()
				return
			}
			cn.txID = resp.TransactionId
			cn.startPending = false
			cn.meterEng.Start(now, float64(meterStart))
			// Persist the open session so it can be closed out with a
			// StopTransaction if the CMS link is interrupted (offline button,
			// CMS restart, or a full process restart).
			if c.store != nil {
				if err := c.store.Put(session.OpenSession{
					CPID:         c.cfg.CPID,
					ConnectorID:  connectorID,
					TxID:         resp.TransactionId,
					IdTag:        idTag,
					MeterStartWh: meterStart,
					StartedAt:    now,
					LastMeterWh:  meterStart, // no tick yet → stop==start is valid
					LastMeterAt:  now,
				}); err != nil {
					c.log.Error("persist open session failed", "tx_id", resp.TransactionId, "err", err)
				}
			}
			c.sendStatus(ctx, connectorID, j16.StatusCharging, j16.ErrorNoError)
			c.log.Info("transaction started",
				"tx_id", resp.TransactionId,
				"meter_start_wh", meterStart)
			c.emitLog("info", "transaction started", map[string]any{
				"tx_id":          resp.TransactionId,
				"meter_start_wh": meterStart,
				"id_tag":         idTag,
			})
			c.publishSnapshot()
		}, nil)
}

func (c *Charger) sendStopTransaction(ctx context.Context, connectorID int, reason j16.StopReason, afterStop func()) {
	cn, ok := c.connectors[connectorID]
	if !ok || cn.txID == 0 {
		c.log.Warn("stopTx: no active tx", "connector", connectorID)
		if afterStop != nil {
			afterStop()
		}
		return
	}
	now := time.Now().UTC()
	snap := cn.meterEng.Sample(now)
	meterStop := int(snap.EnergyWh)
	txID := cn.txID

	req := j16.StopTransactionReq{
		IdTag:         cn.sessionIdTag,
		MeterStop:     meterStop,
		Timestamp:     now,
		TransactionId: txID,
		Reason:        reason,
	}

	c.sendStatus(ctx, connectorID, j16.StatusFinishing, j16.ErrorNoError)

	_ = c.call(ctx, "StopTransaction", req,
		func(payload json.RawMessage) {
			cn.txID = 0
			// The session is closed on the CMS — drop its persisted record so it
			// is not replayed as an interrupted session on the next boot.
			if c.store != nil {
				_ = c.store.Delete(c.cfg.CPID, connectorID)
			}
			wh := meterStop - cn.sessionStartWh
			c.log.Info("transaction stopped",
				"tx_id", txID,
				"energy_wh", wh,
				"duration", time.Since(cn.sessionStartedAt).Round(time.Second),
				"reason", reason)
			c.emitLog("info", "transaction stopped", map[string]any{
				"tx_id":     txID,
				"energy_wh": wh,
				"duration":  time.Since(cn.sessionStartedAt).Round(time.Second).String(),
				"reason":    string(reason),
			})
			cn.sessionIdTag = ""
			cn.sessionStartedAt = time.Time{}
			cn.sessionStartWh = 0
			c.publishSnapshot()
			if afterStop != nil {
				afterStop()
			}
		}, nil)
}

// recoverInterruptedSessions closes out every session that was open when the CMS
// link was last interrupted. It is called on every boot-accepted, so it covers
// all three interruption modes with one path: the operator offline/online
// button, an unexpected CMS drop (via auto-reconnect), and a full process
// restart (where the in-memory txIDs are gone but the store still holds the
// records). Each recovered session is closed with StopTransaction(PowerLoss).
func (c *Charger) recoverInterruptedSessions(ctx context.Context) {
	if c.store == nil {
		return
	}
	open, err := c.store.LoadOpen(c.cfg.CPID)
	if err != nil {
		c.log.Error("recover: load open sessions failed", "err", err)
		return
	}
	for _, rec := range open {
		c.log.Warn("recovering interrupted session → StopTransaction(PowerLoss)",
			"tx_id", rec.TxID, "connector", rec.ConnectorID)
		c.emitLog("warn", "recovering session interrupted by outage (PowerLoss)", map[string]any{
			"tx_id": rec.TxID, "connector": rec.ConnectorID,
		})
		c.stopTransactionFromRecord(ctx, rec)
	}
}

// stopTransactionFromRecord sends a StopTransaction built purely from a persisted
// record (no live session state required), then — on the CMS's CALLRESULT —
// deletes the record and returns the connector to Available. Deleting only on
// the reply makes recovery at-least-once: if the link drops again before the
// reply, the identical StopTransaction is replayed on the next boot (a real CMS
// dedupes by transactionId).
func (c *Charger) stopTransactionFromRecord(ctx context.Context, rec session.OpenSession) {
	connID := rec.ConnectorID
	req := j16.StopTransactionReq{
		IdTag:         rec.IdTag,
		MeterStop:     rec.LastMeterWh, // last reading seen before the outage
		Timestamp:     rec.LastMeterAt,
		TransactionId: rec.TxID,
		Reason:        j16.StopReasonPowerLoss,
	}
	_ = c.call(ctx, "StopTransaction", req,
		func(payload json.RawMessage) {
			if c.store != nil {
				if err := c.store.Delete(rec.CPID, connID); err != nil {
					c.log.Error("recover: delete record failed", "tx_id", rec.TxID, "err", err)
				}
			}
			c.log.Info("interrupted session closed on CMS", "tx_id", rec.TxID, "connector", connID)
			c.emitLog("info", "interrupted session closed on CMS", map[string]any{
				"tx_id": rec.TxID, "connector": connID,
			})
			// Reconcile in-memory state whether or not this connector still holds
			// the txID (same-process outage → it does; cold restart → it doesn't).
			if cn, ok := c.connectors[connID]; ok {
				cn.txID = 0
				cn.startPending = false
				cn.sessionIdTag = ""
				cn.sessionStartedAt = time.Time{}
				cn.sessionStartWh = 0
				c.sendStatus(ctx, connID, j16.StatusAvailable, j16.ErrorNoError)
			} else {
				// Connector no longer exists in the current config (e.g. 2 guns
				// → 1). The tx is closed on the CMS; nothing to reconcile.
				c.log.Warn("recover: connector absent in current config; tx closed, no reconcile",
					"connector", connID)
				c.publishSnapshot()
			}
		}, nil)
}

func (c *Charger) sendMeterValues(ctx context.Context, connectorID int) {
	cn, ok := c.connectors[connectorID]
	if !ok || cn.txID == 0 {
		return
	}
	snap := cn.meterEng.Sample(time.Now().UTC())
	txID := cn.txID

	// Refresh the persisted last-seen meter reading. Done before the fault
	// branch so the record still advances when telemetry is suppressed, and
	// only while online — so on an outage the record freezes at this value,
	// which the recovery StopTransaction reports as meterStop.
	if c.store != nil {
		_ = c.store.UpdateMeter(c.cfg.CPID, connectorID, int(snap.EnergyWh), snap.At)
	}

	// Inject any armed energy-meter fault. send=false means a total telemetry
	// blackout — we drop the whole MeterValues CALL (heartbeats keep flowing).
	mv, send := meter.ApplyFault(snap.ToOCPP(), cn.meterFault)
	if !send {
		c.log.Warn("meter fault: suppressing MeterValues",
			"connector", connectorID, "tx_id", txID, "mode", string(cn.meterFault))
		c.emitLog("warn", "energy-meter fault — MeterValues suppressed", map[string]any{
			"connector": connectorID, "mode": string(cn.meterFault),
		})
		// Integrator already advanced; refresh the dashboard so it shows the
		// real telemetry alongside the active-fault badge.
		c.publishSnapshot()
		return
	}

	req := j16.MeterValuesReq{
		ConnectorId:   connectorID,
		TransactionId: &txID,
		MeterValue:    []j16.MeterValue{mv},
	}
	_ = c.call(ctx, "MeterValues", req, nil, nil)
	c.log.Info("meter sample",
		"connector", connectorID,
		"tx_id", txID,
		"fault", string(cn.meterFault),
		"energy_wh", int(snap.EnergyWh),
		"power_w", int(snap.PowerW),
		"current_a", fmt.Sprintf("%.1f", snap.CurrentA),
		"voltage_v", fmt.Sprintf("%.1f", snap.VoltageV),
		"soc", fmt.Sprintf("%.1f", snap.SoC))
	// Engine.Sample already stored the snapshot internally; publishSnapshot
	// picks it up via cn.meterEng.LastSnap().
	c.publishSnapshot()
}

// ---------------------------------------------------------------------------
// Inbound handling
// ---------------------------------------------------------------------------

func (c *Charger) handleInbound(ctx context.Context, data []byte) {
	f, err := j16.Decode(data)
	if err != nil {
		c.log.Warn("← decode error", "err", err, "raw", string(data))
		return
	}
	switch f.Type {
	case j16.CALLRESULT:
		pc, ok := c.pending[f.CallResult.MessageID]
		if !ok {
			c.log.Warn("← unmatched result", "msg_id", f.CallResult.MessageID)
			return
		}
		delete(c.pending, f.CallResult.MessageID)
		latency := time.Since(pc.sentAt).Milliseconds()
		c.log.Info("← in",
			"type", "CALLRESULT",
			"action", pc.action,
			"msg_id", f.CallResult.MessageID,
			"payload", string(f.CallResult.Payload),
			"rt_ms", latency)
		c.emit(Event{
			Kind: EventOCPP, Dir: "in", OCPPType: "CALLRESULT",
			Action: pc.action, MsgID: f.CallResult.MessageID,
			Payload: string(f.CallResult.Payload), LatencyMs: latency,
		})
		if pc.onResult != nil {
			pc.onResult(f.CallResult.Payload)
		}
	case j16.CALLERROR:
		pc, ok := c.pending[f.CallError.MessageID]
		if !ok {
			c.log.Warn("← unmatched error", "msg_id", f.CallError.MessageID)
			return
		}
		delete(c.pending, f.CallError.MessageID)
		c.log.Warn("← in",
			"type", "CALLERROR",
			"action", pc.action,
			"code", f.CallError.ErrorCode,
			"desc", f.CallError.ErrorDescription)
		c.emit(Event{
			Kind: EventOCPP, Dir: "in", OCPPType: "CALLERROR",
			Action: pc.action, MsgID: f.CallError.MessageID,
			ErrorCode: f.CallError.ErrorCode, ErrorDesc: f.CallError.ErrorDescription,
		})
		if pc.onError != nil {
			pc.onError(f.CallError.ErrorCode, f.CallError.ErrorDescription)
		}
	case j16.CALL:
		c.handleIncomingCall(ctx, f.Call)
	}
}

func (c *Charger) handleIncomingCall(ctx context.Context, call *j16.Call) {
	c.log.Info("← in",
		"type", "CALL",
		"action", call.Action,
		"msg_id", call.MessageID,
		"payload", string(call.Payload))
	c.emit(Event{
		Kind: EventOCPP, Dir: "in", OCPPType: "CALL",
		Action: call.Action, MsgID: call.MessageID, Payload: string(call.Payload),
	})

	switch call.Action {
	case "RemoteStartTransaction":
		var req j16.RemoteStartTransactionReq
		_ = json.Unmarshal(call.Payload, &req)
		connID := 1
		if req.ConnectorId != nil {
			connID = *req.ConnectorId
		}
		// Reply truthfully: only Accept if the charger could actually start
		// (online, gun plugged in, no session already running). Otherwise the
		// CMS would think a start succeeded when the charger silently dropped it.
		if okStart, reason := c.canStartCharging(connID); !okStart {
			c.log.Warn("RemoteStartTransaction rejected", "connector", connID, "reason", reason)
			c.emitLog("warn", "remote start rejected: "+reason, map[string]any{"connector": connID})
			c.replyResult(ctx, call.MessageID, j16.RemoteStartTransactionResp{Status: "Rejected"})
			return
		}
		c.replyResult(ctx, call.MessageID, j16.RemoteStartTransactionResp{Status: "Accepted"})
		c.enqueue(CmdStartCharging{ConnectorID: connID, IdTag: req.IdTag})

	case "RemoteStopTransaction":
		var req j16.RemoteStopTransactionReq
		_ = json.Unmarshal(call.Payload, &req)
		connID := 0
		for id, cn := range c.connectors {
			if cn.txID == req.TransactionId {
				connID = id
				break
			}
		}
		if connID == 0 {
			c.replyResult(ctx, call.MessageID, j16.RemoteStopTransactionResp{Status: "Rejected"})
			return
		}
		c.replyResult(ctx, call.MessageID, j16.RemoteStopTransactionResp{Status: "Accepted"})
		c.enqueue(CmdStopCharging{ConnectorID: connID})

	case "Reset":
		c.replyResult(ctx, call.MessageID, j16.ResetResp{Status: "Accepted"})
		c.log.Warn("reset received — slice 2 logs only; full reboot lands later")
		c.emitLog("warn", "reset received (no-op in slice 2)", nil)

	case "ChangeAvailability":
		c.replyResult(ctx, call.MessageID, j16.ChangeAvailabilityResp{Status: "Accepted"})

	case "TriggerMessage":
		var req j16.TriggerMessageReq
		_ = json.Unmarshal(call.Payload, &req)
		c.replyResult(ctx, call.MessageID, j16.TriggerMessageResp{Status: "Accepted"})
		c.handleTrigger(ctx, req)

	case "UnlockConnector":
		c.replyResult(ctx, call.MessageID, j16.UnlockConnectorResp{Status: "Unlocked"})

	default:
		c.replyError(ctx, call.MessageID, "NotImplemented", "not implemented: "+call.Action)
	}
}

func (c *Charger) handleTrigger(ctx context.Context, req j16.TriggerMessageReq) {
	switch req.RequestedMessage {
	case "BootNotification":
		_ = c.sendBoot(ctx)
	case "Heartbeat":
		c.sendHeartbeat(ctx)
	case "StatusNotification":
		id := 1
		if req.ConnectorId != nil {
			id = *req.ConnectorId
		}
		if cn, ok := c.connectors[id]; ok {
			c.sendStatus(ctx, id, cn.status, cn.errorCode)
		}
	case "MeterValues":
		id := 1
		if req.ConnectorId != nil {
			id = *req.ConnectorId
		}
		c.sendMeterValues(ctx, id)
	}
}

func (c *Charger) replyResult(ctx context.Context, msgID string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		c.log.Error("marshal result", "err", err)
		return
	}
	frame := j16.CallResult{MessageID: msgID, Payload: b}
	raw, _ := frame.Marshal()
	if err := c.ws.Send(ctx, raw); err != nil {
		c.log.Error("send result", "err", err)
		return
	}
	c.log.Info("→ out", "type", "CALLRESULT", "msg_id", msgID, "payload", string(b))
	c.emit(Event{
		Kind: EventOCPP, Dir: "out", OCPPType: "CALLRESULT",
		MsgID: msgID, Payload: string(b),
	})
}

func (c *Charger) replyError(ctx context.Context, msgID, code, desc string) {
	frame := j16.CallError{
		MessageID:        msgID,
		ErrorCode:        code,
		ErrorDescription: desc,
		ErrorDetails:     json.RawMessage(`{}`),
	}
	raw, _ := frame.Marshal()
	if err := c.ws.Send(ctx, raw); err != nil {
		c.log.Error("send error", "err", err)
		return
	}
	c.log.Warn("→ out", "type", "CALLERROR", "msg_id", msgID, "code", code)
	c.emit(Event{
		Kind: EventOCPP, Dir: "out", OCPPType: "CALLERROR",
		MsgID: msgID, ErrorCode: code, ErrorDesc: desc,
	})
}

// ---------------------------------------------------------------------------
// Command handlers (run on the actor goroutine)
// ---------------------------------------------------------------------------

func (c *Charger) handleCommand(ctx context.Context, cmd Command) {
	switch v := cmd.(type) {
	case CmdPlugIn:
		c.handlePlugIn(ctx, v)
	case CmdStartCharging:
		c.handleStartCharging(ctx, v)
	case CmdStopCharging:
		c.handleStopCharging(ctx, v)
	case CmdUnplug:
		c.handleUnplug(ctx, v)
	case CmdEmergencyStop:
		c.handleEmergencyStop(ctx, v)
	case CmdClearFault:
		c.handleClearFault(ctx, v)
	case CmdSetPower:
		c.handleSetPower(v)
	case CmdSetMeterFault:
		c.handleSetMeterFault(v)
	case CmdGoOffline:
		c.handleGoOffline()
	case CmdGoOnline:
		// GoOnline is intercepted in Run() so it can swap the inbound channel.
		// Reaching here means it slipped past that interception — ignore safely.
		c.log.Warn("goOnline reached handleCommand; expected interception in Run loop")
	default:
		c.log.Warn("unknown command", "type", fmt.Sprintf("%T", cmd))
	}
}

// handleGoOffline simulates the charger losing its uplink: it drops the CMS
// WebSocket (so the CMS observes a disconnect), flips into the Offline state,
// and discards in-flight calls. Connector/session state is left intact so a
// later GoOnline can resume cleanly. The Run loop notices the inbound channel
// close, sees c.offline, and keeps the actor alive instead of exiting.
func (c *Charger) handleGoOffline() {
	if c.offline {
		c.log.Info("goOffline: already offline")
		return
	}
	c.log.Warn("[event] GOING OFFLINE (simulated wifi/power loss)")
	c.emitLog("warn", "charger going offline (simulated wifi/power loss) — CMS will see it disconnect", nil)
	c.offline = true
	c.cpState = cpOffline
	// In-flight CALLs will never get a reply over the dropped socket.
	c.pending = make(map[string]*pendingCall)
	c.publishSnapshot()
	// Actually drop the link. readLoop will close the inbound channel, which the
	// Run loop handles without tearing the charger down.
	c.ws.Close()
}

// handleGoOnline ends a simulated outage. It builds a fresh WS client (the old
// one is single-use after Close), re-dials the CMS, and re-boots. It returns
// the new inbound channel for the Run loop to select on; on failure it returns
// nil and the charger stays offline so the operator can retry.
func (c *Charger) handleGoOnline(ctx context.Context) <-chan []byte {
	if !c.offline {
		c.log.Info("goOnline: already online")
		return c.ws.Inbound()
	}
	c.log.Info("[event] GOING ONLINE — reconnecting to CMS", "url", c.cfg.CMSURL)
	c.emitLog("info", "charger reconnecting to CMS", map[string]any{"url": c.cfg.CMSURL})
	c.cpState = cpBooting
	c.publishSnapshot()

	// Manual reconnect is a SINGLE attempt — the operator retries via the button
	// if it fails. (Unexpected drops use reconnectWithBackoff instead.)
	if err := c.dialAndSwap(ctx); err != nil {
		c.log.Error("reconnect failed", "err", err)
		c.emitLog("error", "reconnect failed — still offline", map[string]any{"err": err.Error()})
		c.cpState = cpOffline // stay offline; operator can retry GoOnline
		c.publishSnapshot()
		return nil
	}
	c.offline = false
	c.emitLog("info", "reconnected — sending BootNotification", nil)
	if err := c.sendBoot(ctx); err != nil {
		c.log.Error("reconnect boot failed", "err", err)
		c.emitLog("error", "boot after reconnect failed", map[string]any{"err": err.Error()})
	}
	return c.ws.Inbound()
}

// Backoff defaults for auto-reconnect after an unexpected CMS drop. Overridable
// per charger via config.ChargerConfig.Reconnect{Initial,Max}Backoff.
const (
	defaultReconnectInitial = 1 * time.Second
	defaultReconnectMax     = 30 * time.Second
)

// dialAndSwap builds a fresh ws.Client (the old one is single-use after Close),
// copies the auth creds, dials, and on success swaps it into c.ws. It does NOT
// send BootNotification. Shared by the manual GoOnline path and auto-reconnect.
func (c *Charger) dialAndSwap(ctx context.Context) error {
	wsClient := ws.New(c.cfg.CMSURL, c.log.With("layer", "ws"))
	wsClient.BasicUser = c.cfg.BasicAuthUser
	wsClient.BasicPass = c.cfg.BasicAuthPass
	if err := wsClient.Connect(ctx); err != nil {
		return err
	}
	c.ws = wsClient
	return nil
}

// onUnexpectedDrop transitions into the auto-reconnecting state after the CMS
// link is lost unexpectedly. Session state (txID, connector status) is kept; the
// persisted records already hold the last-online meter reading, so no in-memory
// freeze is needed. In-flight CALLs are discarded — their replies will never
// arrive over the dead socket.
func (c *Charger) onUnexpectedDrop() {
	c.cpState = cpReconnecting
	c.pending = make(map[string]*pendingCall)
	c.emitLog("warn", "CMS link lost — auto-reconnecting with backoff", nil)
	c.publishSnapshot()
}

// reconnectWithBackoff loops dial→boot until it succeeds (returning the new
// inbound channel) or ctx is cancelled (returning nil). The wait between
// attempts is the only place the actor blocks, and it stays cancellable via
// ctx.Done(). A successful BootNotification triggers recoverInterruptedSessions
// in the RegAccepted branch, closing out any session the outage interrupted.
func (c *Charger) reconnectWithBackoff(ctx context.Context) <-chan []byte {
	backoff := c.cfg.ReconnectInitialBackoff
	if backoff <= 0 {
		backoff = defaultReconnectInitial
	}
	maxB := c.cfg.ReconnectMaxBackoff
	if maxB <= 0 {
		maxB = defaultReconnectMax
	}
	for attempt := 1; ; attempt++ {
		if err := c.dialAndSwap(ctx); err == nil {
			// Dial up. Boot; RegAccepted flips cpState to online and runs
			// recovery. Leave cpReconnecting until the boot RESULT lands.
			if err := c.sendBoot(ctx); err != nil {
				c.log.Error("reconnect boot send failed", "err", err)
				c.ws.Close() // fall through to another backoff cycle
			} else {
				c.emitLog("info", "reconnected — BootNotification sent", map[string]any{"attempt": attempt})
				return c.ws.Inbound()
			}
		} else {
			c.log.Warn("reconnect attempt failed",
				"attempt", attempt, "backoff", backoff.String(), "err", err)
			c.emitLog("warn", "reconnect attempt failed", map[string]any{
				"attempt": attempt, "backoff": backoff.String(),
			})
		}

		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
		if backoff *= 2; backoff > maxB {
			backoff = maxB
		}
	}
}

func (c *Charger) handlePlugIn(ctx context.Context, v CmdPlugIn) {
	cn, ok := c.connectors[v.ConnectorID]
	if !ok {
		c.log.Warn("plugIn: unknown connector", "connector", v.ConnectorID)
		return
	}
	if c.cpState != cpOnline {
		c.log.Warn("plugIn: charger not online yet", "state", c.cpState)
		c.emitLog("warn", "plug-in ignored: charger not online", nil)
		return
	}
	if cn.status != j16.StatusAvailable {
		c.log.Warn("plugIn: connector not Available",
			"connector", v.ConnectorID, "status", cn.status)
		c.emitLog("warn", "plug-in ignored: connector not Available", map[string]any{
			"status": string(cn.status),
		})
		return
	}
	c.log.Info("[event] plug-in", "connector", v.ConnectorID)
	c.emitLog("info", "gun plugged in", map[string]any{"connector": v.ConnectorID})
	c.sendStatus(ctx, v.ConnectorID, j16.StatusPreparing, j16.ErrorNoError)
}

// handleUnplug is the symmetric inverse of handlePlugIn: it only acts in
// Preparing. For a Charging connector the caller should use CmdStopCharging.
func (c *Charger) handleUnplug(ctx context.Context, v CmdUnplug) {
	cn, ok := c.connectors[v.ConnectorID]
	if !ok {
		c.log.Warn("unplug: unknown connector", "connector", v.ConnectorID)
		return
	}
	if c.cpState != cpOnline {
		c.log.Warn("unplug: charger not online yet", "state", c.cpState)
		c.emitLog("warn", "unplug ignored: charger not online", nil)
		return
	}
	if cn.status != j16.StatusPreparing {
		c.log.Warn("unplug: connector not Preparing",
			"connector", v.ConnectorID, "status", cn.status)
		c.emitLog("warn", "unplug ignored: connector not Preparing", map[string]any{
			"status": string(cn.status),
		})
		return
	}
	c.log.Info("[event] unplug", "connector", v.ConnectorID)
	c.emitLog("info", "gun unplugged", map[string]any{"connector": v.ConnectorID})
	c.sendStatus(ctx, v.ConnectorID, j16.StatusAvailable, j16.ErrorNoError)
}

func (c *Charger) handleStartCharging(ctx context.Context, v CmdStartCharging) {
	cn, ok := c.connectors[v.ConnectorID]
	if !ok {
		c.log.Warn("startCharging: unknown connector", "connector", v.ConnectorID)
		return
	}

	// All the "can we start?" rules live in canStartCharging so the CMS-facing
	// RemoteStartTransaction reply and this local path stay in lockstep.
	if okStart, reason := c.canStartCharging(v.ConnectorID); !okStart {
		c.log.Warn("startCharging rejected", "connector", v.ConnectorID, "reason", reason, "status", cn.status)
		c.emitLog("warn", "start rejected: "+reason, map[string]any{
			"connector": v.ConnectorID, "status": string(cn.status),
		})
		return
	}

	idTag := v.IdTag
	if idTag == "" {
		idTag = "TESTRFID0001"
	}
	cn.startPending = true
	c.log.Info("[event] start charging", "connector", v.ConnectorID, "id_tag", idTag)
	c.emitLog("info", "start charging requested", map[string]any{
		"connector": v.ConnectorID, "id_tag": idTag,
	})
	c.sendAuthorize(ctx, v.ConnectorID, idTag, func() {
		c.sendStartTransaction(ctx, v.ConnectorID, idTag)
	})
}

// canStartCharging reports whether a StartCharging on this connector would be
// honoured right now, with a human-readable reason when it would not. It is the
// single source of truth for the start guards so both the local StartCharging
// path and the CMS-initiated RemoteStartTransaction reply agree. The rules:
//
//   - charger must be online (rejects Offline / Booting / Disconnected);
//   - the connector must not already have a session (no stop-and-restart);
//   - the gun must be plugged in — Preparing (we never auto-plug);
//   - any other status (Faulted, Reserved, …) is not startable;
//   - no other start may be mid-handshake on this connector.
func (c *Charger) canStartCharging(connectorID int) (bool, string) {
	cn, ok := c.connectors[connectorID]
	if !ok {
		return false, "unknown connector"
	}
	if c.cpState != cpOnline {
		return false, c.connectivityReason()
	}
	if cn.txID != 0 || cn.status == j16.StatusCharging || cn.status == j16.StatusFinishing {
		return false, "a charging session is already in progress on this connector"
	}
	if cn.status == j16.StatusAvailable {
		return false, "gun not plugged in"
	}
	if cn.status != j16.StatusPreparing {
		return false, "connector not ready (status " + string(cn.status) + ")"
	}
	if cn.startPending {
		return false, "a start request is already being processed"
	}
	return true, ""
}

// connectivityReason returns a human-readable explanation for why the charger
// can't act on a user command, based on the current CP lifecycle state.
func (c *Charger) connectivityReason() string {
	switch c.cpState {
	case cpOffline:
		return "charger is offline"
	case cpBooting:
		return "charger not online yet"
	case cpReconnecting:
		return "charger reconnecting to CMS"
	case cpDisconnected:
		return "charger disconnected"
	default:
		return "charger not online"
	}
}

func (c *Charger) handleStopCharging(ctx context.Context, v CmdStopCharging) {
	cn, ok := c.connectors[v.ConnectorID]
	if !ok || cn.txID == 0 {
		c.log.Warn("stopCharging: no active tx", "connector", v.ConnectorID)
		c.emitLog("warn", "stop ignored: no active transaction", nil)
		return
	}
	c.log.Info("[event] stop charging", "connector", v.ConnectorID)
	c.emitLog("info", "stop charging requested", map[string]any{"connector": v.ConnectorID})
	c.sendStopTransaction(ctx, v.ConnectorID, j16.StopReasonLocal, func() {
		c.sendStatus(ctx, v.ConnectorID, j16.StatusAvailable, j16.ErrorNoError)
	})
}

func (c *Charger) handleEmergencyStop(ctx context.Context, v CmdEmergencyStop) {
	cn, ok := c.connectors[v.ConnectorID]
	if !ok {
		return
	}
	c.log.Warn("[event] EMERGENCY STOP", "connector", v.ConnectorID)
	c.emitLog("warn", "EMERGENCY STOP triggered", map[string]any{"connector": v.ConnectorID})
	finish := func() {
		c.sendStatusWithInfo(ctx, v.ConnectorID, j16.StatusFaulted, j16.ErrorOtherError, "EmergencyStop")
	}
	if cn.txID != 0 {
		c.sendStopTransaction(ctx, v.ConnectorID, j16.StopReasonEmergencyStop, finish)
		return
	}
	finish()
}

func (c *Charger) handleClearFault(ctx context.Context, v CmdClearFault) {
	cn, ok := c.connectors[v.ConnectorID]
	if !ok {
		return
	}
	if cn.status != j16.StatusFaulted {
		c.log.Warn("clearFault: connector not Faulted",
			"connector", v.ConnectorID, "status", cn.status)
		return
	}
	c.log.Info("[event] clear fault", "connector", v.ConnectorID)
	c.emitLog("info", "fault cleared", map[string]any{"connector": v.ConnectorID})
	c.sendStatus(ctx, v.ConnectorID, j16.StatusAvailable, j16.ErrorNoError)
}

func (c *Charger) handleSetPower(v CmdSetPower) {
	w := v.Watts
	if w < 0 {
		w = 0
	}
	if w > c.cfg.MaxPowerW {
		w = c.cfg.MaxPowerW
	}
	// Apply the new ceiling to every connector. Real CMS Smart-Charging
	// profiles can be per-connector; that's slice 5.
	for _, cn := range c.connectors {
		cn.meterEng.SetMaxPower(w)
	}
	c.log.Info("power limit changed", "watts", int(w))
	c.emitLog("info", "power limit changed", map[string]any{"watts": int(w)})
	c.publishSnapshot()
}

func (c *Charger) handleSetMeterFault(v CmdSetMeterFault) {
	cn, ok := c.connectors[v.ConnectorID]
	if !ok {
		c.log.Warn("setMeterFault: unknown connector", "connector", v.ConnectorID)
		return
	}
	mode := v.Mode
	if mode == "" {
		mode = meter.FaultNone
	}
	if !meter.ValidFault(string(mode)) {
		c.log.Warn("setMeterFault: unknown fault mode", "mode", string(mode))
		c.emitLog("warn", "meter fault ignored: unknown mode", map[string]any{"mode": string(mode)})
		return
	}
	cn.meterFault = mode
	if mode == meter.FaultNone {
		c.log.Info("meter fault cleared", "connector", v.ConnectorID)
		c.emitLog("info", "energy-meter fault cleared", map[string]any{"connector": v.ConnectorID})
	} else {
		c.log.Warn("meter fault armed", "connector", v.ConnectorID, "mode", string(mode))
		c.emitLog("warn", "energy-meter fault armed", map[string]any{
			"connector": v.ConnectorID, "mode": string(mode),
		})
	}
	c.publishSnapshot()
}

// ---------------------------------------------------------------------------
// Event emission + snapshot publication
// ---------------------------------------------------------------------------

func (c *Charger) emit(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	select {
	case c.events <- e:
	default:
		// drop on slow consumer; the next snapshot/event will catch them up.
	}
}

func (c *Charger) emitLog(level, msg string, fields map[string]any) {
	c.emit(Event{Kind: EventLog, Level: level, Msg: msg, Fields: fields})
}

// publishSnapshot writes a fresh snapshot from current state (no meter advance)
// and emits a state event. Engine.Sample stores its own LastSnap, so the meter
// values for live V/I/P/SoC are read straight from there.
func (c *Charger) publishSnapshot() {
	snap := c.buildSnapshotFromState()
	c.snapshot.Store(&snap)
	c.emit(Event{Kind: EventState, State: &snap})
}

// buildSnapshotFromState assembles the full state view: identity, lifecycle,
// every connector's per-session telemetry, plus a mirror of connector 1 at the
// top level for back-compat with slice 2/3 dashboards.
func (c *Charger) buildSnapshotFromState() StateSnapshot {
	chargerType := string(config.ChargerTypeAC)
	if c.cfg.IsDC() {
		chargerType = string(config.ChargerTypeDC)
	}

	// Per-connector snapshots — sort by ID for deterministic JSON.
	cons := make([]ConnectorSnapshot, 0, len(c.connectors))
	for _, cn := range c.connectors {
		cons = append(cons, c.connectorSnapshot(cn))
	}
	sort.Slice(cons, func(i, j int) bool { return cons[i].ID < cons[j].ID })

	snap := StateSnapshot{
		CPID:          c.cfg.CPID,
		Vendor:        c.cfg.Vendor,
		Model:         c.cfg.Model,
		CMSURL:        c.cfg.CMSURL,
		ChargerType:   chargerType,
		NumConnectors: len(c.connectors),
		Phases:        c.cfg.Phases,
		NominalV:      c.cfg.NominalVoltageV,
		Connected:     c.cpState == cpOnline,
		CPState:       c.cpState.String(),
		Connectors:    cons,
		UpdatedAt:     time.Now().UTC().Unix(),
	}

	// Mirror connector 1's fields at the top level so slice 2/3 frontends and
	// curl scripts that read `state.energy_wh` etc. keep working.
	if cn1, ok := c.connectors[1]; ok {
		mirrorConnectorToTopLevel(&snap, cn1, c.connectorSnapshot(cn1))
	} else {
		snap.ConnectorStatus = string(j16.StatusAvailable)
	}
	return snap
}

// connectorSnapshot turns one connector into a ConnectorSnapshot using its
// engine's last sample for live V/I/P/SoC.
func (c *Charger) connectorSnapshot(cn *connector) ConnectorSnapshot {
	cs := ConnectorSnapshot{
		ID:                  cn.id,
		Status:              string(cn.status),
		TransactionID:       cn.txID,
		MaxPowerWConfigured: c.cfg.MaxPowerW,
	}
	if cn.errorCode != j16.ErrorNoError && cn.errorCode != "" {
		cs.ErrorCode = string(cn.errorCode)
	}
	if cn.meterFault != "" && cn.meterFault != meter.FaultNone {
		cs.MeterFault = string(cn.meterFault)
	}
	if cn.meterEng != nil {
		cs.EnergyWh = int(cn.meterEng.CurrentWh())
		cs.MaxPowerWCurrent = cn.meterEng.MaxPowerW
		if last := cn.meterEng.LastSnap(); !last.At.IsZero() {
			cs.PowerW = last.PowerW
			cs.VoltageV = last.VoltageV
			cs.CurrentA = last.CurrentA
			cs.SoC = last.SoC
		}
	}
	if cn.txID != 0 && !cn.sessionStartedAt.IsZero() {
		cs.IdTag = cn.sessionIdTag
		cs.SessionStartUnix = cn.sessionStartedAt.Unix()
		cs.SessionEnergyWh = cs.EnergyWh - cn.sessionStartWh
	}
	return cs
}

func mirrorConnectorToTopLevel(snap *StateSnapshot, cn *connector, cs ConnectorSnapshot) {
	snap.ConnectorStatus = cs.Status
	snap.ConnectorErrorCode = cs.ErrorCode
	snap.TransactionID = cs.TransactionID
	snap.IdTag = cs.IdTag
	snap.EnergyWh = cs.EnergyWh
	snap.PowerW = cs.PowerW
	snap.VoltageV = cs.VoltageV
	snap.CurrentA = cs.CurrentA
	snap.SoC = cs.SoC
	snap.SessionStartUnix = cs.SessionStartUnix
	snap.SessionEnergyWh = cs.SessionEnergyWh
	snap.MaxPowerWConfigured = cs.MaxPowerWConfigured
	snap.MaxPowerWCurrent = cs.MaxPowerWCurrent
	snap.MeterFault = cs.MeterFault
}
