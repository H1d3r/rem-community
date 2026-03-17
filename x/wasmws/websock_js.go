package wasmws

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall/js"
	"time"

	"github.com/chainreactors/logs"
)

const (
	socketStreamThresholdBytes = 1024 //If enabled, the Blob interface will be used when consecutive messages exceed this threshold
)

var (
	//EnableBlobStreaming allows the browser provided Websocket's streaming
	// interface to be used, if supported.
	EnableBlobStreaming bool = true

	//ErrWebsocketClosed is returned when operations are performed on a closed Websocket
	ErrWebsocketClosed = errors.New("WebSocket: Web socket is closed")

	blobSupported bool //set to true by init if browser supports the Blob interface
)

func newStoppedTimer() *time.Timer {
	timer := time.NewTimer(time.Hour)
	stopTimer(timer)
	return timer
}

func stopTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

// init checks to see if the browser hosting this application support the Websocket Blob interface
func init() {
	newBlob := js.Global().Get("Blob")
	if newBlob.Equal(jsUndefined) {
		blobSupported = false
		return
	}

	testBlob := js.Global().Get("Blob").New()
	blobSupported = !testBlob.Get("arrayBuffer").Equal(jsUndefined) && !testBlob.Get("stream").Equal(jsUndefined)
	logs.Log.Debugf("Websocket: Init: EnableBlobStreaming is %v and blobSupported is %v", EnableBlobStreaming, blobSupported)
}

// WebSocket is a Go struct that wraps the web browser's JavaScript websocket object and provides a net.Conn interface
type WebSocket struct {
	ctx       context.Context
	ctxCancel context.CancelFunc

	URL        string
	ws         js.Value
	wsType     socketType
	enableBlob bool
	openCh     chan struct{}

	readLock  sync.Mutex
	remaining io.Reader
	readCh    chan io.Reader

	readDeadlineTimer  *time.Timer
	readDeadlineActive bool
	newReadDeadlineCh  chan time.Time

	writeLock sync.Mutex
	errCh     chan error

	writeDeadlineTimer  *time.Timer
	writeDeadlineActive bool
	newWriteDeadlineCh  chan time.Time

	cleanup []func()
}

// New returns a new WebSocket using the provided dial context and websocket URL.
// The URL should be in the form of "ws://host/path..." for unsecured websockets
// and "wss://host/path..." for secured websockets. If tunnel a TLS based protocol
// over a "wss://..." websocket you will get TLS twice, once on the websocket using
// the browsers TLS stack and another using the Go (or other compiled) TLS stack.
func New(dialCtx context.Context, URL string) (*WebSocket, error) {
	ctx, cancel := context.WithCancel(context.Background())
	ws := &WebSocket{
		ctx:       ctx,
		ctxCancel: cancel,

		URL:        URL,
		ws:         js.Global().Get("WebSocket").New(URL),
		wsType:     socketTypeArrayBuffer,
		enableBlob: EnableBlobStreaming && blobSupported,
		openCh:     make(chan struct{}),

		readCh:             make(chan io.Reader, 8),
		readDeadlineTimer:  newStoppedTimer(),
		readDeadlineActive: false,
		newReadDeadlineCh:  make(chan time.Time, 1),

		errCh:               make(chan error, 1),
		writeDeadlineTimer:  newStoppedTimer(),
		writeDeadlineActive: false, // 默认禁用
		newWriteDeadlineCh:  make(chan time.Time, 1),

		cleanup: make([]func(), 0, 3),
	}

	ws.wsType.Set(ws.ws)
	ws.addHandler(ws.handleOpen, "open")
	ws.addHandler(ws.handleClose, "close")
	ws.addHandler(ws.handleError, "error")
	ws.addHandler(ws.handleMessage, "message")

	go func() { //handle shutdown
		<-ws.ctx.Done()
		logs.Log.Debugf("Websocket: Shutdown")

		ws.ws.Call("close")
		for _, cleanup := range ws.cleanup {
			cleanup()
		}

		for {
			select {
			case pending := <-ws.readCh:
				if closer, hasClose := pending.(io.Closer); hasClose {
					closer.Close()
				}
				continue

			default:
			}
			break
		}
	}()

	//Wait for connection or failure
	select {
	case <-ws.ctx.Done():
		return nil, ErrWebsocketClosed

	case <-dialCtx.Done():
		ws.ctxCancel()
		return nil, dialCtx.Err()

	case err := <-ws.errCh:
		ws.ctxCancel()
		return nil, err

	case <-ws.openCh:
		logs.Log.Debugf("Websocket: Connected!")
	}

	//Find out what kind of socket we are
	if ws.wsType = newSocketType(ws.ws); ws.wsType == socketTypeUnknown {
		logs.Log.Debugf("Websocket: Invalid socket type")
		ws.ctxCancel()
		return nil, fmt.Errorf("WebSocket: %q's method 'websocket.binaryType' returned %q which is an invalid socket type!", ws.URL, ws.wsType)
	}
	return ws, nil
}

// Close shuts the websocket down
func (ws *WebSocket) Close() error {
	ws.ctxCancel()
	return nil
}

// LocalAddr returns a dummy websocket address to satisfy net.Conn, see: wsAddr
func (ws *WebSocket) LocalAddr() net.Addr {
	return wsAddr(ws.URL)
}

// RemoteAddr returns a dummy websocket address to satisfy net.Conn, see: wsAddr
func (ws *WebSocket) RemoteAddr() net.Addr {
	return wsAddr(ws.URL)
}

// Write implements the standard io.Writer interface. Due to the JavaScript writes
// being internally buffered it will never block and a write timeout from a
// previous write may not surface until a subsequent write.
func (ws *WebSocket) Write(buf []byte) (n int, err error) {
	//Check for noop
	writeCount := len(buf)
	if writeCount < 1 {
		return 0, nil
	}

	//Lock
	ws.writeLock.Lock()
	defer ws.writeLock.Unlock()

	//Check for close or new deadline
	select {
	case <-ws.ctx.Done():
		return 0, ErrWebsocketClosed

	case newWriteDeadline := <-ws.newWriteDeadlineCh:
		ws.setWriteDeadline(newWriteDeadline)

	default:
	}

	//Write
	select {
	case <-ws.ctx.Done():
		return 0, ErrWebsocketClosed

	case err = <-ws.errCh:
		logs.Log.Debugf("Websocket: Write: Outstanding error %v", err)
		ws.ctxCancel()
		return 0, fmt.Errorf("WebSocket: Previous write resulted in stream error; Details: %w", err)

	case newWriteDeadline := <-ws.newWriteDeadlineCh:
		ws.setWriteDeadline(newWriteDeadline)

	case <-ws.writeDeadlineTimer.C:
		if ws.writeDeadlineActive {
			if remaining := ws.ws.Get("bufferedAmount").Int(); remaining > 0 {
				return 0, timeoutError{}
			}
		}
		// 如果deadline未激活，忽略timer事件

	default:
		jsBuf := uint8Array.New(len(buf))
		js.CopyBytesToJS(jsBuf, buf)
		ws.ws.Call("send", jsBuf)
		logs.Log.Debugf("Websocket: Write %d bytes (content: %q)", writeCount, buf)
	}

	//Check for status updates before returning
	select {
	case err = <-ws.errCh:
		ws.ctxCancel()
		return 0, fmt.Errorf("WebSocket: Write resulted in stream error; Details: %w", err)

	case <-ws.writeDeadlineTimer.C:
		if ws.writeDeadlineActive {
			if remaining := ws.ws.Get("bufferedAmount").Int(); remaining > 0 {
				return 0, timeoutError{}
			}
		}
		// 如果deadline未激活，忽略timer事件

	default:
	}
	return writeCount, nil
}

// Read implements the standard io.Reader interface (typical semantics)
func (ws *WebSocket) Read(buf []byte) (int, error) {
	//Check for noop
	if len(buf) < 1 {
		return 0, nil
	}
	defer logs.Log.Debugf("Websocket: Read done")
	//Lock
	ws.readLock.Lock()
	defer ws.readLock.Unlock()

	//Check for close or new deadline
	select {
	case <-ws.ctx.Done():
		return 0, ErrWebsocketClosed

	case newReadDeadline := <-ws.newReadDeadlineCh:
		ws.setReadDeadline(newReadDeadline)

	default:
	}

	for {
		//Get next chunk
		if ws.remaining == nil {
			logs.Log.Debugf("Websocket: Read wait on queue-")
			select {
			case ws.remaining = <-ws.readCh:
				logs.Log.Debugf("Websocket: Received new reader from queue")

			case <-ws.ctx.Done():
				return 0, ErrWebsocketClosed

			case newReadDeadline := <-ws.newReadDeadlineCh:
				logs.Log.Debugf("Websocket: Set new read deadline (during read)")
				ws.setReadDeadline(newReadDeadline)
				continue

			case <-ws.readDeadlineTimer.C:
				if ws.readDeadlineActive {
					logs.Log.Debugf("Websocket: Read timeout")
					return 0, timeoutError{}
				}
				logs.Log.Debugf("Websocket: Ignoring inactive read deadline timer event")
				continue
			}
			if ws.remaining == nil {
				logs.Log.Debugf("Websocket: Reader queue yielded nil, retrying")
				continue
			}
		}

		//Read from chunk
		logs.Log.Debugf("Websocket: Reading")
		n, err := ws.remaining.Read(buf)
		if err == io.EOF {
			if closer, hasClose := ws.remaining.(io.Closer); hasClose {
				closer.Close()
			}
			ws.remaining, err = nil, nil
			if n < 1 {
				continue
			}
		}
		logs.Log.Debugf("Websocket: Read %d bytes (content: %q)", n, buf[:n])
		return n, err
	}
}

func (ws *WebSocket) SetDeadline(future time.Time) (err error) {
	select {
	case ws.newWriteDeadlineCh <- future:
		ws.newReadDeadlineCh <- future
	case ws.newReadDeadlineCh <- future:
		ws.newWriteDeadlineCh <- future
	}
	return nil
}

// SetWriteDeadline implements the Conn SetWriteDeadline method
func (ws *WebSocket) SetWriteDeadline(future time.Time) error {
	logs.Log.Debugf("Websocket: Set write deadline for %s", future.String())
	ws.newWriteDeadlineCh <- future
	return nil
}

// SetReadDeadline implements the Conn SetReadDeadline method
func (ws *WebSocket) SetReadDeadline(future time.Time) error {
	logs.Log.Debugf("Websocket: Set read deadline for %s", future.String())
	ws.newReadDeadlineCh <- future
	return nil
}

// setWriteDeadline 内部方法，用于设置写入deadline
func (ws *WebSocket) setWriteDeadline(future time.Time) {
	if future.IsZero() {
		// 禁用deadline
		ws.writeDeadlineActive = false
		stopTimer(ws.writeDeadlineTimer)
		logs.Log.Debugf("Websocket: Write deadline disabled")
	} else {
		// 启用deadline
		ws.writeDeadlineActive = true
		stopTimer(ws.writeDeadlineTimer)
		ws.writeDeadlineTimer.Reset(time.Until(future))
		logs.Log.Debugf("Websocket: Write deadline set to %s", future.String())
	}
}

// setReadDeadline 内部方法，用于设置读取deadline
func (ws *WebSocket) setReadDeadline(future time.Time) {
	if future.IsZero() {
		// 禁用deadline
		ws.readDeadlineActive = false
		stopTimer(ws.readDeadlineTimer)
		logs.Log.Debugf("Websocket: Read deadline disabled")
	} else {
		// 启用deadline
		ws.readDeadlineActive = true
		stopTimer(ws.readDeadlineTimer)
		ws.readDeadlineTimer.Reset(time.Until(future))
		logs.Log.Debugf("Websocket: Read deadline set to %s", future.String())
	}
}

// addHandler is used internall by the WebSocket constructor
func (ws *WebSocket) addHandler(handler func(this js.Value, args []js.Value), event string) {
	jsHandler := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		handler(this, args)
		return nil
	})
	cleanup := func() {
		ws.ws.Call("removeEventListener", event, jsHandler)
		jsHandler.Release()
	}
	ws.ws.Call("addEventListener", event, jsHandler)
	ws.cleanup = append(ws.cleanup, cleanup)
}

// handleOpen is a callback for JavaScript to notify Go when the websocket is open:
// See: https://developer.mozilla.org/en-US/docs/Web/API/WebSocket/onopen
func (ws *WebSocket) handleOpen(_ js.Value, _ []js.Value) {
	logs.Log.Debugf("Websocket: Open JS callback!")
	close(ws.openCh)
}

// handleClose is a callback for JavaScript to notify Go when the websocket is closed:
// See: https://developer.mozilla.org/en-US/docs/Web/API/WebSocket/onclose
func (ws *WebSocket) handleClose(_ js.Value, _ []js.Value) {
	logs.Log.Debugf("Websocket: Close JS callback!")
	ws.ctxCancel()
}

// handleError is a callback for JavaScript to notify Go when the websocket is in an error state:
// See: https://developer.mozilla.org/en-US/docs/Web/API/WebSocket/onerror
func (ws *WebSocket) handleError(_ js.Value, args []js.Value) {
	logs.Log.Debugf("Websocket: Error JS Callback")
	errMsg := "Unknown error"
	if len(args) > 0 {
		errMsg = args[0].String()
	}

	select {
	case ws.errCh <- errors.New(errMsg):
	default:
	}
}

// handleMessage is a callback for JavaScript to notify Go when the websocket has a new message:
// See: https://developer.mozilla.org/en-US/docs/Web/API/WebSocket/onmessage
func (ws *WebSocket) handleMessage(_ js.Value, args []js.Value) {
	logs.Log.Debugf("Websocket: New Message JS Callback")

	// JS→Go边界，必须recover防止WASM runtime崩溃
	defer func() {
		if r := recover(); r != nil {
			logs.Log.Errorf("Websocket: handleMessage panic: %v", r)
		}
	}()

	select {
	case <-ws.ctx.Done():
		logs.Log.Debugf("Websocket: Context done, ignoring message")
		return
	default:
	}

	var rdr io.Reader
	var size int

	// 检测数据类型
	data := args[0].Get("data")
	dataType := data.Type()
	logs.Log.Debugf("Websocket: Processing message, data type = %v", dataType)

	// 根据实际数据类型处理，而不是依赖ws.wsType
	switch dataType {
	case js.TypeObject:
		// 可能是ArrayBuffer或Blob，需要进一步检查
		if data.Get("constructor").Get("name").String() == "Blob" {
			// Blob数据
			logs.Log.Debugf("Websocket: Creating Blob reader")
			if size = data.Get("size").Int(); size <= socketStreamThresholdBytes {
				rdr = newReaderArrayPromise(data.Call("arrayBuffer"))
				//switch to ArrayBuffers for next read
				ws.wsType = socketTypeArrayBuffer
				ws.wsType.Set(ws.ws)
			} else {
				rdr = newStreamReaderPromise(data.Call("stream").Call("getReader"))
			}
			logs.Log.Debugf("Websocket: Blob reader created, size = %d", size)
		} else {
			// ArrayBuffer数据
			logs.Log.Debugf("Websocket: Creating ArrayBuffer reader")
			rdr, size = newReaderArrayBuffer(data)
			logs.Log.Debugf("Websocket: ArrayBuffer reader created, size = %d", size)

			//Should we switch to blobs for next time?
			if ws.enableBlob && size > socketStreamThresholdBytes {
				ws.wsType = socketTypeBlob
				ws.wsType.Set(ws.ws)
			}
		}

	case js.TypeString:
		// 字符串数据
		logs.Log.Debugf("Websocket: Creating String reader")
		rdr, size = newReaderString(data)
		logs.Log.Debugf("Websocket: String reader created, size = %d", size)

	default:
		logs.Log.Debugf("Websocket: Unknown data type: %v, falling back to string", dataType)
		// 作为fallback，尝试处理为字符串
		rdr, size = newReaderString(data)
	}

	logs.Log.Debugf("Websocket: JS read callback sync enqueue")

	select {
	case ws.readCh <- rdr: //Try non-blocking queue first...
		logs.Log.Debugf("Websocket: JS read callback sync enqueue success")

	case <-ws.ctx.Done():
		logs.Log.Debugf("Websocket: Context done during enqueue")

	default:
		go func() { //Don't block in a callback!
			select {
			case ws.readCh <- rdr:
				logs.Log.Debugf("Websocket: JS read callback async enqueue success")

			case <-ws.ctx.Done():
				logs.Log.Debugf("Websocket: Context done during async enqueue")
			}
		}()
	}
}
