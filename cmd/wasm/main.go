//go:build js && wasm

package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"syscall/js"
	"time"

	"github.com/chainreactors/logs"
	"github.com/chainreactors/rem/agent"
	_ "github.com/chainreactors/rem/protocol/tunnel/wasm"
	"github.com/chainreactors/rem/runner"
	"github.com/chainreactors/rem/x/utils"
	"github.com/kballard/go-shellquote"
)

const (
	ErrCmdParseFailed  = 1
	ErrArgsParseFailed = 2
	ErrPrepareFailed   = 3
	ErrNoConsoleURL    = 4
	ErrCreateConsole   = 5
	ErrDialFailed      = 6

	agentInitTimeout = 15 * time.Second
)

var (
	agents       sync.Map
	runtimeReady atomic.Bool

	eventSinkMu  sync.RWMutex
	eventSink    js.Value
	eventSinkSet bool
)

func init() {
	utils.Log = logs.NewLogger(100)
	logs.Log = utils.Log
}

func main() {
	js.Global().Set("RemRuntimeReady", false)
	js.Global().Set("base64", encodeWrapper())
	js.Global().Set("MyGoFunc", MyGoFunc())
	js.Global().Set("MyGoFuncStream", MyGoFuncStream())
	js.Global().Set("RemSetEventSink", RemSetEventSinkWrapper())
	js.Global().Set("RemDial", RemDialWrapper())
	js.Global().Set("RemClose", RemCloseWrapper())

	runtimeReady.Store(true)
	js.Global().Set("RemRuntimeReady", true)
	emitRuntimeEvent("ready", "", 0, "runtime ready")

	<-make(chan bool)
}

func encodeWrapper() js.Func {
	return js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if len(args) == 0 {
			return wrap("", "not enough arguments")
		}
		input := args[0].String()
		return wrap(base64.StdEncoding.EncodeToString([]byte(input)), "")
	})
}

func wrap(encoded string, err string) map[string]interface{} {
	return map[string]interface{}{
		"error":   err,
		"encoded": encoded,
	}
}

func MyGoFunc() js.Func {
	return js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		requestURL := args[0].String()
		handler := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
			resolve := args[0]
			reject := args[1]
			go func() {
				res, err := http.DefaultClient.Get(requestURL)
				if err != nil {
					reject.Invoke(js.Global().Get("Error").New(err.Error()))
					return
				}
				defer res.Body.Close()

				data, err := io.ReadAll(res.Body)
				if err != nil {
					reject.Invoke(js.Global().Get("Error").New(err.Error()))
					return
				}

				dataJS := js.Global().Get("Uint8Array").New(len(data))
				js.CopyBytesToJS(dataJS, data)

				response := js.Global().Get("Response").New(dataJS)
				resolve.Invoke(response)
			}()
			return nil
		})
		return js.Global().Get("Promise").New(handler)
	})
}

func MyGoFuncStream() js.Func {
	return js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		requestURL := args[0].String()
		handler := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
			resolve := args[0]
			reject := args[1]
			go func() {
				res, err := http.DefaultClient.Get(requestURL)
				if err != nil {
					reject.Invoke(js.Global().Get("Error").New(err.Error()))
					return
				}
				underlyingSource := map[string]interface{}{
					"start": js.FuncOf(func(this js.Value, args []js.Value) interface{} {
						controller := args[0]
						go func() {
							defer res.Body.Close()
							for {
								buf := make([]byte, 16384)
								n, err := res.Body.Read(buf)
								if err != nil && err != io.EOF {
									controller.Call("error", js.Global().Get("Error").New(err.Error()))
									return
								}
								if n > 0 {
									dataJS := js.Global().Get("Uint8Array").New(n)
									js.CopyBytesToJS(dataJS, buf[:n])
									controller.Call("enqueue", dataJS)
								}
								if err == io.EOF {
									controller.Call("close")
									return
								}
							}
						}()
						return nil
					}),
					"cancel": js.FuncOf(func(this js.Value, args []js.Value) interface{} {
						res.Body.Close()
						return nil
					}),
				}

				readableStream := js.Global().Get("ReadableStream").New(underlyingSource)
				responseInit := map[string]interface{}{
					"status":     http.StatusOK,
					"statusText": http.StatusText(http.StatusOK),
				}
				response := js.Global().Get("Response").New(readableStream, responseInit)
				resolve.Invoke(response)
			}()
			return nil
		})
		return js.Global().Get("Promise").New(handler)
	})
}

func RemSetEventSinkWrapper() js.Func {
	return js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if len(args) == 0 || args[0].Type() != js.TypeFunction {
			clearEventSink()
			return false
		}

		setEventSink(args[0])
		if runtimeReady.Load() {
			emitRuntimeEvent("ready", "", 0, "runtime ready")
		}
		return true
	})
}

func RemDialWrapper() js.Func {
	return js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if len(args) == 0 {
			reason := "missing command line"
			emitRuntimeEvent("error", "", ErrCmdParseFailed, reason)
			return map[string]interface{}{
				"error":   reason,
				"success": false,
				"code":    ErrCmdParseFailed,
			}
		}

		cmdline := args[0].String()
		handler := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
			resolve := args[0]
			reject := args[1]

			go func() {
				result := runRemDial(cmdline)
				if result.Code != 0 {
					reject.Invoke(js.Global().Get("Error").New(result.Message))
					return
				}

				resolve.Invoke(map[string]interface{}{
					"success": true,
					"agentID": result.AgentID,
					"message": result.Message,
				})
			}()

			return nil
		})

		return js.Global().Get("Promise").New(handler)
	})
}

func RemCloseWrapper() js.Func {
	return js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if len(args) == 0 {
			return map[string]interface{}{
				"error":   "missing agent ID",
				"success": false,
			}
		}

		agentID := args[0].String()
		emitRuntimeEvent("closing", agentID, 0, "closing agent")

		if agentValue, ok := agents.Load(agentID); ok {
			if a, ok := agentValue.(*agent.Agent); ok {
				a.Close(fmt.Errorf("closed by user"))
				agents.Delete(agentID)
				emitRuntimeEvent("closed", agentID, 0, "agent closed")
				return map[string]interface{}{
					"success": true,
					"message": "agent closed successfully",
				}
			}
		}

		return map[string]interface{}{
			"error":   "agent not found",
			"success": false,
		}
	})
}

type dialResult struct {
	AgentID string
	Code    int
	Message string
}

func runRemDial(cmdline string) dialResult {
	emitRuntimeEvent("connecting", "", 0, "connecting")

	var option runner.Options
	args, err := shellquote.Split(cmdline)
	if err != nil {
		return dialFailure(ErrCmdParseFailed, "failed to parse command line: %v", err)
	}

	err = option.ParseArgs(args)
	if err != nil {
		return dialFailure(ErrArgsParseFailed, "failed to parse args: %v", err)
	}

	if option.Debug {
		utils.Log = logs.NewLogger(logs.DebugLevel)
		logs.Log = utils.Log
	}
	if option.Detail {
		utils.Log.SetLevel(utils.IOLog)
		logs.Log = utils.Log
	} else if option.Quiet {
		utils.Log.SetLevel(100)
		logs.Log = utils.Log
	}

	r, err := option.Prepare()
	if err != nil {
		return dialFailure(ErrPrepareFailed, "failed to prepare options: %v", err)
	}
	if len(r.ConsoleURLs) == 0 {
		return dialFailure(ErrNoConsoleURL, "no console URL provided")
	}

	conURL := r.ConsoleURLs[0]
	console, err := runner.NewConsole(r, r.NewURLs(conURL))
	if err != nil {
		return dialFailure(ErrCreateConsole, "failed to create console: %v", err)
	}

	a, err := console.Dial(console.ConsoleURL)
	if err != nil {
		return dialFailure(ErrDialFailed, "failed to dial console: %v", err)
	}

	handlerErrCh := make(chan error, 1)
	go func() {
		handlerErrCh <- a.Handler()
	}()

	deadline := time.NewTimer(agentInitTimeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()

	for !a.Init {
		select {
		case err = <-handlerErrCh:
			if err != nil {
				return dialFailure(ErrDialFailed, "agent initialization failed: %v", err)
			}
			return dialFailure(ErrDialFailed, "agent exited before initialization completed")
		case <-deadline.C:
			a.Close(fmt.Errorf("timeout waiting for agent initialization"))
			return dialFailure(ErrDialFailed, "timeout waiting for agent initialization")
		case <-ticker.C:
		}
	}

	agents.Store(a.ID, a)
	emitRuntimeEvent("connected", a.ID, 0, "connected to rem server successfully")

	go watchAgent(a, handlerErrCh)

	return dialResult{
		AgentID: a.ID,
		Message: "Connected to rem server successfully",
	}
}

func watchAgent(a *agent.Agent, handlerErrCh <-chan error) {
	err := <-handlerErrCh
	agents.Delete(a.ID)
	if err != nil {
		emitRuntimeEvent("error", a.ID, 0, err.Error())
		return
	}
	emitRuntimeEvent("closed", a.ID, 0, "agent disconnected")
}

func dialFailure(code int, format string, args ...interface{}) dialResult {
	message := fmt.Sprintf(format, args...)
	emitRuntimeEvent("error", "", code, message)
	return dialResult{
		Code:    code,
		Message: message,
	}
}

func setEventSink(sink js.Value) {
	eventSinkMu.Lock()
	defer eventSinkMu.Unlock()
	eventSink = sink
	eventSinkSet = true
}

func clearEventSink() {
	eventSinkMu.Lock()
	defer eventSinkMu.Unlock()
	eventSink = js.Value{}
	eventSinkSet = false
}

func emitRuntimeEvent(eventType, agentID string, code int, message string) {
	payload := map[string]interface{}{
		"type": eventType,
	}
	if agentID != "" {
		payload["agentID"] = agentID
	}
	if code != 0 {
		payload["code"] = code
	}
	if message != "" {
		payload["message"] = message
	}

	eventSinkMu.RLock()
	sink := eventSink
	ok := eventSinkSet
	eventSinkMu.RUnlock()
	if !ok || sink.Type() != js.TypeFunction {
		return
	}

	sink.Invoke(payload)
}
