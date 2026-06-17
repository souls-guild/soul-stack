package augur

import (
	"context"
	"io"
	"net/http"
	"strings"
)

// recordingDoer — fake HTTPDoer: фиксирует исходящий запрос и отдаёт заготовленный
// ответ. Не ходит в сеть. validateEndpoint брокера проходит на валидном
// https-публичном endpoint-е; сам HTTP-вызов перехватывается здесь.
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

// staticKV — fake KVReader: один секрет для auth_ref-credential-резолва.
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
