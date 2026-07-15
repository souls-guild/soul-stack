package url

import (
	"net/http"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
)

// CheckRedirect re-exports util.CheckRedirect for testing package url in
// isolation: fake HTTPDoer injection bypasses the real client's
// CheckRedirect, so downgrade protection is verified either here or with a
// real httptest client. The direct util-level test lives in the util package.
var CheckRedirect = util.CheckRedirect

// MaxRedirects re-exports the redirect limit (moved to util after the
// HTTPDoer→util refactor) for the CheckRedirect test in package url.
const MaxRedirects = util.MaxRedirects

// NewRealClient returns the module's default *http.Client (with
// CheckRedirect) for an httptest run through a real redirect chain. The
// test's Transport is swapped to trust the self-signed httptest cert, while
// CheckRedirect is preserved. Built via the default New().NewClient factory
// with zero flags (the most restrictive client).
func NewRealClient() *http.Client {
	return New().NewClient(util.HTTPClientOpts{}).(*http.Client)
}
