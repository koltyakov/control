package node

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/model"
)

type APICall struct {
	Target string          `json:"target"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Handler is the authenticated local API used by CLI and MCP clients. Bind it
// to loopback; it carries the local node's authority, not a remote caller's.
func (n *Node) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/service/stop", func(w http.ResponseWriter, r *http.Request) {
		if n.shutdown == nil {
			http.Error(w, "service control unavailable", http.StatusNotImplemented)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		n.shutdown()
	})
	mux.HandleFunc("POST /v1/call", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxFrame)
		var call APICall
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(call.Params) == 0 {
			call.Params = json.RawMessage(`{}`)
		}
		var result json.RawMessage
		err := n.Call(r.Context(), call.Target, call.Method, call.Params, &result)
		response := model.Response{Result: result}
		if err != nil {
			response.Error = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	})
	mux.HandleFunc("POST /v1/download", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
		var q struct {
			Artifact model.Artifact `json:"artifact"`
			Offset   int64          `json:"offset"`
		}
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if q.Offset < 0 || q.Offset > q.Artifact.Size {
			http.Error(w, "invalid offset", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		// Do not send a successful response until the peer supplies bytes. A
		// short response remains detectable through the artifact size/hash.
		if err := n.Download(r.Context(), q.Artifact, q.Offset, w); err != nil {
			// Abort so a partially sent body cannot be mistaken for a full file.
			panic(http.ErrAbortHandler)
		}
	})
	mux.HandleFunc("GET /v1/tunnel", func(w http.ResponseWriter, r *http.Request) {
		peer, err := n.OpenTCP(r.Context(), r.URL.Query().Get("target"), r.URL.Query().Get("address"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer peer.Close()
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		stream := websocket.NetConn(r.Context(), ws, websocket.MessageBinary)
		Bridge(r.Context(), peer, stream)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(n.Config.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func HTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 * 1024}
}
