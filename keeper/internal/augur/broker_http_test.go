package augur

import (
	"context"
	"io"
	"net/http"
	"strings"
)

// recordingDoer — fake HTTPDoer: records the outgoing request and returns a
// canned response. Never touches the network. The broker's validateEndpoint
// runs against a valid public https endpoint; the HTTP call itself is
// intercepted here.
type recordingDoer struct {
	gotReq     *http.Request
	gotAuth    string
	respStatus int
	respBody   string
	err        error
}

func (d *recordingDoer) Do(req *http.Request) (*http.Response, error) {
	d.gotReq = req
	d.gotAuth = req.Header.Get("Authorization")
	if d.err != nil {
		return nil, d.err
	}
	status := d.respStatus
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(d.respBody)),
		Header:     make(http.Header),
	}, nil
}

// staticKV — fake KVReader: a single secret for auth_ref credential resolution.
type staticKV struct {
	data map[string]any
	err  error
}

func (k staticKV) ReadKV(_ context.Context, _ string) (map[string]any, error) {
	if k.err != nil {
		return nil, k.err
	}
	return k.data, nil
}
