package client

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/transport"
)

func (c Client) reverseListener(ctx context.Context, target, listen string) (net.Listener, error) {
	peer, err := c.backend(ctx)
	if err != nil {
		return nil, err
	}
	var conn net.Conn
	var address string
	if peer != nil {
		conn, address, err = peer.OpenTCPListener(ctx, target, listen)
		if err != nil {
			return nil, err
		}
	} else {
		u := strings.TrimRight(c.URL, "/") + "/v1/listener?" + url.Values{"target": {target}, "listen": {listen}}.Encode()
		u = strings.Replace(strings.Replace(u, "https://", "wss://", 1), "http://", "ws://", 1)
		ws, response, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + c.Token}}})
		if err != nil {
			return nil, err
		}
		address = response.Header.Get("Control-Tunnel-Listen")
		if response.Header.Get("Control-Tunnel-Protocol") != transport.TCPListenerProtocol || address == "" {
			_ = ws.CloseNow()
			return nil, errors.New("unsupported TCP listener protocol")
		}
		conn = websocket.NetConn(ctx, ws, websocket.MessageBinary)
	}
	return transport.NewTCPListener(ctx, conn, address)
}
