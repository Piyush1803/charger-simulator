package actor_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/tobor/charger-simulator/internal/actor"
	"github.com/tobor/charger-simulator/internal/config"
	"github.com/tobor/charger-simulator/internal/ocpp/j16"
	"github.com/tobor/charger-simulator/internal/session"
)

// These integration tests stand up a real OCPP-J WebSocket central system and
// drive the real Charger actor + real on-disk session store through every
// interruption mode, asserting a StopTransaction(PowerLoss) is emitted for the
// session that was open when the link dropped. They exercise the exact code
// paths the worker uses in production.

// ---- test CMS ----

type testCMS struct {
	srv *httptest.Server

	mu     sync.Mutex
	stops  []j16.StopTransactionReq
	boots  int
	nextTx int
	conn   *websocket.Conn
}

func newTestCMS(t *testing.T) *testCMS {
	t.Helper()
	c := &testCMS{nextTx: 1000}
	c.srv = httptest.NewServer(http.HandlerFunc(c.handleWS))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *testCMS) wsURL(cpid string) string {
	return strings.Replace(c.srv.URL, "http", "ws", 1) + "/ocpp/" + cpid
}

func (c *testCMS) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"ocpp1.6"}})
	if err != nil {
		return
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	ctx := r.Context()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		f, err := j16.Decode(data)
		if err != nil || f.Type != j16.CALL {
			continue
		}
		payload := c.reply(f.Call.Action, f.Call.Payload)
		raw, _ := j16.CallResult{MessageID: f.Call.MessageID, Payload: payload}.Marshal()
		if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
			return
		}
	}
}

func (c *testCMS) reply(action string, payload json.RawMessage) json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch action {
	case "BootNotification":
		c.boots++
		return mustJSON(j16.BootNotificationResp{CurrentTime: time.Now().UTC(), Interval: 30, Status: j16.RegAccepted})
	case "Heartbeat":
		return mustJSON(j16.HeartbeatResp{CurrentTime: time.Now().UTC()})
	case "Authorize":
		return mustJSON(j16.AuthorizeResp{IdTagInfo: j16.IdTagInfo{Status: j16.AuthAccepted}})
	case "StartTransaction":
		c.nextTx++
		return mustJSON(j16.StartTransactionResp{
			IdTagInfo:     j16.IdTagInfo{Status: j16.AuthAccepted},
			TransactionId: c.nextTx,
		})
	case "StopTransaction":
		var req j16.StopTransactionReq
		_ = json.Unmarshal(payload, &req)
		c.stops = append(c.stops, req)
		info := j16.IdTagInfo{Status: j16.AuthAccepted}
		return mustJSON(j16.StopTransactionResp{IdTagInfo: &info})
	default: // StatusNotification, MeterValues, ...
		return json.RawMessage(`{}`)
	}
}

// forceDrop abruptly kills the current connection (simulates a CMS crash /
// network loss). The charger must auto-reconnect to the same URL.
func (c *testCMS) forceDrop() {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		_ = conn.CloseNow()
	}
}

func (c *testCMS) powerLossStops() []j16.StopTransactionReq {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []j16.StopTransactionReq
	for _, s := range c.stops {
		if s.Reason == j16.StopReasonPowerLoss {
			out = append(out, s)
		}
	}
	return out
}

func (c *testCMS) bootCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.boots
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// ---- helpers ----

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig(wsURL, cpid string, dc bool) config.ChargerConfig {
	cfg := config.ChargerConfig{
		CPID:                    cpid,
		Vendor:                  "Test",
		Model:                   "Test",
		FirmwareVersion:         "test",
		CMSURL:                  wsURL,
		HeartbeatInterval:       30 * time.Second,
		MeterInterval:           50 * time.Millisecond,
		NumConnectors:           1,
		ReconnectInitialBackoff: 20 * time.Millisecond,
		ReconnectMaxBackoff:     50 * time.Millisecond,
	}
	if dc {
		cfg.Type = config.ChargerTypeDC
		cfg.MaxPowerW = 60000
		cfg.NominalVoltageV = 400
		cfg.BatteryCapacityKWh = 50
		cfg.InitialSoC = 20
	} else {
		cfg.Type = config.ChargerTypeAC
		cfg.MaxPowerW = 7000
		cfg.NominalVoltageV = 230
		cfg.Phases = 1
		cfg.PowerFactor = 0.99
	}
	return cfg
}

func openStore(t *testing.T, path string) session.Store {
	t.Helper()
	s, err := session.NewFileStore(path, testLogger())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	return s
}

func waitFor(t *testing.T, d time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", desc)
}

func connectorOf(ch *actor.Charger, id int) actor.ConnectorSnapshot {
	for _, cn := range ch.State().Connectors {
		if cn.ID == id {
			return cn
		}
	}
	return actor.ConnectorSnapshot{}
}

// startChargingSession drives PlugIn → StartCharging and returns the CMS-assigned
// transactionId once the connector reports Charging.
func startChargingSession(t *testing.T, ch *actor.Charger) int {
	t.Helper()
	waitFor(t, 5*time.Second, "charger online", func() bool { return ch.State().CPState == "Online" })
	ch.PlugIn(1)
	waitFor(t, 5*time.Second, "connector Preparing", func() bool { return connectorOf(ch, 1).Status == "Preparing" })
	ch.StartCharging("TESTRFID0001", 1)
	var txID int
	waitFor(t, 5*time.Second, "connector Charging", func() bool {
		cn := connectorOf(ch, 1)
		txID = cn.TransactionID
		return cn.Status == "Charging" && cn.TransactionID != 0
	})
	return txID
}

func runCharger(t *testing.T, cfg config.ChargerConfig, store session.Store) (*actor.Charger, context.CancelFunc) {
	t.Helper()
	ch := actor.NewCharger(cfg, testLogger(), store)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = ch.Run(ctx) }()
	return ch, cancel
}

// ---- scenario 1: operator offline/online button ----

func TestRecovery_OfflineOnlineButton(t *testing.T) {
	for _, dc := range []bool{false, true} {
		name := "AC"
		if dc {
			name = "DC"
		}
		t.Run(name, func(t *testing.T) {
			cpid := "CP-BTN-" + name
			cms := newTestCMS(t)
			store := openStore(t, filepath.Join(t.TempDir(), "sessions.json"))
			ch, cancel := runCharger(t, testConfig(cms.wsURL(cpid), cpid, dc), store)
			defer cancel()

			txID := startChargingSession(t, ch)

			ch.GoOffline()
			waitFor(t, 5*time.Second, "offline", func() bool { return ch.State().CPState == "Offline" })
			ch.GoOnline()

			waitFor(t, 5*time.Second, "StopTransaction(PowerLoss)", func() bool { return len(cms.powerLossStops()) >= 1 })

			stops := cms.powerLossStops()
			if stops[0].TransactionId != txID {
				t.Fatalf("PowerLoss stop txID = %d, want %d", stops[0].TransactionId, txID)
			}
			waitFor(t, 5*time.Second, "connector Available", func() bool { return connectorOf(ch, 1).Status == "Available" })
			waitFor(t, 5*time.Second, "record cleared", func() bool {
				open, _ := store.LoadOpen(cpid)
				return len(open) == 0
			})
		})
	}
}

// ---- scenario 2: unexpected CMS drop → auto-reconnect ----

func TestRecovery_UnexpectedDropAutoReconnect(t *testing.T) {
	for _, dc := range []bool{false, true} {
		name := "AC"
		if dc {
			name = "DC"
		}
		t.Run(name, func(t *testing.T) {
			cpid := "CP-DROP-" + name
			cms := newTestCMS(t)
			store := openStore(t, filepath.Join(t.TempDir(), "sessions.json"))
			ch, cancel := runCharger(t, testConfig(cms.wsURL(cpid), cpid, dc), store)
			defer cancel()

			txID := startChargingSession(t, ch)
			bootsBefore := cms.bootCount()

			cms.forceDrop() // simulate CMS crash / network loss

			waitFor(t, 8*time.Second, "auto-reconnect + StopTransaction(PowerLoss)", func() bool {
				return len(cms.powerLossStops()) >= 1
			})
			if cms.bootCount() <= bootsBefore {
				t.Fatalf("expected a re-boot after auto-reconnect (boots %d → %d)", bootsBefore, cms.bootCount())
			}
			if got := cms.powerLossStops()[0].TransactionId; got != txID {
				t.Fatalf("PowerLoss stop txID = %d, want %d", got, txID)
			}
			waitFor(t, 5*time.Second, "connector Available", func() bool { return connectorOf(ch, 1).Status == "Available" })
		})
	}
}

// ---- scenario 3: full process restart ----

func TestRecovery_ProcessRestart(t *testing.T) {
	for _, dc := range []bool{false, true} {
		name := "AC"
		if dc {
			name = "DC"
		}
		t.Run(name, func(t *testing.T) {
			cpid := "CP-RESTART-" + name
			cms := newTestCMS(t)
			storePath := filepath.Join(t.TempDir(), "sessions.json")
			cfg := testConfig(cms.wsURL(cpid), cpid, dc)

			// First "process": start a session, then hard-exit (cancel ctx) WITHOUT
			// stopping the transaction — the in-memory txID is lost.
			store1 := openStore(t, storePath)
			ch1, cancel1 := runCharger(t, cfg, store1)
			txID := startChargingSession(t, ch1)
			cancel1()
			waitFor(t, 5*time.Second, "first charger down", func() bool { return ch1.State().CPState == "Disconnected" })

			// The open session must have survived on disk (mode-3 precondition).
			if open, _ := store1.LoadOpen(cpid); len(open) != 1 {
				t.Fatalf("expected 1 persisted open session after crash, got %d", len(open))
			}

			// Second "process": fresh store over the SAME file, fresh charger. It
			// must replay a StopTransaction(PowerLoss) for the lost session on boot.
			store2 := openStore(t, storePath)
			_, cancel2 := runCharger(t, cfg, store2)
			defer cancel2()

			waitFor(t, 5*time.Second, "StopTransaction(PowerLoss) after restart", func() bool {
				return len(cms.powerLossStops()) >= 1
			})
			if got := cms.powerLossStops()[0].TransactionId; got != txID {
				t.Fatalf("recovered stop txID = %d, want %d", got, txID)
			}
			waitFor(t, 5*time.Second, "record cleared after restart", func() bool {
				open, _ := store2.LoadOpen(cpid)
				return len(open) == 0
			})
		})
	}
}
