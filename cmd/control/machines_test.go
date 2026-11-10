package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMachineListingOptions(t *testing.T) {
	t.Setenv("CONTROL_HOME", t.TempDir())
	for _, name := range []string{"CONTROL_CONFIG", "CONTROL_USER_KEY", "CONTROL_SUPERUSER_KEY", "CONTROL_TOKEN"} {
		t.Setenv(name, "")
	}
	const full = `[{"id":"one","name":"Worker","os":"linux","online":true,"software":{"version":"test","arch":"amd64"},"capabilities":[{"name":"exec.run","inputSchema":{"type":"object"}}],"futureField":"kept"},{"id":"two","name":"offline","online":false,"disabled":true}]`
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.RequestURI() != "/v1/nodes" || r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer listing-key" {
			t.Errorf("unexpected listing request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(full))
	}))
	defer server.Close()
	t.Setenv("CONTROL_GATEWAY", server.URL)
	for _, tc := range []struct {
		name string
		args []string
		ids  []string
		full bool
	}{
		{"legacy default", nil, []string{"one", "two"}, true},
		{"explicit full", []string{"--compact=false"}, []string{"one", "two"}, true},
		{"compact fleet", []string{"--compact"}, []string{"one", "two"}, false},
		{"exact name", []string{"--node", "Worker"}, []string{"one"}, true},
		{"compact stable ID", []string{"--node", "one", "--compact"}, []string{"one"}, false},
		{"offline retained", []string{"--compact", "--node", "offline"}, []string{"two"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := calls.Load()
			args := append([]string{"--token", "listing-key", "machines"}, tc.args...)
			output, err := captureUpdatePushOutput(t, func() error { return run(t.Context(), args) })
			if err != nil || calls.Load() != before+1 {
				t.Fatal("listing failed or made redundant requests", output, calls.Load(), err)
			}
			var records []map[string]any
			if err := json.Unmarshal([]byte(output), &records); err != nil {
				t.Fatal("listing is not a JSON array", output, err)
			}
			ids := make([]string, 0, len(records))
			for _, record := range records {
				ids = append(ids, record["id"].(string))
				if record["id"] == "one" {
					_, hasSchema := record["capabilities"]
					_, hasUnknown := record["futureField"]
					if hasSchema != tc.full || hasUnknown != tc.full {
						t.Fatal("full/compact fields changed", record)
					}
				}
			}
			if !reflect.DeepEqual(ids, tc.ids) {
				t.Fatal("filter changed selection/order", ids, tc.ids)
			}
			if tc.name == "legacy default" {
				var actual, expected any
				if json.Unmarshal([]byte(output), &actual) != nil || json.Unmarshal([]byte(full), &expected) != nil || !reflect.DeepEqual(actual, expected) {
					t.Fatal("legacy JSON consumers lost full records", output)
				}
			}
		})
	}
	for _, args := range [][]string{{"--node"}, {"--node", ""}, {"--compact=invalid"}, {"--unknown"}, {"--compact", "trailing"}} {
		before := calls.Load()
		if err := run(t.Context(), append([]string{"machines"}, args...)); err == nil || calls.Load() != before {
			t.Fatal("invalid options did not fail before discovery", args, err, calls.Load())
		}
	}
	for _, name := range []string{"worker", "foreign-id"} {
		if err := run(t.Context(), []string{"--token", "listing-key", "machines", "--node", name}); err == nil || !strings.Contains(err.Error(), "unknown machine") {
			t.Fatal("unknown target silently fell back to fleet listing", err)
		}
	}
	before := calls.Load()
	if err := run(t.Context(), []string{"machines", "--help"}); err != nil || calls.Load() != before {
		t.Fatal("listing help contacted gateway", err)
	}
}
