package wasmfetch

import "net/http"

// FetchOption configures a fetch request.
type FetchOption struct {
	Method      string
	Headers     http.Header
	Body        []byte
	Credentials string // "include" | "same-origin" | "omit"
}

// FetchResult contains the fetch response.
type FetchResult struct {
	StatusCode int
	Status     string
	Headers    http.Header
	Body       []byte
}
