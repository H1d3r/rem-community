//go:build !js || !wasm

package wasmfetch

import "fmt"

// Fetch is not supported outside of js/wasm.
func Fetch(url string, opt FetchOption) (*FetchResult, error) {
	return nil, fmt.Errorf("wasmfetch: fetch is not supported on this platform")
}
