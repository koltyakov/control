package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConcurrencyLegacyVisibility(t *testing.T) {
	var legacy NodeConcurrency
	if err := json.Unmarshal([]byte(`{"execution":{"limit":4,"inUse":2},"operations":{"limit":8,"inUse":0}}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Execution.Queued != nil || legacy.Operations.Queued != nil || legacy.Desktop != nil {
		t.Fatal("legacy unknown capacity reported as known", legacy)
	}
	encoded := string(JSON(legacy))
	if strings.Contains(encoded, "queued") || strings.Contains(encoded, "desktop") {
		t.Fatal("round trip invented legacy queue/desktop readings", encoded)
	}
	var snapshot NodeActivitySnapshot
	if err := json.Unmarshal([]byte(`{"status":"ready","activeCount":0}`), &snapshot); err != nil || snapshot.Concurrency != nil {
		t.Fatal("legacy activity report invented pool visibility", snapshot, err)
	}
}
