//go:build !advance || tinygo

package runner

import (
	"fmt"

	"github.com/chainreactors/rem/protocol/tunnel"
)

func appendAdvanceTunnelOptions(console *Console, _ *RunnerConfig, tunOpts []tunnel.TunnelOption) ([]tunnel.TunnelOption, error) {
	if console.ConsoleURL.GetQuery("age") != "" {
		return nil, fmt.Errorf("age requires building with the advance tag")
	}
	if console.ConsoleURL.GetQuery("utls") != "" {
		return nil, fmt.Errorf("utls requires building with the advance tag")
	}
	if console.ConsoleURL.GetQuery("shadowtls") != "" || console.ConsoleURL.GetQuery("shadowtls-password") != "" {
		return nil, fmt.Errorf("shadowtls requires building with the advance tag")
	}

	// Standard TLS is always available
	if console.ConsoleURL.GetQuery("tls") != "" {
		tunOpts = append(tunOpts, tunnel.WithTLS())
	}
	if console.ConsoleURL.GetQuery("tlsintls") != "" {
		tunOpts = append(tunOpts, tunnel.WithTLSInTLS())
	}
	return tunOpts, nil
}
