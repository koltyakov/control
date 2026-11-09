package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/koltyakov/control/internal/browser"
	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/model"
)

func TestBrowserCLI(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var call model.APICall
		var request browser.Request
		if json.NewDecoder(r.Body).Decode(&call) != nil || json.Unmarshal(call.Params, &request) != nil ||
			call.Target != "worker" || call.Method != "browser.run" || request.Session != "work" ||
			!reflect.DeepEqual(request.Args, []string{"fill", "@e1", "literal ; $(no-shell)"}) {
			t.Error("browser CLI changed the worker or structured arguments")
		}
		_, _ = w.Write([]byte(`{"result":{"success":true}}`))
	}))
	defer server.Close()
	c := client.Client{URL: server.URL, Token: "test"}
	if err := browserCLI(t.Context(), c, []string{"run", "worker", "--session", "work", "--", "fill", "@e1", "literal ; $(no-shell)"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{}, {"run", "worker"}, {"run", "worker", "--session", "../other", "--", "snapshot"},
		{"view", "worker", "--listen", "0.0.0.0:1234"}, {"view", "worker", "--port", "-1"}, {"unknown", "worker"},
	} {
		if err := browserCLI(t.Context(), c, args); err == nil {
			t.Fatal("invalid browser command accepted", args)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("invalid browser arguments contacted a worker")
	}
}
