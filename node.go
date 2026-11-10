package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"github.com/gorilla/websocket"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

func dialTarget(ctx context.Context, address string, allowPrivate bool) (net.Conn, error) {
	host, port, e := net.SplitHostPort(address)
	if e != nil {
		return nil, e
	}
	p, e := strconv.Atoi(port)
	if e != nil || p < 1 || p > 65535 {
		return nil, fmt.Errorf("invalid target port")
	}
	ips, e := net.DefaultResolver.LookupIPAddr(ctx, host)
	if e != nil {
		return nil, e
	}
	var last error
	for _, v := range ips {
		ip := v.IP
		if !allowPrivate && (!ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
			continue
		}
		c, e := (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		if e == nil {
			return c, nil
		}
		last = e
	}
	if last == nil {
		last = fmt.Errorf("target address is not public")
	}
	return nil, last
}

type dialAck struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func nodeHandler(token string, allowPrivate bool) http.Handler {
	upgrade := websocket.Upgrader{ReadBufferSize: chunkSize, WriteBufferSize: chunkSize, HandshakeTimeout: 10 * time.Second}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tunnel" {
			http.NotFound(w, r)
			return
		}
		if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Tunnel-Token")), []byte(token)) != 1 {
			http.Error(w, "Forbidden", 403)
			return
		}
		if !websocket.IsWebSocketUpgrade(r) {
			http.Error(w, "WebSocket required", 400)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		var target net.Conn
		var ws *websocket.Conn
		var e error
		if r.Header.Get("X-Tunnel-Protocol") == "2" {
			// Report target errors inside an established WebSocket. SCF rewrites a
			// pre-upgrade HTTP error as a platform handshake failure (500/430).
			ws, e = upgrade.Upgrade(w, r, nil)
			if e != nil {
				return
			}
			target, e = dialTarget(ctx, r.Header.Get("X-Tunnel-Target"), allowPrivate)
			ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if e != nil {
				ws.WriteJSON(dialAck{Error: "target", Detail: e.Error()})
				ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "target unavailable"), time.Now().Add(time.Second))
				ws.Close()
				return
			}
			if e = ws.WriteJSON(dialAck{OK: true}); e != nil {
				target.Close()
				ws.Close()
				return
			}
		} else {
			// Keep legacy agents working while regional functions are updated.
			target, e = dialTarget(ctx, r.Header.Get("X-Tunnel-Target"), allowPrivate)
			if e != nil {
				w.Header().Set("X-Tunnel-Error", "target")
				http.Error(w, "Target connection failed", 502)
				return
			}
			ws, e = upgrade.Upgrade(w, r, nil)
			if e != nil {
				target.Close()
				return
			}
		}

		relay(target, newStream(ws))
	})
}
func runNode() error {
	token := os.Getenv("CLOUD_PROXY_TOKEN")
	if token == "" {
		return fmt.Errorf("missing node authentication token")
	}
	return (&http.Server{Addr: ":9000", Handler: nodeHandler(token, false), ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 16 * 1024}).ListenAndServe()
}
