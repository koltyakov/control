package node

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/transport"
)

const (
	// taskLogHead bounds the task log file served by offset reads and follow
	// streams. Its semantics predate the retained tail and stay unchanged.
	taskLogHead = 10 << 20
	// taskLogTail bounds the last output bytes kept beyond the head: in memory
	// while the task runs, then in dataDir/tasks/ID.tail when it ends.
	taskLogTail = 1 << 20
	// A persisted tail is magic, head bytes and the tail's absolute start
	// offset, followed by the tail bytes. It is replaced atomically.
	logTailMagic  = "CTLTAIL1"
	logTailHeader = len(logTailMagic) + 16
)

var errInvalidLogTail = errors.New("retained task log tail is invalid; head log reads are unaffected")

func (n *Node) logTailPath(id string) string {
	return filepath.Join(n.Config.DataDir, "tasks", id+".tail")
}

// logTail keeps the last taskLogTail bytes written beyond the head limit.
// The ring is allocated only after the head is full. The owning limitedLog's
// mutex guards it.
type logTail struct {
	head   int64
	total  int64
	ring   []byte
	next   int
	filled int
	sealed bool
}

// logTailState locates the retained tail by absolute byte offsets in the
// task's complete output. Bytes in [head, start) were not retained.
type logTailState struct {
	head, start, size int64
}

func (s logTailState) annotate(response map[string]any) {
	response["omitted"] = s.start - s.head
	response["tailOffset"] = s.start
	response["size"] = s.size
}

func (t *logTail) add(b []byte) {
	if t.sealed || len(b) == 0 {
		return
	}
	if t.ring == nil {
		t.ring = make([]byte, taskLogTail)
	}
	t.total += int64(len(b))
	t.next, t.filled = ringWrite(t.ring, t.next, t.filled, b)
}

// at returns the i-th oldest retained byte.
func (t *logTail) at(i int) byte {
	oldest := 0
	if t.filled == len(t.ring) {
		oldest = t.next
	}
	return t.ring[(oldest+i)%len(t.ring)]
}

func (t *logTail) state() logTailState {
	size := t.head + t.total
	start := size - int64(t.filled)
	if t.total > int64(t.filled) {
		// Bytes before the ring were dropped. Do not begin inside a UTF-8
		// sequence; a contiguous tail keeps every byte after the head.
		skip := 0
		for skip < 3 && skip < t.filled && isContinuation(t.at(skip)) {
			skip++
		}
		start += int64(skip)
	}
	return logTailState{head: t.head, start: start, size: size}
}

func isContinuation(b byte) bool { return b&0xC0 == 0x80 }

// tailView copies at most limit retained bytes starting at the absolute
// offset, or at the tail start when offset precedes it. A negative offset
// returns only the state. ok is false when the head limit was never exceeded.
func (w *limitedLog) tailView(offset int64, limit int) (logTailState, []byte, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t := w.tail
	if t == nil || t.total == 0 {
		return logTailState{}, nil, false
	}
	state := t.state()
	if offset < 0 {
		return state, nil, true
	}
	from := max(offset, state.start)
	if from >= state.size {
		return state, nil, true
	}
	out := make([]byte, min(int64(limit), state.size-from))
	oldest := 0
	if t.filled == len(t.ring) {
		oldest = t.next
	}
	// out never extends past the filled bytes, so at most one wrap occurs.
	p := (oldest + int(from-(state.size-int64(t.filled)))) % len(t.ring)
	copy(out[copy(out, t.ring[p:]):], t.ring)
	return state, out, true
}

// persistLogTail seals the live tail and writes it before the task can become
// terminal. Logs that never exceeded the head have no tail file.
func (n *Node) persistLogTail(id string, w *limitedLog) error {
	w.mu.Lock()
	if w.tail != nil {
		w.tail.sealed = true
	}
	w.mu.Unlock()
	state, data, ok := w.tailView(0, taskLogTail)
	if !ok {
		return nil
	}
	b := make([]byte, logTailHeader+len(data))
	copy(b, logTailMagic)
	binary.BigEndian.PutUint64(b[len(logTailMagic):], uint64(state.head))
	binary.BigEndian.PutUint64(b[len(logTailMagic)+8:], uint64(state.start))
	copy(b[logTailHeader:], data)
	return store.Bytes(n.logTailPath(id), b, 0600)
}

// persistedLogTail reads a chunk of a finished task's retained tail, with the
// same offset rules as tailView.
func (n *Node) persistedLogTail(id string, offset int64, limit int) (logTailState, []byte, bool, error) {
	f, err := os.Open(n.logTailPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return logTailState{}, nil, false, nil
	}
	if err != nil {
		return logTailState{}, nil, false, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return logTailState{}, nil, false, err
	}
	var header [logTailHeader]byte
	if _, err = io.ReadFull(f, header[:]); err != nil {
		return logTailState{}, nil, false, errInvalidLogTail
	}
	head := binary.BigEndian.Uint64(header[len(logTailMagic):])
	start := binary.BigEndian.Uint64(header[len(logTailMagic)+8:])
	length := info.Size() - int64(logTailHeader)
	if string(header[:len(logTailMagic)]) != logTailMagic || length <= 0 || length > taskLogTail || head > taskLogHead || start < head || start > 1<<62 {
		return logTailState{}, nil, false, errInvalidLogTail
	}
	state := logTailState{head: int64(head), start: int64(start), size: int64(start) + length}
	if offset < 0 {
		return state, nil, true, nil
	}
	from := max(offset, state.start)
	if from >= state.size {
		return state, nil, true, nil
	}
	data := make([]byte, min(int64(limit), state.size-from))
	if _, err = f.ReadAt(data, int64(logTailHeader)+from-state.start); err != nil {
		return logTailState{}, nil, false, err
	}
	return state, data, true, nil
}

// retainedLogTail reads the live tail of a running task, or the persisted
// tail once its writer was released.
func (n *Node) retainedLogTail(id string, live *limitedLog, offset int64) (logTailState, []byte, bool, error) {
	if live != nil {
		state, data, ok := live.tailView(offset, transport.MaxLogChunk)
		return state, data, ok, nil
	}
	return n.persistedLogTail(id, offset, transport.MaxLogChunk)
}

// taskLogSnapshot serves a non-streaming tasks.logs read. Head reads keep
// their original byte-offset semantics; a read reaching the end of the head
// also reports the retained tail once the head limit was exceeded. A tail
// read uses absolute offsets and falls back to a head read when there is no
// tail, as an older worker that ignores the field would.
func (n *Node) taskLogSnapshot(id string, offset int64, tail, terminal bool, live *limitedLog) (map[string]any, error) {
	if tail {
		state, data, ok, err := n.retainedLogTail(id, live, offset)
		if err != nil {
			return nil, err
		}
		if ok {
			from := max(offset, state.start)
			if len(data) == transport.MaxLogChunk && from+int64(len(data)) < state.size {
				if trimmed := completeText(data); len(trimmed) != 0 {
					data = trimmed
				}
			}
			response := map[string]any{"text": string(data), "offset": from + int64(len(data)), "terminal": terminal, "tail": true}
			state.annotate(response)
			return response, nil
		}
	}
	f, err := os.Open(n.logPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{"text": "", "offset": offset, "terminal": terminal}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, transport.MaxLogChunk))
	response := map[string]any{"text": string(b), "offset": offset + int64(len(b)), "terminal": terminal}
	if err == nil && len(b) < transport.MaxLogChunk {
		// Only a read at the end of the head looks for a tail, so mid-log
		// reads cost what they did before. Tail errors never fail head reads.
		if state, _, ok, tailErr := n.retainedLogTail(id, live, -1); ok && tailErr == nil {
			state.annotate(response)
		}
	}
	return response, err
}

// ringWrite appends b to a circular buffer holding its last len(ring) bytes
// and returns the new write position and fill.
func ringWrite(ring []byte, next, filled int, b []byte) (int, int) {
	if len(b) >= len(ring) {
		copy(ring, b[len(b)-len(ring):])
		return 0, len(ring)
	}
	for len(b) > 0 {
		n := copy(ring[next:], b)
		b = b[n:]
		next = (next + n) % len(ring)
		filled = min(filled+n, len(ring))
	}
	return next, filled
}
