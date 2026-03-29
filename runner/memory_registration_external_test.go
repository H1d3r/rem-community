package runner_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/chainreactors/proxyclient"
	"github.com/chainreactors/rem/protocol/core"
	runnerpkg "github.com/chainreactors/rem/runner"
)

func TestRunnerPackageRegistersMemoryTransportAndProxyClient(t *testing.T) {
	_ = runnerpkg.NewConsoleWithCMD

	if _, err := core.DialerCreate(core.MemoryTunnel, context.Background()); err != nil {
		t.Fatalf("memory dialer should be registered by importing runner: %v", err)
	}
	if _, err := core.ListenerCreate(core.MemoryTunnel, context.Background()); err != nil {
		t.Fatalf("memory listener should be registered by importing runner: %v", err)
	}

	memURL := &url.URL{Scheme: "memory", Host: "runner-memory-registration"}
	if _, err := proxyclient.NewClient(memURL); err != nil {
		t.Fatalf("memory proxyclient should be registered by importing runner: %v", err)
	}
}
