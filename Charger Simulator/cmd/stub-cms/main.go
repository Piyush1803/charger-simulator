// Command stub-cms is a minimal OCPP 1.6J central system used for local dev
// and tests. It accepts a WebSocket upgrade at /ocpp/<cp-id>, validates the
// "ocpp1.6" subprotocol, and replies with canned success responses to every
// known action. It does NOT initiate any CMS-to-charger calls in slice 1 —
// that comes when we add the remote-start/remote-stop test scenarios.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/tobor/charger-simulator/internal/ocpp/j16"
)

type server struct {
	log    *slog.Logger
	nextTx atomic.Int64
}

func main() {
	addr := flag.String("addr", ":9000", "Listen address")
	logLevel := flag.String("log", "info", "Log level: debug|info|warn|error")
	flag.Parse()

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})).
		With("service", "stub-cms")

	s := &server{log: log}
	s.nextTx.Store(1000)

	mux := http.NewServeMux()
	mux.HandleFunc("/ocpp/", s.handleWS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		log.Info("listening", "addr", *addr, "ws_path", "/ocpp/<cp-id>")
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutCancel()
	_ = httpSrv.Shutdown(shutCtx)
}

func (s *server) handleWS(w http.ResponseWriter, r *http.Request) {
	cpID := strings.TrimPrefix(r.URL.Path, "/ocpp/")
	if cpID == "" || strings.Contains(cpID, "/") {
		http.Error(w, "bad cp-id", http.StatusBadRequest)
		return
	}
	log := s.log.With("cp_id", cpID, "remote", r.RemoteAddr)

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{"ocpp1.6"},
	})
	if err != nil {
		log.Warn("ws accept", "err", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(1 << 20)

	if conn.Subprotocol() != "ocpp1.6" {
		log.Warn("client did not select ocpp1.6 subprotocol", "got", conn.Subprotocol())
		_ = conn.Close(websocket.StatusProtocolError, "subprotocol")
		return
	}

	log.Info("charger connected")

	ctx := r.Context()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			log.Info("connection closed", "err", err)
			return
		}
		f, err := j16.Decode(data)
		if err != nil {
			log.Warn("bad frame", "err", err, "raw", string(data))
			continue
		}
		if f.Type != j16.CALL {
			log.Debug("non-CALL inbound; ignored in slice 1", "type", int(f.Type))
			continue
		}

		log.Info("← in",
			"action", f.Call.Action,
			"msg_id", f.Call.MessageID,
			"payload", string(f.Call.Payload))

		respPayload, action, err := s.handleCall(f.Call.Action, f.Call.Payload)
		if err != nil {
			frame := j16.CallError{
				MessageID:        f.Call.MessageID,
				ErrorCode:        "InternalError",
				ErrorDescription: err.Error(),
				ErrorDetails:     json.RawMessage(`{}`),
			}
			raw, _ := frame.Marshal()
			_ = conn.Write(ctx, websocket.MessageText, raw)
			continue
		}

		result := j16.CallResult{MessageID: f.Call.MessageID, Payload: respPayload}
		raw, _ := result.Marshal()
		if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
			log.Warn("write", "err", err)
			return
		}
		log.Info("→ out", "action", action, "msg_id", f.Call.MessageID, "payload", string(respPayload))
	}
}

func (s *server) handleCall(action string, _ json.RawMessage) (json.RawMessage, string, error) {
	switch action {
	case "BootNotification":
		return mustJSON(j16.BootNotificationResp{
			CurrentTime: time.Now().UTC(),
			Interval:    30,
			Status:      j16.RegAccepted,
		}), action, nil

	case "Heartbeat":
		return mustJSON(j16.HeartbeatResp{CurrentTime: time.Now().UTC()}), action, nil

	case "StatusNotification":
		return json.RawMessage(`{}`), action, nil

	case "Authorize":
		return mustJSON(j16.AuthorizeResp{
			IdTagInfo: j16.IdTagInfo{Status: j16.AuthAccepted},
		}), action, nil

	case "StartTransaction":
		txID := int(s.nextTx.Add(1))
		return mustJSON(j16.StartTransactionResp{
			IdTagInfo:     j16.IdTagInfo{Status: j16.AuthAccepted},
			TransactionId: txID,
		}), action, nil

	case "StopTransaction":
		info := j16.IdTagInfo{Status: j16.AuthAccepted}
		return mustJSON(j16.StopTransactionResp{IdTagInfo: &info}), action, nil

	case "MeterValues":
		return json.RawMessage(`{}`), action, nil

	case "DataTransfer":
		return json.RawMessage(`{"status":"Accepted"}`), action, nil

	default:
		return nil, action, fmt.Errorf("unknown action: %s", action)
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
