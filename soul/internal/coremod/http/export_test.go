package http

import (
	"net/http"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
)

// NewRealClient returns the module's default *http.Client (with CheckRedirect)
// for an httptest test that drives a real redirect chain: the test swaps in a
// Transport trusting the self-signed httptest cert, CheckRedirect is kept as
// is. Symmetric to url.NewRealClient. Built via New() with zero-value opts
// (the maximally-safe default), same as production.
func NewRealClient() *http.Client {
	return New().NewClient(util.HTTPClientOpts{}).(*http.Client)
}
