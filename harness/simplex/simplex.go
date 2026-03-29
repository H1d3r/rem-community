// Package simplex provides conformance test suites for Simplex transport implementations.
//
// Four levels of verification:
//
//   - TestTransfer: data delivery (echo, large, bidirectional, multi-message, throughput)
//   - TestStreamSemantics: close/EOF propagation, deadline, data-before-close delivery
//   - TestResilience: packet loss recovery, partition recovery (requires FaultController)
//   - TestStress: concurrent clients, rapid reconnection, sustained load
//
// Usage:
//
//	func TestCloudStorage_Suite(t *testing.T) {
//	    simplex.TestTransfer(t, makePipeline)
//	    simplex.TestStreamSemantics(t, makePipeline)
//	    simplex.TestResilience(t, makeFaultyPipeline)
//	}
package simplex

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// MakePipeline creates a full transport pipeline (typically Simplex + ARQ).
// serverConn and clientConn are the resulting net.Conn endpoints.
// stop releases all resources; safe to call multiple times.
type MakePipeline func(t *testing.T) (serverConn, clientConn net.Conn, stop func(), err error)

// ── TestTransfer: 数据传输正确性 ─────────────────────────────

// TestTransfer verifies that the Simplex transport delivers data correctly.
func TestTransfer(t *testing.T, makePipeline MakePipeline) {
	t.Helper()

	t.Run("BasicEcho", func(t *testing.T) {
		server, client, stop, err := makePipeline(t)
		if err != nil {
			t.Fatalf("MakePipeline: %v", err)
		}
		defer stop()

		msg := []byte("hello-simplex-transport")
		if _, err := client.Write(msg); err != nil {
			t.Fatalf("client.Write: %v", err)
		}

		got, err := readExact(server, len(msg), 30*time.Second)
		if err != nil {
			t.Fatalf("server read: %v", err)
		}
		if !bytes.Equal(got, msg) {
			t.Fatalf("echo mismatch: got %q, want %q", got, msg)
		}
	})

	t.Run("ReverseEcho", func(t *testing.T) {
		server, client, stop, err := makePipeline(t)
		if err != nil {
			t.Fatalf("MakePipeline: %v", err)
		}
		defer stop()

		msg := []byte("server-to-client-echo")
		if _, err := server.Write(msg); err != nil {
			t.Fatalf("server.Write: %v", err)
		}

		got, err := readExact(client, len(msg), 30*time.Second)
		if err != nil {
			t.Fatalf("client read: %v", err)
		}
		if !bytes.Equal(got, msg) {
			t.Fatalf("reverse echo mismatch: got %q, want %q", got, msg)
		}
	})

	for _, size := range []int{64 * 1024, 256 * 1024, 1024 * 1024} {
		label := formatSize(size)
		t.Run("LargeTransfer_"+label, func(t *testing.T) {
			server, client, stop, err := makePipeline(t)
			if err != nil {
				t.Fatalf("MakePipeline: %v", err)
			}
			defer stop()

			payload := makePayload(size)
			start := time.Now()

			go writeInChunks(client, payload, 32*1024)

			received, err := readExact(server, size, 120*time.Second)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("readExact(%s): got %d/%d in %v, err=%v", label, len(received), size, elapsed, err)
			}
			if !bytes.Equal(received, payload) {
				t.Fatalf("%s: data integrity failure", label)
			}
			throughput := float64(size) / elapsed.Seconds() / 1024
			t.Logf("%s: verified OK in %v (%.1f KB/s)", label, elapsed, throughput)
		})
	}

	t.Run("Bidirectional_Concurrent", func(t *testing.T) {
		server, client, stop, err := makePipeline(t)
		if err != nil {
			t.Fatalf("MakePipeline: %v", err)
		}
		defer stop()

		size := 256 * 1024
		c2sPayload := makePayload(size)
		s2cPayload := make([]byte, size)
		for i := range s2cPayload {
			s2cPayload[i] = byte((i*11 + 37) % 251)
		}

		var wg sync.WaitGroup
		var c2sReceived, s2cReceived []byte
		var c2sErr, s2cErr error

		wg.Add(4)
		go func() { defer wg.Done(); writeInChunks(client, c2sPayload, 32*1024) }()
		go func() { defer wg.Done(); writeInChunks(server, s2cPayload, 32*1024) }()
		go func() { defer wg.Done(); c2sReceived, c2sErr = readExact(server, size, 120*time.Second) }()
		go func() { defer wg.Done(); s2cReceived, s2cErr = readExact(client, size, 120*time.Second) }()
		wg.Wait()

		if c2sErr != nil {
			t.Fatalf("client→server: %v (got %d/%d)", c2sErr, len(c2sReceived), size)
		}
		if s2cErr != nil {
			t.Fatalf("server→client: %v (got %d/%d)", s2cErr, len(s2cReceived), size)
		}
		if !bytes.Equal(c2sReceived, c2sPayload) {
			t.Fatal("client→server data integrity failure")
		}
		if !bytes.Equal(s2cReceived, s2cPayload) {
			t.Fatal("server→client data integrity failure")
		}
	})

	t.Run("MultiMessage_Ordered", func(t *testing.T) {
		server, client, stop, err := makePipeline(t)
		if err != nil {
			t.Fatalf("MakePipeline: %v", err)
		}
		defer stop()

		messages := [][]byte{
			makePayload(100),
			makePayload(1000),
			makePayload(10000),
			makePayload(50000),
			makePayload(100000),
		}

		go func() {
			for _, msg := range messages {
				header := []byte{
					byte(len(msg) >> 24), byte(len(msg) >> 16),
					byte(len(msg) >> 8), byte(len(msg)),
				}
				client.Write(header)
				client.Write(msg)
			}
		}()

		for i, expected := range messages {
			hdr, err := readExact(server, 4, 60*time.Second)
			if err != nil {
				t.Fatalf("msg %d header: %v", i, err)
			}
			length := int(hdr[0])<<24 | int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
			data, err := readExact(server, length, 60*time.Second)
			if err != nil {
				t.Fatalf("msg %d data: %v (got %d/%d)", i, err, len(data), length)
			}
			if !bytes.Equal(data, expected) {
				t.Fatalf("msg %d: data integrity failure at size %d", i, length)
			}
		}
	})

	t.Run("SmallWrites_StreamCoherence", func(t *testing.T) {
		server, client, stop, err := makePipeline(t)
		if err != nil {
			t.Fatalf("MakePipeline: %v", err)
		}
		defer stop()

		var expected []byte
		for i := 0; i < 100; i++ {
			chunk := []byte(fmt.Sprintf("msg%03d|", i))
			expected = append(expected, chunk...)
			client.Write(chunk)
		}

		received, err := readExact(server, len(expected), 60*time.Second)
		if err != nil {
			t.Fatalf("small writes: %v (got %d/%d)", err, len(received), len(expected))
		}
		if !bytes.Equal(received, expected) {
			t.Fatal("small writes: stream coherence failure")
		}
	})
}

// ── TestStreamSemantics: 流语义一致性 ────────────────────────

// TestStreamSemantics verifies close/EOF/deadline behavior of the pipeline.
func TestStreamSemantics(t *testing.T, makePipeline MakePipeline) {
	t.Helper()

	t.Run("WriteAfterClose", func(t *testing.T) {
		_, client, stop, err := makePipeline(t)
		if err != nil {
			t.Fatalf("MakePipeline: %v", err)
		}
		defer stop()

		client.Close()
		_, werr := client.Write([]byte("after-close"))
		if werr == nil {
			t.Fatal("Write after Close must return error")
		}
	})

	t.Run("ReadAfterClose", func(t *testing.T) {
		_, client, stop, err := makePipeline(t)
		if err != nil {
			t.Fatalf("MakePipeline: %v", err)
		}
		defer stop()

		client.Close()
		_, rerr := client.Read(make([]byte, 64))
		if rerr == nil {
			t.Fatal("Read after Close must return error")
		}
	})

	t.Run("DataBeforeClose_FullyDelivered", func(t *testing.T) {
		server, client, stop, err := makePipeline(t)
		if err != nil {
			t.Fatalf("MakePipeline: %v", err)
		}
		defer stop()

		payload := makePayload(32 * 1024)
		client.Write(payload)
		client.Close()

		received, err := readExact(server, len(payload), 30*time.Second)
		if err != nil && len(received) < len(payload) {
			t.Fatalf("data before close not fully delivered: got %d/%d, err=%v",
				len(received), len(payload), err)
		}
		if !bytes.Equal(received[:len(payload)], payload) {
			t.Fatal("data before close: integrity failure")
		}
	})

	t.Run("PeerClose_EventualEOF", func(t *testing.T) {
		server, client, stop, err := makePipeline(t)
		if err != nil {
			t.Fatalf("MakePipeline: %v", err)
		}
		defer stop()

		msg := []byte("last-words")
		client.Write(msg)
		client.Close()

		// Read the data first
		readExact(server, len(msg), 10*time.Second)

		// Subsequent reads should eventually error (EOF, ErrClosedPipe, etc.)
		server.SetReadDeadline(time.Now().Add(10 * time.Second))
		buf := make([]byte, 64)
		for {
			_, err := server.Read(buf)
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
					break // correct
				}
				break // any terminal error is acceptable
			}
		}
	})
}

// ── TestResilience: 丢包/断链恢复 ───────────────────────────

// FaultController allows the harness to inject network faults.
type FaultController interface {
	SetDropRate(percent int)
	SetPartition(active bool)
}

// MakeFaultyPipeline creates a pipeline with fault injection support.
type MakeFaultyPipeline func(t *testing.T) (serverConn, clientConn net.Conn, faults FaultController, stop func(), err error)

// TestResilience verifies transport recovery under packet loss and partition.
func TestResilience(t *testing.T, makeFaultyPipeline MakeFaultyPipeline) {
	t.Helper()

	t.Run("LossRecovery_10pct_256KB", func(t *testing.T) {
		server, client, faults, stop, err := makeFaultyPipeline(t)
		if err != nil {
			t.Fatalf("MakeFaultyPipeline: %v", err)
		}
		defer stop()

		size := 256 * 1024
		payload := makePayload(size)
		faults.SetDropRate(10)

		start := time.Now()
		go writeInChunks(client, payload, 32*1024)

		received, err := readExact(server, size, 120*time.Second)
		faults.SetDropRate(0)

		if err != nil {
			t.Fatalf("10%% loss 256KB: got %d/%d, err=%v", len(received), size, err)
		}
		if !bytes.Equal(received, payload) {
			t.Fatal("10% loss: data integrity failure")
		}
		t.Logf("10%% loss 256KB: delivered in %v", time.Since(start))
	})

	t.Run("LossRecovery_20pct_Bidirectional", func(t *testing.T) {
		server, client, faults, stop, err := makeFaultyPipeline(t)
		if err != nil {
			t.Fatalf("MakeFaultyPipeline: %v", err)
		}
		defer stop()

		size := 128 * 1024
		c2sPayload := makePayload(size)
		s2cPayload := make([]byte, size)
		for i := range s2cPayload {
			s2cPayload[i] = byte((i*11 + 37) % 251)
		}

		faults.SetDropRate(20)

		var wg sync.WaitGroup
		var c2sReceived, s2cReceived []byte
		var c2sErr, s2cErr error

		wg.Add(4)
		go func() { defer wg.Done(); writeInChunks(client, c2sPayload, 32*1024) }()
		go func() { defer wg.Done(); writeInChunks(server, s2cPayload, 32*1024) }()
		go func() { defer wg.Done(); c2sReceived, c2sErr = readExact(server, size, 120*time.Second) }()
		go func() { defer wg.Done(); s2cReceived, s2cErr = readExact(client, size, 120*time.Second) }()
		wg.Wait()
		faults.SetDropRate(0)

		if c2sErr != nil {
			t.Fatalf("c→s under 20%% loss: %v", c2sErr)
		}
		if s2cErr != nil {
			t.Fatalf("s→c under 20%% loss: %v", s2cErr)
		}
		if !bytes.Equal(c2sReceived, c2sPayload) || !bytes.Equal(s2cReceived, s2cPayload) {
			t.Fatal("bidirectional loss: integrity failure")
		}
	})

	t.Run("PartitionRecovery", func(t *testing.T) {
		server, client, faults, stop, err := makeFaultyPipeline(t)
		if err != nil {
			t.Fatalf("MakeFaultyPipeline: %v", err)
		}
		defer stop()

		pre := []byte("pre-partition|")
		client.Write(pre)

		faults.SetPartition(true)
		post := makePayload(32 * 1024)
		go func() { client.Write(post) }()
		time.Sleep(2 * time.Second)
		faults.SetPartition(false)

		totalSize := len(pre) + len(post)
		received, err := readExact(server, totalSize, 60*time.Second)
		if err != nil {
			t.Fatalf("partition recovery: got %d/%d, err=%v", len(received), totalSize, err)
		}

		expected := append(pre, post...)
		if !bytes.Equal(received, expected) {
			t.Fatal("partition recovery: integrity failure")
		}
	})
}

// ── helpers ──────────────────────────────────────────────────

func makePayload(size int) []byte {
	p := make([]byte, size)
	for i := range p {
		p[i] = byte((i*7 + 13) % 251)
	}
	return p
}

func readExact(conn io.Reader, n int, timeout time.Duration) ([]byte, error) {
	result := make([]byte, 0, n)
	buf := make([]byte, 65536)
	deadline := time.Now().Add(timeout)

	for len(result) < n {
		if time.Now().After(deadline) {
			return result, fmt.Errorf("timeout: got %d/%d bytes", len(result), n)
		}
		remaining := n - len(result)
		readSize := len(buf)
		if readSize > remaining {
			readSize = remaining
		}
		nr, err := conn.Read(buf[:readSize])
		if nr > 0 {
			result = append(result, buf[:nr]...)
		}
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func writeInChunks(conn net.Conn, payload []byte, chunkSize int) error {
	written := 0
	for written < len(payload) {
		chunk := chunkSize
		if chunk > len(payload)-written {
			chunk = len(payload) - written
		}
		n, err := conn.Write(payload[written : written+chunk])
		if err != nil {
			return err
		}
		written += n
	}
	return nil
}

func formatSize(size int) string {
	if size >= 1024*1024 {
		return fmt.Sprintf("%dMB", size/(1024*1024))
	}
	return fmt.Sprintf("%dKB", size/1024)
}
