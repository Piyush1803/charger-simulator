// Package api exposes the worker's HTTP/WS surface: the dashboard's REST
// endpoints (/api/*), a WebSocket event stream (/api/events), and the embedded
// static site at /.
//
// The Server owns at most one *actor.Charger at a time. POST /api/configure
// replaces the existing charger (cancelling its context and starting a fresh
// one); connected WS clients keep their sockets and start receiving the new
// charger's events transparently.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/tobor/charger-simulator/internal/actor"
	"github.com/tobor/charger-simulator/internal/api/web"
	"github.com/tobor/charger-simulator/internal/config"
	"github.com/tobor/charger-simulator/internal/sim/meter"
)

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

// wsClient is the server's view of one connected dashboard browser tab.
// outCh is buffered so a slow client cannot block the fanout goroutine; if the
// buffer is full we drop events for that client.
type wsClient struct {
	outCh chan []byte
}

// Server is the dashboard's HTTP/WS host. It owns at most one Charger.
type Server struct {
	log *slog.Logger

	mu             sync.RWMutex
	current        *actor.Charger
	cancelCharger  context.CancelFunc
	currentCfg     *config.ChargerConfig
	fanoutCancel   context.CancelFunc

	clients map[*wsClient]struct{}
}

// New constructs an idle Server. No charger is running until /api/configure.
func New(log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		log:     log,
		clients: make(map[*wsClient]struct{}),
	}
}

// Routes wires every URL the worker serves. Mount the result under your
// http.Server.Handler — there is no per-request global state to share.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", s.serveIndex)
	mux.HandleFunc("/static/", s.serveStatic)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/configure", s.handleConfigure)
	mux.HandleFunc("/api/disconnect", s.handleDisconnect)

	mux.HandleFunc("/api/plug-in", s.handleConnectorAction(func(c *actor.Charger, id int) { c.PlugIn(id) }))
	mux.HandleFunc("/api/unplug", s.handleConnectorAction(func(c *actor.Charger, id int) { c.Unplug(id) }))
	mux.HandleFunc("/api/start-charging", s.handleStartCharging)
	mux.HandleFunc("/api/stop-charging", s.handleConnectorAction(func(c *actor.Charger, id int) { c.StopCharging(id) }))
	mux.HandleFunc("/api/emergency-stop", s.handleConnectorAction(func(c *actor.Charger, id int) { c.EmergencyStop(id) }))
	mux.HandleFunc("/api/clear-fault", s.handleConnectorAction(func(c *actor.Charger, id int) { c.ClearFault(id) }))
	mux.HandleFunc("/api/set-power", s.handleSetPower)
	mux.HandleFunc("/api/meter-fault", s.handleMeterFault)
	mux.HandleFunc("/api/go-offline", s.handleAction(func(c *actor.Charger) { c.GoOffline() }))
	mux.HandleFunc("/api/go-online", s.handleAction(func(c *actor.Charger) { c.GoOnline() }))

	mux.HandleFunc("/api/events", s.handleWS)

	return mux
}

// Close tears down the active charger (if any), stops the fanout, and closes
// every dashboard WS connection. Safe to call multiple times.
func (s *Server) Close() {
	s.mu.Lock()
	if s.cancelCharger != nil {
		s.cancelCharger()
		s.cancelCharger = nil
	}
	if s.fanoutCancel != nil {
		s.fanoutCancel()
		s.fanoutCancel = nil
	}
	s.current = nil
	s.currentCfg = nil
	for cl := range s.clients {
		// Closing outCh tells the per-client write loop to exit, which closes
		// the underlying websocket.
		close(cl.outCh)
		delete(s.clients, cl)
	}
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Static assets
// ---------------------------------------------------------------------------

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := web.FS.ReadFile("index.html")
	if err != nil {
		s.log.Error("read index.html", "err", err)
		http.Error(w, "index unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	// /static/<name> — only the two known files are exposed.
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	if name == "" || strings.ContainsRune(name, '/') {
		http.NotFound(w, r)
		return
	}
	var ctype string
	switch name {
	case "styles.css":
		ctype = "text/css; charset=utf-8"
	case "app.js":
		ctype = "application/javascript; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	b, err := web.FS.ReadFile(name)
	if err != nil {
		s.log.Error("read static", "name", name, "err", err)
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

// ---------------------------------------------------------------------------
// REST handlers
// ---------------------------------------------------------------------------

// configureRequest is the body of POST /api/configure.
// Field names match the form input names in the dashboard.
type configureRequest struct {
	CMSURL      string  `json:"cms_url"`
	CPID        string  `json:"cp_id"`
	User        string  `json:"user"`
	Pass        string  `json:"pass"`
	Vendor      string  `json:"vendor"`
	Model       string  `json:"model"`
	FirmwareVer string  `json:"firmware_version"`
	MaxKW       float64 `json:"max_kw"`
	Voltage     float64 `json:"voltage"`
	HeartbeatS  int     `json:"heartbeat_s"`
	MeterS      int     `json:"meter_s"`

	// Slice 4 — topology
	ChargerType   string `json:"charger_type"`   // "AC" or "DC" (default AC)
	NumConnectors int    `json:"num_connectors"` // 1 or 2 (default 1)

	// AC-only
	Phases int `json:"phases"` // 1 or 3 (required if charger_type=AC)

	// DC-only
	BatteryCapacityKWh float64 `json:"battery_capacity_kwh"` // EV battery size; >0 required if DC
	InitialSoC         float64 `json:"initial_soc"`          // 0..100 starting SoC
}

func (s *Server) handleConfigure(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req configureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.log.Warn("configure: bad json", "err", err)
		writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// Trim user-supplied strings; defaults fill the rest.
	req.CMSURL = strings.TrimSpace(req.CMSURL)
	req.CPID = strings.TrimSpace(req.CPID)
	req.Vendor = strings.TrimSpace(req.Vendor)
	req.Model = strings.TrimSpace(req.Model)
	req.FirmwareVer = strings.TrimSpace(req.FirmwareVer)

	if req.CMSURL == "" {
		writeJSONError(w, http.StatusBadRequest, "cms_url is required")
		return
	}
	if req.CPID == "" {
		writeJSONError(w, http.StatusBadRequest, "cp_id is required")
		return
	}
	if req.MaxKW <= 0 {
		writeJSONError(w, http.StatusBadRequest, "max_kw must be > 0")
		return
	}
	if req.Voltage <= 0 {
		writeJSONError(w, http.StatusBadRequest, "voltage must be > 0")
		return
	}

	// Charger type: default AC, else must be exactly "AC" or "DC".
	chType := config.ChargerType(strings.ToUpper(strings.TrimSpace(req.ChargerType)))
	if chType == "" {
		chType = config.ChargerTypeAC
	}
	if chType != config.ChargerTypeAC && chType != config.ChargerTypeDC {
		writeJSONError(w, http.StatusBadRequest, "charger_type must be 'AC' or 'DC'")
		return
	}

	// AC-only validation.
	if chType == config.ChargerTypeAC {
		if req.Phases != 1 && req.Phases != 3 {
			writeJSONError(w, http.StatusBadRequest, "phases must be 1 or 3 for AC chargers")
			return
		}
	} else {
		// DC: phases field is irrelevant; force it to 0 in the saved config.
		req.Phases = 0
		if req.BatteryCapacityKWh <= 0 {
			writeJSONError(w, http.StatusBadRequest, "battery_capacity_kwh must be > 0 for DC chargers")
			return
		}
		if req.InitialSoC < 0 || req.InitialSoC > 100 {
			writeJSONError(w, http.StatusBadRequest, "initial_soc must be between 0 and 100")
			return
		}
	}

	// Connector count: default 1, accept 1 or 2.
	nConn := req.NumConnectors
	if nConn == 0 {
		nConn = 1
	}
	if nConn != 1 && nConn != 2 {
		writeJSONError(w, http.StatusBadRequest, "num_connectors must be 1 or 2")
		return
	}

	if req.Vendor == "" {
		req.Vendor = "TobOR Sim"
	}
	if req.Model == "" {
		if chType == config.ChargerTypeDC {
			req.Model = "Virtual DC 60kW"
		} else {
			req.Model = "Virtual AC 22kW"
		}
	}
	if req.FirmwareVer == "" {
		req.FirmwareVer = "0.1.0-sim"
	}
	if req.HeartbeatS <= 0 {
		req.HeartbeatS = 30
	}
	if req.MeterS <= 0 {
		req.MeterS = 5
	}

	cfg := config.ChargerConfig{
		CPID:               req.CPID,
		Vendor:             req.Vendor,
		Model:              req.Model,
		FirmwareVersion:    req.FirmwareVer,
		CMSURL:             strings.TrimRight(req.CMSURL, "/") + "/" + req.CPID,
		BasicAuthUser:      req.User,
		BasicAuthPass:      req.Pass,
		HeartbeatInterval:  time.Duration(req.HeartbeatS) * time.Second,
		MeterInterval:      time.Duration(req.MeterS) * time.Second,
		Type:               chType,
		NumConnectors:      nConn,
		MaxPowerW:          req.MaxKW * 1000.0,
		NominalVoltageV:    req.Voltage,
		Phases:             req.Phases,
		PowerFactor:        0.99,
		BatteryCapacityKWh: req.BatteryCapacityKWh,
		InitialSoC:         req.InitialSoC,
	}

	s.startCharger(cfg)

	s.log.Info("charger configured",
		"cp_id", cfg.CPID,
		"cms_url", cfg.CMSURL,
		"max_w", cfg.MaxPowerW,
		"phases", cfg.Phases)

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// startCharger replaces any running charger with a freshly constructed one and
// (re)starts the event fanout. WS clients are NOT disconnected.
func (s *Server) startCharger(cfg config.ChargerConfig) {
	s.mu.Lock()
	// Tear down the previous charger + its fanout.
	if s.cancelCharger != nil {
		s.cancelCharger()
		s.cancelCharger = nil
	}
	if s.fanoutCancel != nil {
		s.fanoutCancel()
		s.fanoutCancel = nil
	}
	s.current = nil
	s.currentCfg = nil

	ch := actor.NewCharger(cfg, s.log)
	ctx, cancel := context.WithCancel(context.Background())
	fanoutCtx, fanoutCancel := context.WithCancel(context.Background())

	s.current = ch
	s.cancelCharger = cancel
	s.fanoutCancel = fanoutCancel
	cfgCopy := cfg
	s.currentCfg = &cfgCopy
	s.mu.Unlock()

	// Run the actor. The actor will publish a state event on first connect.
	go func() {
		if err := ch.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.log.Warn("charger.Run exited", "err", err)
		}
	}()

	go s.fanout(fanoutCtx, ch)
}

func (s *Server) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.mu.Lock()
	if s.cancelCharger != nil {
		s.cancelCharger()
		s.cancelCharger = nil
	}
	if s.fanoutCancel != nil {
		s.fanoutCancel()
		s.fanoutCancel = nil
	}
	s.current = nil
	s.currentCfg = nil
	s.mu.Unlock()

	s.log.Info("charger disconnected via API")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	ch := s.current
	s.mu.RUnlock()
	if ch == nil {
		writeJSONError(w, http.StatusNotFound, "charger not configured")
		return
	}
	writeJSON(w, http.StatusOK, ch.State())
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	cfg := s.currentCfg
	s.mu.RUnlock()
	if cfg == nil {
		writeJSONError(w, http.StatusNotFound, "charger not configured")
		return
	}
	// Don't echo the password back to the dashboard.
	safe := *cfg
	safe.BasicAuthPass = ""
	writeJSON(w, http.StatusOK, safe)
}

// handleAction is a small factory: it returns an http.HandlerFunc that fires
// fn against the currently-running charger, or 409s if none is configured.
func (s *Server) handleAction(fn func(*actor.Charger)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.mu.RLock()
		ch := s.current
		s.mu.RUnlock()
		if ch == nil {
			writeJSONError(w, http.StatusConflict, "charger not configured")
			return
		}
		fn(ch)
		w.WriteHeader(http.StatusNoContent)
	}
}

// connectorAction body shape: {"connector_id": N}. Optional; defaults to 1
// (which is always present, since num_connectors is at least 1).
type connectorActionRequest struct {
	ConnectorID int `json:"connector_id"`
}

// handleConnectorAction is like handleAction but parses a connector_id from
// the request body. Empty / missing body → connector 1.
func (s *Server) handleConnectorAction(fn func(*actor.Charger, int)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		id, err := readConnectorID(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.mu.RLock()
		ch := s.current
		s.mu.RUnlock()
		if ch == nil {
			writeJSONError(w, http.StatusConflict, "charger not configured")
			return
		}
		fn(ch, id)
		w.WriteHeader(http.StatusNoContent)
	}
}

// readConnectorID parses an optional {"connector_id": N} body. Empty body or
// connector_id == 0 maps to connector 1.
func readConnectorID(r *http.Request) (int, error) {
	if r.ContentLength <= 0 {
		return 1, nil
	}
	var req connectorActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return 0, errors.New("invalid JSON: " + err.Error())
	}
	if req.ConnectorID == 0 {
		return 1, nil
	}
	if req.ConnectorID < 1 || req.ConnectorID > 2 {
		return 0, errors.New("connector_id must be 1 or 2")
	}
	return req.ConnectorID, nil
}

type startChargingRequest struct {
	IdTag       string `json:"id_tag"`
	ConnectorID int    `json:"connector_id"`
}

func (s *Server) handleStartCharging(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req startChargingRequest
	// Empty body is fine — we'll default the tag and connector.
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
	}
	idTag := strings.TrimSpace(req.IdTag)
	if idTag == "" {
		idTag = "TESTRFID0001"
	}
	connID := req.ConnectorID
	if connID == 0 {
		connID = 1
	}
	if connID < 1 || connID > 2 {
		writeJSONError(w, http.StatusBadRequest, "connector_id must be 1 or 2")
		return
	}

	s.mu.RLock()
	ch := s.current
	s.mu.RUnlock()
	if ch == nil {
		writeJSONError(w, http.StatusConflict, "charger not configured")
		return
	}
	ch.StartCharging(idTag, connID)
	w.WriteHeader(http.StatusNoContent)
}

type setPowerRequest struct {
	Watts float64 `json:"watts"`
}

func (s *Server) handleSetPower(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req setPowerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	s.mu.RLock()
	ch := s.current
	s.mu.RUnlock()
	if ch == nil {
		writeJSONError(w, http.StatusConflict, "charger not configured")
		return
	}
	ch.SetPower(req.Watts)
	w.WriteHeader(http.StatusNoContent)
}

// meterFaultRequest is the body of POST /api/meter-fault. Empty connector_id
// defaults to 1; mode must be one of meter.Fault's values ("" / "none" clears).
type meterFaultRequest struct {
	ConnectorID int    `json:"connector_id"`
	Mode        string `json:"mode"`
}

func (s *Server) handleMeterFault(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req meterFaultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	connID := req.ConnectorID
	if connID == 0 {
		connID = 1
	}
	if connID < 1 || connID > 2 {
		writeJSONError(w, http.StatusBadRequest, "connector_id must be 1 or 2")
		return
	}
	mode := strings.TrimSpace(req.Mode)
	if mode == "" {
		mode = string(meter.FaultNone)
	}
	if !meter.ValidFault(mode) {
		writeJSONError(w, http.StatusBadRequest, "unknown meter fault mode: "+mode)
		return
	}
	s.mu.RLock()
	ch := s.current
	s.mu.RUnlock()
	if ch == nil {
		writeJSONError(w, http.StatusConflict, "charger not configured")
		return
	}
	ch.SetMeterFault(connID, meter.Fault(mode))
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// WebSocket: /api/events
// ---------------------------------------------------------------------------

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Local dashboards can come from any origin; this is an internal tool.
		InsecureSkipVerify: true,
		OriginPatterns:     []string{"*"},
	})
	if err != nil {
		s.log.Warn("ws accept failed", "err", err)
		return
	}

	cl := &wsClient{outCh: make(chan []byte, 64)}

	s.mu.RLock()
	current := s.current
	s.mu.RUnlock()

	// Greet the client with the current snapshot so the dashboard renders
	// the right values without waiting for the next state change.
	// Build and queue the greet BEFORE registering the client, so the fanout
	// goroutine cannot race a fresh state event ahead of this (stale) snapshot.
	if current != nil {
		snap := current.State()
		greet := actor.Event{
			Time:  time.Now().UTC(),
			Kind:  actor.EventState,
			State: &snap,
		}
		if b, err := json.Marshal(greet); err == nil {
			// Non-blocking; the channel was just created so this will fit.
			select {
			case cl.outCh <- b:
			default:
			}
		}
	}

	// THEN register the client so fanout can observe it.
	s.mu.Lock()
	s.clients[cl] = struct{}{}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		if _, ok := s.clients[cl]; ok {
			delete(s.clients, cl)
		}
		s.mu.Unlock()
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	// Connection context: cancelled if the read loop or write loop exits, or
	// the request context is cancelled (e.g. server shutdown).
	connCtx, connCancel := context.WithCancel(r.Context())
	defer connCancel()

	// Read loop: discard messages, but we MUST read so coder/websocket
	// processes pings, pongs, and close frames.
	go func() {
		defer connCancel()
		for {
			if _, _, err := conn.Read(connCtx); err != nil {
				return
			}
		}
	}()

	// Write loop runs on the request goroutine.
	for {
		select {
		case <-connCtx.Done():
			return
		case msg, ok := <-cl.outCh:
			if !ok {
				// Server is shutting us down.
				return
			}
			writeCtx, cancel := context.WithTimeout(connCtx, 5*time.Second)
			err := conn.Write(writeCtx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				s.log.Debug("ws write failed", "err", err)
				return
			}
		}
	}
}

// fanout reads the charger's event channel and broadcasts every event to all
// connected dashboards. A slow client is non-blocking: if its 64-deep buffer is
// full, the event is dropped for that client only.
func (s *Server) fanout(ctx context.Context, ch *actor.Charger) {
	events := ch.Events()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			b, err := json.Marshal(ev)
			if err != nil {
				s.log.Warn("fanout: marshal event failed", "err", err, "kind", ev.Kind)
				continue
			}
			s.mu.RLock()
			for cl := range s.clients {
				select {
				case cl.outCh <- b:
				default:
					// Slow client — drop this event for them only.
				}
			}
			s.mu.RUnlock()
		}
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
