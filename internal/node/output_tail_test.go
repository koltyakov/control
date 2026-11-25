package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport"
)

const tailTestFinal = "the error that matters\n"

// TestOutputTailProcess is a child process emitting large output.
func TestOutputTailProcess(t *testing.T) {
	mode := os.Getenv("CONTROL_TEST_OUTPUT_TAIL")
	if mode == "" {
		return
	}
	out := bufio.NewWriterSize(os.Stdout, 64<<10)
	fill := func(n int) {
		line := []byte(strings.Repeat("x", 63) + "\n")
		for ; n >= len(line); n -= len(line) {
			_, _ = out.Write(line)
		}
		_, _ = out.Write(line[:n])
	}
	switch mode {
	case "big-stdout":
		fill(3 << 20)
		_, _ = out.WriteString("FINAL ERROR\n")
		_, _ = os.Stderr.WriteString("stderr-ok")
	case "big-log":
		fill(taskLogHead + 5<<19)
		_, _ = out.WriteString(tailTestFinal)
	case "big-log-wait":
		fill(taskLogHead + 200<<10)
		_, _ = out.WriteString("waiting\n")
		_ = out.Flush()
		time.Sleep(time.Minute)
	case "secret-tail":
		fill(3 << 20)
		_, _ = out.WriteString(os.Getenv("CONTROL_TEST_PASSWORD"))
	}
	_ = out.Flush()
	os.Exit(0)
}

func tailProcess(t *testing.T, mode string) map[string]any {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"command": path, "args": []string{"-test.run=^TestOutputTailProcess$"}, "env": map[string]string{"CONTROL_TEST_OUTPUT_TAIL": mode}}
}

type tailLogResponse struct {
	Text       string `json:"text"`
	Offset     int64  `json:"offset"`
	Terminal   bool   `json:"terminal"`
	Tail       bool   `json:"tail"`
	Omitted    *int64 `json:"omitted"`
	TailOffset int64  `json:"tailOffset"`
	Size       int64  `json:"size"`
}

func readTailLog(t *testing.T, n *Node, id string, offset int64, tail bool) tailLogResponse {
	t.Helper()
	params := map[string]any{"id": id, "offset": offset}
	if tail {
		params["tail"] = true
	}
	value, err := n.taskMethod(context.Background(), "owner", "tasks.logs", model.JSON(params))
	if err != nil {
		t.Fatal(err)
	}
	var response tailLogResponse
	if err := json.Unmarshal(model.JSON(value), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func writeChunks(t *testing.T, w io.Writer, data []byte, size int) {
	t.Helper()
	for len(data) > 0 {
		n := min(size, len(data))
		if written, err := w.Write(data[:n]); err != nil || written != n {
			t.Fatal("short write", written, err)
		}
		data = data[n:]
	}
}

func TestCaptureKeepsCompleteOutputWithinLimit(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("0123456789abcdef"), captureLimit/16)
	for _, chunk := range []int{7, 4096, len(data)} {
		c := &capture{keepTail: true}
		writeChunks(t, c, data, chunk)
		text, omitted := c.output()
		if c.truncated || omitted != 0 || text != string(data) {
			t.Fatal("output at the limit was not complete", chunk, c.truncated, omitted, len(text))
		}
	}
}

func TestCaptureRetainsHeadAndTail(t *testing.T) {
	t.Parallel()
	data := make([]byte, 3*captureLimit+12345)
	for i := range data {
		data[i] = byte('a' + i%26)
	}
	wantHead := string(data[:captureHead])
	wantTail := string(data[len(data)-captureTail:])
	wantOmitted := int64(len(data) - captureHead - captureTail)
	// A first write that overflows from below the head, one from above it,
	// and many small writes take different paths into the ring.
	for _, chunks := range [][]int{{len(data)}, {captureHead - 3, len(data)}, {captureLimit - 1, 5}, {4093}, {1 << 20}} {
		c := &capture{keepTail: true}
		rest := data
		for i := 0; len(rest) > 0; i++ {
			n := min(chunks[min(i, len(chunks)-1)], len(rest))
			_, _ = c.Write(rest[:n])
			rest = rest[n:]
		}
		text, omitted := c.output()
		if !c.truncated || omitted != wantOmitted {
			t.Fatal("wrong omission", chunks, c.truncated, omitted, wantOmitted)
		}
		if text != wantHead+omissionMarker(wantOmitted)+wantTail {
			t.Fatal("head, marker and tail were not retained exactly", chunks)
		}
		if len(text) > captureLimit || len(c.kept) > captureLimit {
			t.Fatal("capture exceeded its per-stream bound", len(text), len(c.kept))
		}
	}
	// The default capture used by adapters requiring complete output keeps
	// only its head and still reports truncation.
	c := &capture{}
	_, _ = c.Write(data)
	if text, omitted := c.output(); !c.truncated || omitted != 0 || text != string(data[:captureLimit]) {
		t.Fatal("head-only capture changed")
	}
}

func TestCaptureNeverSplitsUTF8(t *testing.T) {
	// Head and tail limits are not multiples of the three-byte rune: the head
	// cut falls after its first byte, and a final ASCII byte moves the tail
	// cut after a rune's first byte.
	runes := strings.Repeat("€", (2*captureLimit)/3)
	data := runes + "Z"
	c := &capture{keepTail: true}
	writeChunks(t, c, []byte(data), 65536)
	text, omitted := c.output()
	marker := omissionMarker(omitted)
	head, tail, ok := strings.Cut(text, marker)
	if !ok || !utf8.ValidString(text) {
		t.Fatal("split a UTF-8 sequence or lost the marker")
	}
	if len(head) != captureHead-1 || len(tail) != captureTail-2 || !strings.HasSuffix(tail, "€Z") {
		t.Fatal("cuts did not land on rune boundaries", len(head), len(tail))
	}
	if omitted != int64(len(data)-len(head)-len(tail)) {
		t.Fatal("omitted count excludes trimmed partial sequences", omitted)
	}
}

func TestExecRunReturnsHeadAndTail(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	// Task logs serialize both streams; so does this test log.
	var head bytes.Buffer
	logs := &limitedLog{w: &head, remaining: 1 << 30}
	value, err := n.execRun(t.Context(), model.JSON(tailProcess(t, "big-stdout")), Execution{Dir: n.Config.WorkDir, Log: logs})
	if err != nil {
		t.Fatal(err)
	}
	result := value.(commandResult)
	total := int64(3<<20 + len("FINAL ERROR\n"))
	if !result.Truncated || result.StdoutOmitted != total-captureHead-captureTail || result.StderrOmitted != 0 || result.Stderr != "stderr-ok" {
		t.Fatal("unexpected command result", result.Truncated, result.StdoutOmitted, result.StderrOmitted, result.Stderr)
	}
	if !strings.HasSuffix(result.Stdout, "x\nFINAL ERROR\n") || !strings.Contains(result.Stdout, omissionMarker(result.StdoutOmitted)) {
		t.Fatal("lost the end of truncated output")
	}
	if int64(head.Len()) != total+int64(len("stderr-ok")) {
		t.Fatal("raw command logs changed", head.Len())
	}
	encoded := string(model.JSON(result))
	if !strings.Contains(encoded, `"truncated":true`) || !strings.Contains(encoded, `"stdoutOmitted":`) || strings.Contains(encoded, `"stderrOmitted"`) {
		t.Fatal("unexpected result fields", encoded[len(encoded)-120:])
	}
}

func TestSecretCommandTailStaysSuppressed(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	setTestSecret(t, n, "tail.password", "tail-private-token")
	args := tailProcess(t, "secret-tail")
	args["envSecrets"] = map[string]string{"CONTROL_TEST_PASSWORD": "tail.password"}
	var logs bytes.Buffer
	value, err := n.execRun(t.Context(), model.JSON(args), Execution{Dir: n.Config.WorkDir, Log: &logs})
	result := value.(commandResult)
	if err == nil || !result.Truncated || result.Stdout != "" || result.Stderr != "" || result.StdoutOmitted != 0 || logs.Len() != 0 {
		t.Fatal("secret-bearing truncated output was published", err, result.StdoutOmitted, logs.Len())
	}
	// A tracked task keeps its raw log empty and never persists a tail.
	task, err := n.startTaskContext(t.Context(), "owner", model.TaskSpec{ID: "secret-tail", Capability: "exec.run", Args: model.JSON(args)})
	if err != nil {
		t.Fatal(err)
	}
	if task, err = n.waitTask(t.Context(), "owner", task.ID, 30*time.Second); err != nil || task.State != "failed" {
		t.Fatal("secret-bearing task", task.State, err)
	}
	for _, path := range []string{n.taskPath(task.ID), n.logPath(task.ID)} {
		data, _ := os.ReadFile(path)
		if bytes.Contains(data, []byte("tail-private-token")) || bytes.Contains(data, []byte("xxxx")) {
			t.Fatal("secret-bearing output persisted", path)
		}
	}
	if _, err := os.Stat(n.logTailPath(task.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("secret-bearing task left a log tail", err)
	}
	if got := readTailLog(t, n, task.ID, 0, true); got.Tail || got.Omitted != nil || got.Text != "" {
		t.Fatal("secret-bearing task served log text", got)
	}
}

func TestTaskLogTailPersistsAndPrunes(t *testing.T) {
	t.Parallel()
	cfg := Config{Name: "local", Gateway: "http://127.0.0.1:1", Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), TaskRetentionHours: 1}
	n := openLocalNode(t, cfg)
	task, err := n.startTaskContext(t.Context(), "owner", model.TaskSpec{ID: "long-build", Capability: "exec.run", Args: model.JSON(tailProcess(t, "big-log"))})
	if err != nil {
		t.Fatal(err)
	}
	if task, err = n.waitTask(t.Context(), "owner", task.ID, time.Minute); err != nil || task.State != "succeeded" {
		t.Fatal("task", task.State, task.Error, err)
	}
	total := int64(taskLogHead + 5<<19 + len(tailTestFinal))
	wantOmitted := total - taskLogHead - taskLogTail
	if info, err := os.Stat(n.logPath(task.ID)); err != nil || info.Size() != taskLogHead {
		t.Fatal("head log changed", err)
	}

	// Offset reads keep their semantics; only a read reaching the end of
	// the head reports the retained tail.
	first := readTailLog(t, n, task.ID, 0, false)
	if len(first.Text) != transport.MaxLogChunk || first.Offset != transport.MaxLogChunk || first.Omitted != nil || first.Tail {
		t.Fatal("first head chunk changed", first.Offset, first.Omitted)
	}
	last := readTailLog(t, n, task.ID, taskLogHead-10, false)
	if len(last.Text) != 10 || last.Offset != taskLogHead || !last.Terminal || last.Tail || last.Omitted == nil || *last.Omitted != wantOmitted || last.TailOffset != taskLogHead+wantOmitted || last.Size != total {
		t.Fatal("end of head did not report the tail", last.Offset, last.Omitted, last.TailOffset, last.Size)
	}
	end := readTailLog(t, n, task.ID, taskLogHead, false)
	if end.Text != "" || end.Offset != taskLogHead || !end.Terminal {
		t.Fatal("reading past the head changed", end)
	}

	// The follow stream still drains exactly the head and terminates.
	var followed int64
	terminal := false
	err = n.localTaskLogs(t.Context(), "owner", task.ID, 0, func(chunk model.TaskLogChunk) error {
		followed += int64(len(chunk.Data))
		terminal = terminal || chunk.Terminal
		return nil
	})
	if err != nil || followed != taskLogHead || !terminal {
		t.Fatal("follow stream changed", followed, terminal, err)
	}

	tailStart := readTailLog(t, n, task.ID, 0, true)
	if !tailStart.Tail || tailStart.TailOffset != taskLogHead+wantOmitted || tailStart.Offset != tailStart.TailOffset+transport.MaxLogChunk || len(tailStart.Text) != transport.MaxLogChunk {
		t.Fatal("tail read did not start at the retained tail", tailStart.TailOffset, tailStart.Offset)
	}
	tailEnd := readTailLog(t, n, task.ID, total-100, true)
	if !tailEnd.Tail || !strings.HasSuffix(tailEnd.Text, "x\n"+tailTestFinal) || tailEnd.Offset != total || len(tailEnd.Text) != 100 {
		t.Fatal("tail did not end with the final output", tailEnd.Offset, tailEnd.Text)
	}
	if past := readTailLog(t, n, task.ID, total+5, true); !past.Tail || past.Text != "" || past.Offset != total+5 {
		t.Fatal("read past the tail", past)
	}

	// Reopening serves the persisted tail unchanged.
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	n = openLocalNode(t, cfg)
	if again := readTailLog(t, n, task.ID, total-100, true); again.Text != tailEnd.Text || again.Offset != total || again.TailOffset != tailEnd.TailOffset || again.Omitted == nil || *again.Omitted != wantOmitted || !again.Terminal {
		t.Fatal("persisted tail changed across restart")
	}

	// Retention removes the tail with the log, leaving a tombstone.
	if err := n.pruneTasks(time.Now().Add(2 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{n.logPath(task.ID), n.logTailPath(task.ID)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("pruned task left", path, err)
		}
	}
	if pruned := readTailLog(t, n, task.ID, 0, true); pruned.Tail || pruned.Text != "" || !pruned.Terminal || pruned.Omitted != nil {
		t.Fatal("pruned task served a tail", pruned)
	}
}

func TestTaskLogTailWhileRunning(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	task, err := n.startTaskContext(t.Context(), "owner", model.TaskSpec{ID: "still-running", Capability: "exec.run", Args: model.JSON(tailProcess(t, "big-log-wait"))})
	if err != nil {
		t.Fatal(err)
	}
	total := int64(taskLogHead + 200<<10 + len("waiting\n"))
	deadline := time.Now().Add(time.Minute)
	var live tailLogResponse
	for {
		live = readTailLog(t, n, task.ID, total-8, true)
		if live.Text == "waiting\n" || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Nothing is omitted yet: the live tail continues the head contiguously.
	if !live.Tail || live.Terminal || live.Omitted == nil || *live.Omitted != 0 || live.TailOffset != taskLogHead || live.Size != total {
		t.Fatal("live tail", live)
	}
	if _, err := os.Stat(n.logTailPath(task.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("tail persisted before completion", err)
	}
	if _, err := n.taskMethod(t.Context(), "owner", "tasks.cancel", model.JSON(map[string]any{"id": task.ID})); err != nil {
		t.Fatal(err)
	}
	if task, err = n.waitTask(t.Context(), "owner", task.ID, time.Minute); err != nil || task.State != "cancelled" {
		t.Fatal("cancel", task.State, err)
	}
	if final := readTailLog(t, n, task.ID, total-8, true); final.Text != "waiting\n" || !final.Terminal || final.Size != total {
		t.Fatal("cancelled task lost its tail", final)
	}
	if _, err := n.taskMethod(t.Context(), "other", "tasks.logs", model.JSON(map[string]any{"id": task.ID, "tail": true})); err == nil {
		t.Fatal("another owner read the tail")
	}
}

func TestLogTailBoundariesAndFallback(t *testing.T) {
	var head bytes.Buffer
	w := &limitedLog{w: &head, remaining: 16, tail: &logTail{}}
	_, _ = w.Write([]byte("0123456789abcdef"))
	if _, _, ok := w.tailView(0, taskLogTail); ok {
		t.Fatal("tail reported before the head limit was exceeded")
	}
	// A contiguous tail keeps every byte, even one cut inside a rune.
	_, _ = w.Write([]byte("€tail"))
	state, data, ok := w.tailView(0, taskLogTail)
	if !ok || state != (logTailState{head: 16, start: 16, size: 23}) || string(data) != "€tail" || head.String() != "0123456789abcdef" {
		t.Fatal("contiguous tail", state, string(data))
	}
	// Once bytes are dropped the tail starts on a rune boundary, and the
	// skipped continuation bytes count as omitted.
	_, _ = w.Write(bytes.Repeat([]byte("€"), taskLogTail/3+5))
	_, _ = w.Write([]byte("ZZ"))
	state, data, ok = w.tailView(0, taskLogTail)
	size := int64(23 + 3*(taskLogTail/3+5) + 2)
	if !ok || state.size != size || state.head != 16 || !utf8.Valid(data) || int64(len(data)) != size-state.start || state.start-state.head <= 0 || !bytes.HasSuffix(data, []byte("€ZZ")) {
		t.Fatal("dropped tail", state, len(data))
	}
	if int64(len(data)) != taskLogTail-2 {
		t.Fatal("tail start did not skip exactly the cut sequence", len(data))
	}

	// A task without a tail answers a tail read as an ordinary head read,
	// the same shape an older worker returns for the ignored field.
	n := localNode(t, nil)
	n.tasks["small"] = &model.Task{ID: "small", Owner: "owner", State: "succeeded"}
	if err := os.MkdirAll(filepath.Dir(n.logPath("small")), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(n.logPath("small"), []byte("complete log"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tail := range []bool{false, true} {
		if got := readTailLog(t, n, "small", 3, tail); got.Tail || got.Omitted != nil || got.Text != "plete log" || got.Offset != 12 || !got.Terminal {
			t.Fatal("log without a tail", tail, got)
		}
	}
	// A corrupt tail fails explicit tail reads but never head reads.
	if err := os.WriteFile(n.logTailPath("small"), []byte("CTLTAIL1short"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := readTailLog(t, n, "small", 0, false); got.Text != "complete log" || got.Omitted != nil {
		t.Fatal("corrupt tail affected a head read", got)
	}
	if _, err := n.taskMethod(t.Context(), "owner", "tasks.logs", model.JSON(map[string]any{"id": "small", "tail": true})); err == nil {
		t.Fatal("served a corrupt tail")
	}
}
