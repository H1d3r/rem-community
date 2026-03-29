//go:build !tinygo

package main

/*
#include <stdlib.h>
#include <sys/types.h>
*/
import "C"
import (
	"context"
	"io"
	"math/rand"
	"net"
	"net/url"
	"time"
	"unsafe"

	"github.com/chainreactors/logs"
	"github.com/chainreactors/proxyclient"
	"github.com/chainreactors/rem/agent"
	"github.com/chainreactors/rem/runner"
	"github.com/chainreactors/rem/x/utils"
	"github.com/kballard/go-shellquote"
)

func init() {
	utils.Log = logs.NewLogger(100)
}

func initDialerGo() int {
	proxyclient.InitBuiltinSchemes()
	return 0
}

//export InitDialer
func InitDialer() C.int {
	return C.int(initDialerGo())
}

//export RemDial
func RemDial(cmdline *C.char) (*C.char, C.int) {
	var option runner.Options
	args, err := shellquote.Split(C.GoString(cmdline))
	if err != nil {
		return nil, ErrCmdParseFailed
	}
	err = option.ParseArgs(args)
	if err != nil {
		return nil, ErrArgsParseFailed
	}

	if option.Debug {
		utils.Log = logs.NewLogger(logs.DebugLevel)
	}
	if option.Detail {
		utils.Log.SetLevel(utils.IOLog)
	} else if option.Quiet {
		utils.Log.SetLevel(100)
	}

	r, err := option.Prepare()
	if err != nil {
		return nil, ErrPrepareFailed
	}
	if len(r.ConsoleURLs) == 0 {
		return nil, ErrNoConsoleURL
	}

	conURL := r.ConsoleURLs[0]

	console, err := runner.NewConsole(r, r.NewURLs(conURL))
	if err != nil {
		return nil, ErrCreateConsole
	}

	a, err := console.Dial(console.ConsoleURL)
	if err != nil {
		return nil, ErrDialFailed
	}

	go func() {
		err := a.Handler()
		if err != nil {
			utils.Log.Errorf("[rem] handler exited: %v", err)
		}
		a.Close(err)
		agent.Agents.Map.Delete(a.ID)
		// Do NOT reconnect here — the caller (malefic Rust) manages
		// reconnection by re-calling RemDial. Having two reconnection
		// loops (Go + Rust) causes competing agents that kill each
		// other's memory listeners via CreateChannel race.
	}()

	for {
		if a.Init {
			break
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}

	return C.CString(a.ID), 0
}

func memoryDialGo(memhandle string, dst string) (int, int) {
	memURL := &url.URL{
		Scheme: "memory",
		Host:   memhandle,
	}
	memClient, err := proxyclient.NewClient(memURL)
	if err != nil {
		return 0, ErrCreateConsole
	}

	conn, err := memClient(context.Background(), "tcp", dst)
	if err != nil {
		return 0, ErrDialFailed
	}

	connHandle := rand.Intn(0x7FFFFFFF)
	conns.Store(connHandle, conn)
	return connHandle, 0
}

//export MemoryDial
func MemoryDial(memhandle *C.char, dst *C.char) (C.int, C.int) {
	connHandle, errCode := memoryDialGo(C.GoString(memhandle), C.GoString(dst))
	return C.int(connHandle), C.int(errCode)
}

func memoryReadGo(handleInt int, dst []byte) (int, int) {
	conn, ok := conns.Load(handleInt)
	if !ok {
		return 0, ErrArgsParseFailed
	}

	buffer := make([]byte, len(dst))
	n, err := conn.(net.Conn).Read(buffer)
	if err != nil && err != io.EOF {
		return 0, ErrDialFailed
	}

	if n > 0 {
		copy(dst[:n], buffer[:n])
	}

	return n, 0
}

//export MemoryRead
func MemoryRead(chandle C.int, buf unsafe.Pointer, size C.int) (C.int, C.int) {
	cBuf := (*[1 << 30]byte)(buf)
	n, errCode := memoryReadGo(int(chandle), cBuf[:int(size)])
	return C.int(n), C.int(errCode)
}

func memoryWriteGo(handleInt int, src []byte) (int, int) {
	conn, ok := conns.Load(handleInt)
	if !ok {
		return 0, ErrArgsParseFailed
	}

	n, err := conn.(net.Conn).Write(src)
	if err != nil {
		return 0, ErrDialFailed
	}

	return n, 0
}

//export MemoryWrite
func MemoryWrite(chandle C.int, buf unsafe.Pointer, size C.int) (C.int, C.int) {
	cBuf := (*[1 << 30]byte)(buf)
	n, errCode := memoryWriteGo(int(chandle), cBuf[:int(size)])
	return C.int(n), C.int(errCode)
}

func memoryCloseGo(handleInt int) int {
	conn, ok := conns.Load(handleInt)
	if !ok {
		return ErrArgsParseFailed
	}

	err := conn.(net.Conn).Close()
	if err != nil {
		return ErrDialFailed
	}

	conns.Delete(handleInt)
	return 0
}

//export MemoryClose
func MemoryClose(chandle C.int) C.int {
	return C.int(memoryCloseGo(int(chandle)))
}

//export CleanupAgent
func CleanupAgent() {
	agent.Agents.Map.Range(func(key, value interface{}) bool {
		if a, ok := value.(*agent.Agent); ok {
			a.Close(nil)
		}
		return true
	})
}
