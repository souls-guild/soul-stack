package api

// Wire contract of the shared label body, [ADR-0085] / NIM-728. One field, and
// its two properties are BOTH deliberate and neither is huma's default:
//
//   - OPTIONAL — `{}` is a valid body and clears the caption. huma marks a field
//     required unless told otherwise, and the default would have made an empty
//     body a 422 on REST while the MCP twin went on accepting an omitted `label`
//     — the two primary operator surfaces (ADR-004) disagreeing about what
//     clearing a caption looks like.
//   - NULLABLE — `{"label": null}` is the explicit spelling of "clear it", and
//     the one the MCP schema accepts. Reaching optionality through
//     `json:",omitempty"` instead of `required:"false"` would have dropped the
//     `"null"` arm of the generated type and made this body a schema violation.
//
// Both spellings are exercised here rather than asserted in the spec alone,
// because the spec is derived: a wrong tag produces a spec that agrees with
// itself and refuses a caller.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLabelSetRequest_ClearingSpellings drives the real route with each way of
// saying "no caption" and pins that both are accepted.
func TestLabelSetRequest_ClearingSpellings(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"explicit null", `{"label":null}`},
		{"empty object", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := &hHeraldPool{heraldUpdateRows: 1}
			r := humaHeraldRouter(t, strictAllowAll{}, nil, pool)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/v1/heralds/ops-webhook/label", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s\n"+
					"ADR-0085: `%s` must clear the caption. A 422 here means the generated schema "+
					"disagrees with the documented contract and with the MCP twin.",
					rec.Code, rec.Body.String(), tc.body)
			}
			// What the 200 above pins is the CONTRACT: both spellings pass schema
			// validation and reach the handler. That the write then stores NULL is
			// the domain's business and is pinned there (registrylabel.Normalize and
			// each registry's UpdateLabel) — the fixture pool replays one fixed row,
			// so the reply's caption says nothing about what was written.
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal reply: %v", err)
			}
			if _, present := got["label"]; present {
				t.Errorf("the reply carried `label` = %v for a row whose caption is NULL; "+
					"an absent caption must be an absent key, or a consumer cannot tell "+
					"\"no caption\" from an empty one", got["label"])
			}
			if got["id"] != "ops-webhook" {
				t.Errorf("reply `id` = %v, want the identifier — a label-set must not touch it", got["id"])
			}
		})
	}
}

// TestLabelSetRequest_SetsAFreeTextCaption pins the other direction: capitals,
// spaces and punctuation go through untouched, because the narrow grammar
// belongs to the identifier and not to the caption.
func TestLabelSetRequest_SetsAFreeTextCaption(t *testing.T) {
	const caption = "Ops — Billing Webhook (prod)"

	pool := &hHeraldPool{heraldUpdateRows: 1}
	r := humaHeraldRouter(t, strictAllowAll{}, nil, pool)
	rec := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]any{"label": caption})
	req := httptest.NewRequest(http.MethodPut, "/v1/heralds/ops-webhook/label", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// The fixture pool replays a fixed row, so the reply's caption is whatever
	// heraldScanRow carries rather than what was sent; what this pins is that a
	// caption with capitals, spaces and an em-dash is ACCEPTED — no pattern, no
	// length bound, nothing to fail.
	if strings.Contains(rec.Body.String(), "validation") {
		t.Errorf("a free-text caption was rejected: %s", rec.Body.String())
	}
}

// TestLabelSetRequest_UnknownFieldRejected keeps the body closed: the endpoint
// takes one field, and a caller who misspells it must hear so rather than have
// the write silently succeed as a clear.
func TestLabelSetRequest_UnknownFieldRejected(t *testing.T) {
	pool := &hHeraldPool{heraldUpdateRows: 1}
	r := humaHeraldRouter(t, strictAllowAll{}, nil, pool)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/heralds/ops-webhook/label",
		strings.NewReader(`{"labell":"typo"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Errorf("a misspelled field was accepted (status 200) and would have silently CLEARED "+
			"the caption; body=%s", rec.Body.String())
	}
}
