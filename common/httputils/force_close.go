package httputils

import (
	"io"

	"github.com/metacubex/http"
)

type closerIdleConnections interface {
	CloseIdleConnections()
}

type closerHTTP2Connections interface {
	CloseHTTP2Connections()
}

func CloseTransport(roundTripper http.RoundTripper) {
	if tr, ok := roundTripper.(closerIdleConnections); ok {
		tr.CloseIdleConnections() // for *http.Transport
	}
	if tr, ok := roundTripper.(closerHTTP2Connections); ok {
		tr.CloseHTTP2Connections() // for *http.Transport in our own fork
	}
	if tr, ok := roundTripper.(io.Closer); ok {
		_ = tr.Close() // for *http3.Transport
	}
}
