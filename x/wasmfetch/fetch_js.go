//go:build js && wasm

package wasmfetch

import (
	"fmt"
	"net/http"
	"syscall/js"
)

// Fetch calls the JS fetch() API and blocks until the response is available.
func Fetch(url string, opt FetchOption) (*FetchResult, error) {
	jsOpts := make(map[string]interface{})

	method := opt.Method
	if method == "" {
		method = "GET"
	}
	jsOpts["method"] = method

	if opt.Credentials != "" {
		jsOpts["credentials"] = opt.Credentials
	}

	if len(opt.Headers) > 0 {
		jsHeaders := js.Global().Get("Headers").New()
		for key, values := range opt.Headers {
			for _, v := range values {
				jsHeaders.Call("append", key, v)
			}
		}
		jsOpts["headers"] = jsHeaders
	}

	if len(opt.Body) > 0 && method != http.MethodGet && method != http.MethodHead {
		buf := js.Global().Get("Uint8Array").New(len(opt.Body))
		js.CopyBytesToJS(buf, opt.Body)
		jsOpts["body"] = buf
	}

	resultCh := make(chan *FetchResult, 1)
	errCh := make(chan error, 1)

	// All js.Func must be released after the select returns.
	var thenCb, catchCb js.Func
	var abThen, abCatch js.Func
	abAllocated := false

	cleanup := func() {
		thenCb.Release()
		catchCb.Release()
		if abAllocated {
			abThen.Release()
			abCatch.Release()
		}
	}

	thenCb = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		response := args[0]

		statusCode := response.Get("status").Int()
		statusText := response.Get("statusText").String()

		respHeaders := make(http.Header)
		headersForEachCb := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
			value := args[0].String()
			key := args[1].String()
			respHeaders.Add(key, value)
			return nil
		})
		response.Get("headers").Call("forEach", headersForEachCb)
		headersForEachCb.Release()

		abPromise := response.Call("arrayBuffer")
		abAllocated = true
		abThen = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
			arrayBuf := args[0]
			bodyBuf := js.Global().Get("Uint8Array").New(arrayBuf)
			body := make([]byte, bodyBuf.Get("length").Int())
			js.CopyBytesToGo(body, bodyBuf)

			resultCh <- &FetchResult{
				StatusCode: statusCode,
				Status:     statusText,
				Headers:    respHeaders,
				Body:       body,
			}
			return nil
		})
		abCatch = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
			errCh <- fmt.Errorf("fetch body read error: %s", args[0].Call("toString").String())
			return nil
		})
		abPromise.Call("then", abThen).Call("catch", abCatch)
		return nil
	})

	catchCb = js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		errCh <- fmt.Errorf("fetch error: %s", args[0].Call("toString").String())
		return nil
	})

	fetchPromise := js.Global().Call("fetch", url, jsOpts)
	fetchPromise.Call("then", thenCb).Call("catch", catchCb)

	defer cleanup()

	select {
	case result := <-resultCh:
		return result, nil
	case err := <-errCh:
		return nil, err
	}
}
