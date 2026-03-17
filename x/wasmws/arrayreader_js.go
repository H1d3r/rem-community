package wasmws

import (
	"errors"
	"io"
	"sync"
	"syscall/js"

	"github.com/chainreactors/logs"
)

//arrayReader is an io.ReadCloser implementation for Javascript ArrayBuffers
// See: https://developer.mozilla.org/en-US/docs/Web/API/Body/arrayBuffer
type arrayReader struct {
	jsPromise js.Value
	remaining []byte

	read bool
	err  error
}

var arrayReaderPool = sync.Pool{
	New: func() interface{} {
		return new(arrayReader)
	},
}

//newReaderArrayPromise returns a arrayReader from a JavaScript promise for
// an array buffer: See https://developer.mozilla.org/en-US/docs/Web/API/Blob/arrayBuffer
func newReaderArrayPromise(arrayPromise js.Value) *arrayReader {
	ar := arrayReaderPool.Get().(*arrayReader)
	ar.jsPromise = arrayPromise
	return ar
}

//newReaderArrayPromise returns a arrayReader from a JavaScript array buffer:
// See: https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Global_Objects/ArrayBuffer
func newReaderArrayBuffer(arrayBuffer js.Value) (*arrayReader, int) {
	ar := arrayReaderPool.Get().(*arrayReader)
	ar.remaining, ar.read = ar.fromArray(arrayBuffer), true

	logs.Log.Debugf("ArrayReader: newReaderArrayBuffer created with %d bytes", len(ar.remaining))

	return ar, len(ar.remaining)
}

//newReaderString returns a arrayReader from a JavaScript string
func newReaderString(str js.Value) (*arrayReader, int) {
	ar := arrayReaderPool.Get().(*arrayReader)
	ar.remaining, ar.read = ar.fromString(str), true

	logs.Log.Debugf("ArrayReader: newReaderString created with %d bytes", len(ar.remaining))

	return ar, len(ar.remaining)
}

//Close closes the arrayReader and returns it to a pool. DO NOT USE FURTHER!
func (ar *arrayReader) Close() error {
	ar.Reset()
	arrayReaderPool.Put(ar)
	return nil
}

//Reset makes this arrayReader ready for reuse
func (ar *arrayReader) Reset() {
	const bufMax = socketStreamThresholdBytes
	ar.jsPromise, ar.read, ar.err = js.Value{}, false, nil
	if cap(ar.remaining) < bufMax {
		ar.remaining = ar.remaining[:0]
	} else {
		ar.remaining = nil
	}
}

//Read implements the standard io.Reader interface
func (ar *arrayReader) Read(buf []byte) (n int, err error) {
	logs.Log.Debugf("ArrayReader: Read called with buffer size %d", len(buf))
	logs.Log.Debugf("ArrayReader: ar.read = %v, remaining bytes = %d", ar.read, len(ar.remaining))

	if ar.err != nil {
		logs.Log.Debugf("ArrayReader: Returning previous error: %v", ar.err)
		return 0, ar.err
	}

	if !ar.read {
		logs.Log.Debugf("ArrayReader: First read, waiting for Promise")
		ar.read = true
		readCh, errCh := make(chan []byte, 1), make(chan error, 1)

		successCallback := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
			logs.Log.Debugf("ArrayReader: Promise success callback")
			readCh <- ar.fromArray(args[0])
			return nil
		})
		defer successCallback.Release()

		failureCallback := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
			logs.Log.Debugf("ArrayReader: Promise failure callback")
			errCh <- errors.New(args[0].Get("message").String()) //Send TypeError
			return nil
		})
		defer failureCallback.Release()

		//Wait for callback
		logs.Log.Debugf("ArrayReader: Calling Promise.then()")
		ar.jsPromise.Call("then", successCallback, failureCallback)

		logs.Log.Debugf("ArrayReader: Waiting for Promise result...")
		select {
		case ar.remaining = <-readCh:
			logs.Log.Debugf("ArrayReader: Promise resolved with %d bytes", len(ar.remaining))
		case err := <-errCh:
			logs.Log.Debugf("ArrayReader: Promise rejected with error: %v", err)
			return 0, err
		}
	}

	if len(ar.remaining) < 1 {
		logs.Log.Debugf("ArrayReader: No remaining data, returning EOF")
		return 0, io.EOF
	}

	n = copy(buf, ar.remaining)
	ar.remaining = ar.remaining[n:]

	logs.Log.Debugf("ArrayReader: Read %d bytes, remaining: %d", n, len(ar.remaining))

	return n, nil
}

//fromArray is a helper that that copies a JavaScript ArrayBuffer into go-space
// and uses an existing go buffer if possible.
func (ar *arrayReader) fromArray(arrayBuffer js.Value) []byte {
	logs.Log.Debugf("ArrayReader: fromArray called")

	if arrayBuffer.IsUndefined() || arrayBuffer.IsNull() {
		logs.Log.Debugf("ArrayReader: ArrayBuffer is undefined or null")
		return []byte{}
	}

	jsBuf := uint8Array.New(arrayBuffer)
	count := jsBuf.Get("byteLength").Int()

	logs.Log.Debugf("ArrayReader: ArrayBuffer has %d bytes", count)

	if count <= 0 {
		logs.Log.Debugf("ArrayReader: ArrayBuffer is empty")
		return []byte{}
	}

	var goBuf []byte
	if count <= cap(ar.remaining) {
		goBuf = ar.remaining[:count]
		logs.Log.Debugf("ArrayReader: Reusing existing buffer")
	} else {
		goBuf = make([]byte, count)
		logs.Log.Debugf("ArrayReader: Creating new buffer")
	}

	js.CopyBytesToGo(goBuf, jsBuf)

	logs.Log.Debugf("ArrayReader: Copied %d bytes from JS to Go", count)

	return goBuf
}

//fromString is a helper that converts a JavaScript string into go-space bytes
func (ar *arrayReader) fromString(str js.Value) []byte {
	logs.Log.Debugf("ArrayReader: fromString called")

	if str.IsUndefined() || str.IsNull() {
		logs.Log.Debugf("ArrayReader: String is undefined or null")
		return []byte{}
	}

	strValue := str.String()
	logs.Log.Debugf("ArrayReader: String content: %q", strValue)

	// 直接转换字符串为字节数组
	goBuf := []byte(strValue)

	logs.Log.Debugf("ArrayReader: Converted string to %d bytes", len(goBuf))

	return goBuf
}
