package trusttunnel

import (
	"github.com/metacubex/mihomo/common/httputils"

	"github.com/metacubex/http"
)

func forceCloseAllConnections(roundTripper http.RoundTripper) {
	httputils.CloseTransport(roundTripper)
}
