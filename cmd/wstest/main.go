// Command wstest connects to /api/events on a running worker, reads up to N
// frames (or until ctx timeout), prints them compactly, and exits. Used for
// the slice-2 smoke test of the WebSocket broadcaster.
//
// This binary is a developer tool — not shipped.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

func main() {
	url := flag.String("url", "ws://localhost:8080/api/events", "Worker /api/events URL")
	max := flag.Int("n", 20, "Max events to print before exiting")
	dur := flag.Duration("for", 15*time.Second, "Max time to read for")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *dur)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, *url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(1 << 20)

	fmt.Printf("connected; reading up to %d events for up to %s\n", *max, *dur)

	for i := 0; i < *max; i++ {
		_, data, err := conn.Read(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read: %v\n", err)
			return
		}
		var ev map[string]any
		if err := json.Unmarshal(data, &ev); err != nil {
			fmt.Printf("[%2d] BAD JSON: %s\n", i, string(data))
			continue
		}
		fmt.Printf("[%2d] %s\n", i, summarise(ev))
	}
}

func summarise(ev map[string]any) string {
	kind, _ := ev["kind"].(string)
	switch kind {
	case "state":
		s, _ := ev["state"].(map[string]any)
		return fmt.Sprintf("state  cp=%v conn=%v tx=%v energy=%v power=%v cur=%v",
			s["cp_state"], s["connector_status"], s["tx_id"], s["energy_wh"], s["power_w"], s["max_power_w_current"])
	case "ocpp":
		dir, _ := ev["dir"].(string)
		t, _ := ev["ocpp_type"].(string)
		act, _ := ev["action"].(string)
		pl, _ := ev["payload"].(string)
		if len(pl) > 80 {
			pl = pl[:80] + "…"
		}
		lat := ""
		if v, ok := ev["latency_ms"]; ok && v != nil {
			lat = fmt.Sprintf(" rt=%vms", v)
		}
		ec, _ := ev["error_code"].(string)
		if ec != "" {
			return fmt.Sprintf("ocpp   %s %s %s ERR %s: %v", strings.ToUpper(dir), t, act, ec, ev["error_desc"])
		}
		return fmt.Sprintf("ocpp   %s %s %s %s%s", strings.ToUpper(dir), t, act, pl, lat)
	case "log":
		lvl, _ := ev["level"].(string)
		msg, _ := ev["msg"].(string)
		fields, _ := ev["fields"].(map[string]any)
		if fields != nil {
			b, _ := json.Marshal(fields)
			return fmt.Sprintf("log    %-5s %s %s", strings.ToUpper(lvl), msg, string(b))
		}
		return fmt.Sprintf("log    %-5s %s", strings.ToUpper(lvl), msg)
	default:
		return fmt.Sprintf("? kind=%v", kind)
	}
}
