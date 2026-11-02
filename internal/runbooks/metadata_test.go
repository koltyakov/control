package runbooks

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRunbookApplicabilityMetadata(t *testing.T) {
	header := "---\ntitle: GPU video encoding\ndescription: Encode video on suitable Windows machines.\nrequires:\n  os: [windows]\n  apps: [ffmpeg]\n  gpu: true\n  capabilities: [exec.run]\n---\n"
	want := map[string]any{"os": []any{"windows"}, "apps": []any{"ffmpeg"}, "gpu": true, "capabilities": []any{"exec.run"}}
	for _, fixture := range []struct{ file, text string }{
		{"shared/encode-video/RUNBOOK.md", header + "# Fallback title\nFallback description.\n"},
		{"shared/encode-video.md", strings.ReplaceAll(header, "\n", "\r\n") + "# Instructions\r\n"},
		{"shared/encode-video.json", `{"title":"GPU video encoding","description":"Encode video on suitable Windows machines.","requires":{"os":["windows"],"apps":["ffmpeg"],"gpu":true,"capabilities":["exec.run"]},"instructions":["Inspect prerequisites before executing."]}`},
	} {
		t.Run(fixture.file, func(t *testing.T) {
			root := t.TempDir()
			writeBook(t, root, fixture.file, fixture.text)
			catalog, err := Discover(context.Background(), []Root{{Source: "project", Path: root}}, Selector{}, Query{})
			if err != nil || len(catalog.Runbooks) != 1 {
				t.Fatalf("discovery: %+v, %v", catalog, err)
			}
			book := catalog.Runbooks[0]
			if book.Machine != "" || book.Title != "GPU video encoding" || book.Description != "Encode video on suitable Windows machines." || !reflect.DeepEqual(book.Requires, want) || book.Content != fixture.text {
				t.Fatalf("metadata not preserved independently of folders: %+v", book)
			}
			if !ValidBook(book, Selector{Node: "any-machine"}) {
				t.Fatal("shared metadata required a machine-name directory")
			}
			// Requirements remain advisory. A selected non-Windows machine does
			// not cause discovery to silently hide the runbook or claim readiness.
			filtered, err := Discover(context.Background(), []Root{{Source: "project", Path: root}}, Selector{Node: "linux-worker"}, Query{})
			if err != nil || len(filtered.Runbooks) != 1 || !reflect.DeepEqual(filtered.Runbooks[0].Requires, want) {
				t.Fatalf("requirements were hidden or changed: %+v, %v", filtered, err)
			}
		})
	}
}

func TestRunbookMetadataRemainsVisibleWhenContentOmitted(t *testing.T) {
	root := t.TempDir()
	text := "---\nrequires:\n  os: [windows]\n  apps: [ffmpeg]\n  gpu: true\n---\n# Encode video\nRead the whole procedure before acting.\n" + strings.Repeat("x", 33<<10)
	file := "shared/encode-video/RUNBOOK.md"
	writeBook(t, root, file, text)
	roots := []Root{{Source: "project", Path: root}}
	catalog, err := Discover(context.Background(), roots, Selector{}, Query{})
	if err != nil || len(catalog.Runbooks) != 1 {
		t.Fatalf("discovery failed: %v", err)
	}
	book := catalog.Runbooks[0]
	if !book.Omitted || book.Content != "" || book.Requires == nil || requirementsObject(t, book.Requires)["gpu"] != true || book.Title != "Encode video" || book.Description != "Read the whole procedure before acting." {
		t.Fatalf("omission discarded applicability metadata: %+v", book)
	}
	read, err := Discover(context.Background(), roots, Selector{}, Query{Source: "project", File: file})
	if err != nil || read.Runbooks[0].Omitted || read.Runbooks[0].Content != text || !reflect.DeepEqual(read.Runbooks[0].Requires, book.Requires) {
		t.Fatalf("complete read changed metadata: %+v, %v", read, err)
	}
}

func TestRunbookMetadataValidation(t *testing.T) {
	for _, text := range []string{
		"---\nrequires:\n  os: [windows]\n",
		"---\nrequires:\n  numeric: .nan\n---\n",
		"---\nrequires:\n  123: invalid key\n---\n",
		"---\nunknown: true\n---\n",
		"---\ntitle: one\ntitle: two\n---\n",
		"---\ntitle: &ref Example\ndescription: *ref\n---\n",
		"---\nrequires:\n  <<: {gpu: true}\n---\n",
		"---\ntitle: " + strings.Repeat("x", 257) + "\n---\n",
		"---\ndescription: " + strings.Repeat("x", 513) + "\n---\n",
		"---\n# " + strings.Repeat("x", maxMetadataBytes) + "\n---\n",
	} {
		if _, _, err := parseMetadata("RUNBOOK.md", text); err == nil {
			t.Fatalf("invalid metadata accepted: %.120s", text)
		}
	}
	for _, text := range []string{
		`{"requires":{"note":"\u0000"}}`,
		`{"title":false}`,
	} {
		if _, _, err := parseMetadata("runbook.json", text); err == nil {
			t.Fatalf("invalid JSON metadata accepted: %s", text)
		}
	}
	for _, requirements := range []map[string]any{
		{"apps": make([]any, 65)},
		{"custom": strings.Repeat("x", 2049)},
		{strings.Repeat("key", 44): true},
		{"not-json": make(chan int)},
	} {
		if ValidateRequirements(requirements) == nil {
			t.Fatal("metadata bounds ignored")
		}
		book := Book{ID: "vpn", File: "shared/vpn.md", Source: "worker-user", Requires: requirements}
		if ValidBook(book, Selector{}) {
			t.Fatal("invalid remote metadata accepted")
		}
	}
}

func TestRunbookMetadataDefaultsAndSourceFailures(t *testing.T) {
	m, body, err := parseMetadata("RUNBOOK.md", "---\nrequires:\n  os: [MacOS]\n---\n# Mac task\n")
	if err != nil || !reflect.DeepEqual(m.Requires, map[string]any{"os": []any{"MacOS"}}) || !strings.HasPrefix(body, "# Mac task") {
		t.Fatalf("requirement values were changed: %+v, %v", m, err)
	}
	for _, text := range []string{"# Plain title\nPlain instructions.\n", "---\n---\n# Plain title\n", "\uFEFF# Plain title\n"} {
		if _, _, err := parseMetadata("RUNBOOK.md", text); err != nil {
			t.Fatal(err)
		}
	}
	for _, text := range []string{`{"capability":"exec.run","args":{}}`, `[]`, `null`} {
		if _, _, err := parseMetadata("template.json", text); err != nil {
			t.Fatal(err)
		}
	}
	bad, good := t.TempDir(), t.TempDir()
	writeBook(t, bad, "shared/task.md", "---\ntitle: [invalid, title]\n---\n")
	writeBook(t, good, "shared/task.md", "# Good\nInstructions.")
	catalog, err := Discover(context.Background(), []Root{{Source: "project", Path: bad}, {Source: "global", Path: good}}, Selector{}, Query{})
	if err != nil || catalog.Sources[0].Error == "" || len(catalog.Runbooks) != 1 || catalog.Runbooks[0].Source != "global" {
		t.Fatalf("invalid metadata hid healthy stores: %+v, %v", catalog, err)
	}
	b, err := json.Marshal(catalog)
	if err != nil || !json.Valid(b) {
		t.Fatal("catalog could not be serialized")
	}
}

func TestRunbookDynamicVendorRequirements(t *testing.T) {
	text := "---\ntitle: Private service diagnostics\nrequires:\n  os: [windows]\n  vpn:\n    vendor: Palo Alto Networks\n    client: GlobalProtect\n    state: connected\n    portalSecret: vpn.portal\n  gpu:\n    vendor: NVIDIA\n    encoder: h264_nvenc\n    minMemoryBytes: 8589934592\n  service.example:\n    reachable: true\n    endpoints: [api, status]\n---\n# Diagnostics\nCheck the declared prerequisites.\n" + strings.Repeat("x", 33<<10)
	root := t.TempDir()
	writeBook(t, root, "shared/private-service/RUNBOOK.md", text)
	catalog, err := Discover(context.Background(), []Root{{Source: "project", Path: root}}, Selector{}, Query{})
	if err != nil || len(catalog.Runbooks) != 1 {
		t.Fatalf("dynamic metadata discovery: %+v, %v", catalog, err)
	}
	book := catalog.Runbooks[0]
	if book.Machine != "" || !book.Omitted {
		t.Fatalf("dynamic requirements changed shared scope or omitted content: %+v", book)
	}
	vpn, ok := requirementsObject(t, book.Requires)["vpn"].(map[string]any)
	if !ok || vpn["vendor"] != "Palo Alto Networks" || vpn["state"] != "connected" || vpn["portalSecret"] != "vpn.portal" {
		t.Fatalf("vendor-specific VPN requirement lost: %+v", book.Requires)
	}
	gpu, ok := requirementsObject(t, book.Requires)["gpu"].(map[string]any)
	if !ok || gpu["vendor"] != "NVIDIA" || gpu["minMemoryBytes"] != json.Number("8589934592") {
		t.Fatalf("dynamic GPU requirement lost: %+v", gpu)
	}
	wire, err := json.Marshal(book)
	if err != nil {
		t.Fatal(err)
	}
	var remote Book
	if err := json.Unmarshal(wire, &remote); err != nil || !reflect.DeepEqual(remote.Requires, book.Requires) || !ValidBook(remote, Selector{Node: "candidate"}) {
		t.Fatalf("worker/client round trip changed dynamic requirements: %+v, %v", remote, err)
	}
}

func requirementsObject(t *testing.T, value any) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected structured test requirements, got %T", value)
	}
	return object
}

func TestRunbookFriendlyRequirementDescriptions(t *testing.T) {
	for _, fixture := range []struct {
		file, text string
		want       any
	}{
		{"shared/task.md", "---\nrequires: A machine suited to the requested video job, with FFmpeg available.\n---\n# Task\n", "A machine suited to the requested video job, with FFmpeg available."},
		{"shared/task.md", "---\nrequires:\n  - Able to reach the requested service.\n  - The appropriate VPN configured if needed.\n---\n# Task\n", []any{"Able to reach the requested service.", "The appropriate VPN configured if needed."}},
		{"shared/task.json", `{"requires":"Choose a machine with access to the intended VPN environment.","instructions":"Inspect current access."}`, "Choose a machine with access to the intended VPN environment."},
		{"shared/task.json", `{"requires":["Choose a suitable machine.",{"environment":"company network","vpn":"The appropriate client configured if needed."}]}`, []any{"Choose a suitable machine.", map[string]any{"environment": "company network", "vpn": "The appropriate client configured if needed."}}},
	} {
		root := t.TempDir()
		text := fixture.text + strings.Repeat(" ", 33<<10)
		writeBook(t, root, fixture.file, text)
		catalog, err := Discover(context.Background(), []Root{{Source: "project", Path: root}}, Selector{}, Query{})
		if err != nil || len(catalog.Runbooks) != 1 {
			t.Fatalf("description discovery: %+v, %v", catalog, err)
		}
		book := catalog.Runbooks[0]
		if !book.Omitted || !reflect.DeepEqual(book.Requires, fixture.want) {
			t.Fatalf("friendly requirements were not retained: %+v", book)
		}
		wire, err := json.Marshal(book)
		if err != nil {
			t.Fatal(err)
		}
		var remote Book
		if err := json.Unmarshal(wire, &remote); err != nil || !reflect.DeepEqual(remote.Requires, fixture.want) || !ValidBook(remote, Selector{}) {
			t.Fatalf("friendly requirements changed across discovery: %+v, %v", remote, err)
		}
	}
}

func TestRunbookDynamicRequirementBounds(t *testing.T) {
	tooManyFields := map[string]any{}
	for i := range 65 {
		tooManyFields[strings.Repeat("x", i+1)] = true
	}
	tooManyValues := map[string]any{}
	for i := range 32 {
		tooManyValues[strings.Repeat("x", i+1)] = make([]any, 32)
	}
	tooDeep := map[string]any{}
	for range 10 {
		tooDeep = map[string]any{"nested": tooDeep}
	}
	tooLarge := map[string]any{}
	for i := range 32 {
		tooLarge[strings.Repeat("x", i+1)] = strings.Repeat("z", 1024)
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	for _, r := range []map[string]any{tooManyFields, tooManyValues, tooDeep, tooLarge, cycle} {
		if ValidateRequirements(r) == nil {
			t.Fatal("unbounded dynamic requirements accepted")
		}
	}
	var r Book
	for _, raw := range []string{`{"vpn":{}} {}`, `{"value":1e999}`, `{"value":"\u0000"}`, `"` + strings.Repeat("x", 2049) + `"`} {
		if err := json.Unmarshal([]byte(`{"requires":`+raw+`}`), &r); err == nil {
			t.Fatal("invalid remote requirements accepted")
		}
	}
	_, _, err := parseMetadata("runbook.json", `{"requires":{"vpn":{"vendor":"Example vendor","state":"connected"},"custom":{"revision":9007199254740993}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"requires":{"custom":{"revision":9007199254740993}}}`), &r); err != nil {
		t.Fatal(err)
	}
	if requirementsObject(t, r.Requires)["custom"].(map[string]any)["revision"] != json.Number("9007199254740993") {
		t.Fatal("numeric requirement rounded during decoding")
	}
}
