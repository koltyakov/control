package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/wire"
)

// TestConnection measures one authenticated bulk stream, without disk staging or
// replay. Upload/download are sequential and relative to the initiating peer.
func (p *Peer) TestConnection(ctx context.Context, target string, options model.ConnectionTestOptions) (model.ConnectionTestResult, error) {
	result := model.ConnectionTestResult{StartedAt: time.Now().UTC(), SourceID: p.IdentityID()}
	q, err := options.Normalize()
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, model.ConnectionTestTimeout)
	defer cancel()
	destination, err := p.Lookup(ctx, target)
	if err != nil {
		return result, err
	}
	if destination.ID == p.IdentityID() {
		return result, errors.New("connection test requires distinct peers")
	}
	if !destination.Online || destination.Disabled || destination.ControlPending {
		return result, errors.New("connection test target is offline, disabled, or awaiting policy acknowledgement")
	}
	result.Target, result.TargetID = destination.Name, destination.ID
	started := time.Now()
	conn, ack, err := p.OpenRPC(ctx, destination.ID, model.ConnectionOpenMethod, model.ConnectionOpenRequest{Protocol: model.ConnectionTestProtocol, ConnectionTestOptions: q})
	if err != nil {
		return result, fmt.Errorf("open connection test (upgrade the target if unsupported): %w", err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()); _ = conn.Close() })
	defer stop()
	var ready struct {
		Protocol string `json:"protocol"`
	}
	if err = json.Unmarshal(ack, &ready); err != nil || ready.Protocol != model.ConnectionTestProtocol {
		return result, errors.New("unsupported connection test protocol; upgrade the target")
	}
	result.SetupMillis = float64(time.Since(started)) / float64(time.Millisecond)
	result.Transport = conn.(*sessionStream).mode
	result.Latency, result.Upload, result.Download, err = runConnectionTest(conn, q)
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return result, err
}

func runConnectionTest(conn net.Conn, q model.ConnectionTestOptions) (latency model.ConnectionLatency, upload, download model.ConnectionThroughput, err error) {
	// Warm up before sampling, so connection establishment is reported separately.
	var ping, reply [8]byte
	var previous, jitter, total float64
	for i := 0; i <= q.Samples; i++ {
		binary.BigEndian.PutUint64(ping[:], uint64(i))
		started := time.Now()
		if _, err = conn.Write(ping[:]); err != nil {
			return
		}
		if _, err = io.ReadFull(conn, reply[:]); err != nil {
			return
		}
		if ping != reply {
			err = errors.New("connection test echo mismatch")
			return
		}
		if i == 0 {
			continue
		}
		ms := float64(time.Since(started)) / float64(time.Millisecond)
		if i == 1 || ms < latency.MinMillis {
			latency.MinMillis = ms
		}
		latency.MaxMillis = math.Max(latency.MaxMillis, ms)
		if i > 1 {
			jitter += math.Abs(ms - previous)
		}
		previous, total = ms, total+ms
	}
	latency.Samples, latency.MeanMillis = q.Samples, total/float64(q.Samples)
	if q.Samples > 1 {
		latency.JitterMillis = jitter / float64(q.Samples-1)
	}
	started := time.Now()
	if err = sendTestBytes(conn, q.Bytes); err != nil {
		return
	}
	if err = readTestReceipt(conn); err != nil {
		return
	}
	upload = testThroughput(q.Bytes, time.Since(started))
	// The receiver waits for this marker, so download does not overlap upload.
	if _, err = conn.Write([]byte{1}); err != nil {
		return
	}
	started = time.Now()
	if err = receiveTestBytes(conn, q.Bytes); err != nil {
		return
	}
	download = testThroughput(q.Bytes, time.Since(started))
	if err = wire.WriteFrame(conn, model.Response{Result: model.JSON(map[string]bool{"verified": true})}); err != nil {
		return
	}
	err = readTestReceipt(conn)
	return
}

// ServeConnectionTest runs only after the node checks membership, instruction
// authority, method permissions, concurrency, and maintenance admission.
func ServeConnectionTest(conn net.Conn, q model.ConnectionTestOptions) error {
	var err error
	q, err = q.Normalize()
	if err != nil {
		return err
	}
	if err := wire.WriteFrame(conn, model.Response{Result: model.JSON(map[string]string{"protocol": model.ConnectionTestProtocol})}); err != nil {
		return err
	}
	var ping [8]byte
	for i := 0; i <= q.Samples; i++ {
		if _, err := io.ReadFull(conn, ping[:]); err != nil {
			return err
		}
		if _, err := conn.Write(ping[:]); err != nil {
			return err
		}
	}
	if err := receiveTestBytes(conn, q.Bytes); err != nil {
		return err
	}
	if err := wire.WriteFrame(conn, model.Response{Result: model.JSON(map[string]bool{"verified": true})}); err != nil {
		return err
	}
	var marker [1]byte
	if _, err := io.ReadFull(conn, marker[:]); err != nil {
		return err
	}
	if marker[0] != 1 {
		return errors.New("invalid connection test phase")
	}
	if err := sendTestBytes(conn, q.Bytes); err != nil {
		return err
	}
	if err := readTestReceipt(conn); err != nil {
		return err
	}
	return wire.WriteFrame(conn, model.Response{Result: model.JSON(map[string]bool{"verified": true})})
}

func sendTestBytes(w io.Writer, size int64) error {
	buffer := make([]byte, 64<<10)
	if _, err := rand.Read(buffer); err != nil {
		return err
	}
	hash := sha256.New()
	for remaining := size; remaining > 0; {
		chunk := buffer[:min(int64(len(buffer)), remaining)]
		n, err := w.Write(chunk)
		if err != nil {
			return err
		}
		if n != len(chunk) {
			return io.ErrShortWrite
		}
		_, _ = hash.Write(chunk)
		remaining -= int64(n)
	}
	digest := hash.Sum(nil)
	n, err := w.Write(digest)
	if err == nil && n != len(digest) {
		err = io.ErrShortWrite
	}
	return err
}

func receiveTestBytes(r io.Reader, size int64) error {
	hash := sha256.New()
	n, err := io.CopyBuffer(hash, io.LimitReader(r, size), make([]byte, 64<<10))
	if err != nil {
		return err
	}
	if n != size {
		return io.ErrUnexpectedEOF
	}
	var digest [sha256.Size]byte
	if _, err := io.ReadFull(r, digest[:]); err != nil {
		return err
	}
	if !bytes.Equal(digest[:], hash.Sum(nil)) {
		return errors.New("connection test checksum mismatch")
	}
	return nil
}

func readTestReceipt(r io.Reader) error {
	var receipt model.Response
	if err := wire.ReadFrame(r, &receipt); err != nil {
		return err
	}
	if receipt.Error != "" {
		return errors.New(receipt.Error)
	}
	var verified struct {
		Verified bool `json:"verified"`
	}
	if err := json.Unmarshal(receipt.Result, &verified); err != nil {
		return err
	}
	if !verified.Verified {
		return errors.New("connection test was not verified")
	}
	return nil
}

func testThroughput(size int64, elapsed time.Duration) model.ConnectionThroughput {
	seconds := max(elapsed.Seconds(), 1e-9)
	return model.ConnectionThroughput{Bytes: size, Seconds: seconds, Mbps: float64(size) * 8 / seconds / 1e6}
}
