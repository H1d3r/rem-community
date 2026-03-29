//go:build !tinygo

package runner

// basic
import (
	_ "github.com/chainreactors/rem/protocol/wrapper"
)

// application
import (
	_ "github.com/chainreactors/rem/protocol/serve/http"
	_ "github.com/chainreactors/rem/protocol/serve/portforward"
	_ "github.com/chainreactors/rem/protocol/serve/raw"
	_ "github.com/chainreactors/rem/protocol/serve/socks"
)

// transport
import (
	_ "github.com/chainreactors/rem/protocol/tunnel/memory"
	_ "github.com/chainreactors/rem/protocol/tunnel/simplex"
)
