package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// wsClient implements Client. Messages go through a bounded queue; a client
// that cannot keep up is disconnected instead of stalling the server.
type wsClient struct {
	out    chan wsMsg
	cancel context.CancelFunc
}

type wsMsg struct {
	typ  websocket.MessageType
	data []byte
}

func (c *wsClient) push(m wsMsg) {
	select {
	case c.out <- m:
	default:
		c.cancel()
	}
}

func (c *wsClient) SendJSON(v any) {
	b, _ := json.Marshal(v)
	c.push(wsMsg{websocket.MessageText, b})
}

func (c *wsClient) SendText(b []byte) {
	c.push(wsMsg{websocket.MessageText, append([]byte(nil), b...)})
}

func (c *wsClient) SendBinary(b []byte) { c.push(wsMsg{websocket.MessageBinary, b}) }

type clientMsg struct {
	Op   string `json:"op"`
	Code string `json:"code"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
	ID   string `json:"id"` // version id for op "version"
}

func (srv *server) handleWS(w http.ResponseWriter, r *http.Request) {
	// Default AcceptOptions enforce Origin == Host (no cross-site WebSockets).
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(256 << 10)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	cl := &wsClient{out: make(chan wsMsg, 256), cancel: cancel}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-cl.out:
				wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Write(wctx, m.typ, m.data)
				wcancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	s := srv.m.Connect(cl, srv.clientIP(r))
	defer srv.m.Disconnect(s)

	var lastRun time.Time
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary {
			srv.m.TermInput(s, data)
			continue
		}
		var msg clientMsg
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.Op {
		case "run":
			if time.Since(lastRun) < time.Second {
				continue
			}
			lastRun = time.Now()
			srv.m.Run(s, msg.Code)
		case "kill":
			srv.m.Kill(s)
		case "acquire":
			srv.m.Acquire(s)
		case "release":
			srv.m.Release(s)
		case "term":
			srv.m.OpenTerm(s, msg.Cols, msg.Rows)
		case "resize":
			srv.m.Resize(s, msg.Cols, msg.Rows)
		case "version":
			srv.m.SetImage(s, msg.ID)
		case "active":
			srv.m.Touch(s)
		case "ping":
			// client heartbeat: keeps Cloudflare from closing an idle WebSocket
		}
	}
}

// clientIP: behind cloudflared every request comes from localhost, so the
// real address is in CF-Connecting-IP. Only trusted when -trust-cf is set
// (pgd must then listen on localhost only).
func (srv *server) clientIP(r *http.Request) string {
	if srv.trustCF {
		if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
