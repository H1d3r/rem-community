//go:build !tinygo

package main

import (
	"net"
	"testing"
)

func TestMemoryReadEOFReturnsZeroNoError(t *testing.T) {
	const handle = 710001

	server, client := net.Pipe()
	conns.Store(handle, server)
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
		conns.Delete(handle)
	})

	if err := client.Close(); err != nil {
		t.Fatalf("close peer: %v", err)
	}

	buf := make([]byte, 16)
	n, errCode := memoryReadGo(handle, buf)
	if errCode != 0 {
		t.Fatalf("MemoryRead EOF error code = %d, want 0", errCode)
	}
	if n != 0 {
		t.Fatalf("MemoryRead EOF bytes = %d, want 0", n)
	}
}

func TestMemoryReadDataThenEOFDoesNotReportError(t *testing.T) {
	const handle = 710002
	want := []byte("abc")

	server, client := net.Pipe()
	conns.Store(handle, server)
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
		conns.Delete(handle)
	})

	go func() {
		_, _ = client.Write(want)
		_ = client.Close()
	}()

	buf := make([]byte, 16)
	n, errCode := memoryReadGo(handle, buf)
	if errCode != 0 {
		t.Fatalf("first MemoryRead error code = %d, want 0", errCode)
	}
	if got := string(buf[:int(n)]); got != string(want) {
		t.Fatalf("first MemoryRead bytes = %q, want %q", got, want)
	}

	n, errCode = memoryReadGo(handle, buf)
	if errCode != 0 {
		t.Fatalf("second MemoryRead EOF error code = %d, want 0", errCode)
	}
	if n != 0 {
		t.Fatalf("second MemoryRead EOF bytes = %d, want 0", n)
	}
}

func TestMemoryCloseRemovesConnectionHandle(t *testing.T) {
	const handle = 710003

	server, client := net.Pipe()
	conns.Store(handle, server)
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
		conns.Delete(handle)
	})

	if got := memoryCloseGo(handle); got != 0 {
		t.Fatalf("MemoryClose error code = %d, want 0", got)
	}
	if _, ok := conns.Load(handle); ok {
		t.Fatalf("connection handle %d still present after MemoryClose", handle)
	}
}
