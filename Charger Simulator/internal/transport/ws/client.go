// Package ws is a thin OCPP-J aware WebSocket client for one virtual charger.
//
// Responsibilities:
//   - Dial with the "ocpp1.6" subprotocol.
//   - Optional HTTP Basic auth (OCPP security profile 1).
//   - Pump inbound text frames onto a single channel for the actor to consume.
//   - Provide a Send method that serializes one frame at a time.
//
// The client is NOT goroutine-safe for concurrent Send calls — the charger actor
// is the single owner of the write side. That is intentional: WebSocket requires
// writes to be serialized, and modelling that explicitly avoids hidden mutexes.
package ws

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/coder/websocket"
)

// Client wraps a single WebSocket connection to a CMS endpoint.
type Client struct {
	URL         string
	Subprotocol string
	BasicUser   string
	BasicPass   string
	Logger      *slog.Logger

	conn        *websocket.Conn
	inbound     chan []byte
	readCancel  context.CancelFunc
	closeOnce   sync.Once
	closed      chan struct{}
}

// New returns an unconnected client. Call Connect before Send/Inbound.
func New(url string, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		URL:         url,
		Subprotocol: "ocpp1.6",
		Logger:      log,
		inbound:     make(chan []byte, 64),
		closed:      make(chan struct{}),
	}
}

// Connect dials the CMS. The provided ctx applies only to the dial; the read
// loop runs until Close is called.
func (c *Client) Connect(ctx context.Context) error {
	header := http.Header{}
	if c.BasicUser != "" || c.BasicPass != "" {
		creds := c.BasicUser + ":" + c.BasicPass
		header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(creds)))
	}
	opts := &websocket.DialOptions{
		Subprotocols: []string{c.Subprotocol},
		HTTPHeader:   header,
	}
	conn, _, err := websocket.Dial(ctx, c.URL, opts)
	if err != nil {
		return err
	}
	negotiated := conn.Subprotocol()
	if negotiated != c.Subprotocol {
		_ = conn.Close(websocket.StatusProtocolError, "subprotocol mismatch")
		return errors.New("server did not negotiate ocpp1.6 subprotocol; got " + negotiated)
	}
	conn.SetReadLimit(1 << 20) // 1 MiB

	c.conn = conn
	readCtx, cancel := context.WithCancel(context.Background())
	c.readCancel = cancel
	go c.readLoop(readCtx)
	c.Logger.Info("ws connected", "url", c.URL, "subprotocol", negotiated)
	return nil
}

func (c *Client) readLoop(ctx context.Context) {
	defer close(c.inbound)
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				c.Logger.Info("ws read closed", "err", err)
			}
			return
		}
		select {
		case c.inbound <- data:
		case <-c.closed:
			return
		}
	}
}

// Inbound returns the channel of raw inbound frames. Closed when the read loop exits.
func (c *Client) Inbound() <-chan []byte { return c.inbound }

// Send writes one text frame. Must be called from a single goroutine.
func (c *Client) Send(ctx context.Context, data []byte) error {
	if c.conn == nil {
		return errors.New("ws not connected")
	}
	return c.conn.Write(ctx, websocket.MessageText, data)
}

// Close shuts the connection. Idempotent.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		if c.readCancel != nil {
			c.readCancel()
		}
		if c.conn != nil {
			_ = c.conn.Close(websocket.StatusNormalClosure, "shutdown")
		}
	})
}
