//go:build teams

package simplex

import "testing"

func TestRegisteredTeamsSimplexResolvers(t *testing.T) {
	const scheme = "teams"
	const address = "teams://127.0.0.1:8080/rem?interval=1000&webhook=https%3A%2F%2Fexample.com%2Fhook&chat_url=https%3A%2F%2Fexample.com%2Fchat&auth_token=test-token&wrapper=raw"

	if _, err := GetSimplexClient(scheme); err != nil {
		t.Fatalf("GetSimplexClient(%q) error: %v", scheme, err)
	}
	if _, err := GetSimplexServer(scheme); err != nil {
		t.Fatalf("GetSimplexServer(%q) error: %v", scheme, err)
	}

	addr, err := ResolveSimplexAddr(scheme, address)
	if err != nil {
		t.Fatalf("ResolveSimplexAddr(%q) error: %v", scheme, err)
	}
	if got := addr.Network(); got != scheme {
		t.Fatalf("unexpected simplex addr network: got %q want %q", got, scheme)
	}
}
