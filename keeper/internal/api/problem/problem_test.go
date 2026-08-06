package problem

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestNew_FillsTitleAndStatus(t *testing.T) {
	p := New(TypeUnauthenticated, "/v1/foo", "missing Authorization")
	if p.Title != "Authentication required" {
		t.Errorf("Title = %q", p.Title)
	}
	if p.Status != 401 {
		t.Errorf("Status = %d, want 401", p.Status)
	}
	if p.Instance != "/v1/foo" {
		t.Errorf("Instance = %q", p.Instance)
	}
	if p.Detail != "missing Authorization" {
		t.Errorf("Detail = %q", p.Detail)
	}
	if p.Type != TypeUnauthenticated {
		t.Errorf("Type = %q", p.Type)
	}
}

func TestWrite_SetsContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, New(TypeNotFound, "/v1/incarnations/x", "no such incarnation"))

	if got, want := rec.Header().Get("Content-Type"), ContentType; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if rec.Code != 404 {
		t.Errorf("Code = %d, want 404", rec.Code)
	}

	var p Details
	if err := json.NewDecoder(rec.Body).Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Type != TypeNotFound || p.Status != 404 {
		t.Errorf("body shape: %+v", p)
	}
	// JSON must not start with a BOM (0xEF 0xBB 0xBF).
	body := rec.Body.Bytes()
	if len(body) >= 3 && body[0] == 0xEF && body[1] == 0xBB && body[2] == 0xBF {
		t.Errorf("body has UTF-8 BOM")
	}
}

func TestWrite_AllKnownTypes(t *testing.T) {
	cases := []struct {
		t      string
		status int
	}{
		{TypeUnauthenticated, 401},
		{TypeForbidden, 403},
		{TypeNotFound, 404},
		{TypeMalformedRequest, 400},
		{TypeValidationFailed, 422},
		{TypeInternalError, 500},
		{TypeOperatorExists, 409},
		{TypeOperatorRevoked, 409},
		{TypeWouldLockOutCluster, 409},
	}
	for _, c := range cases {
		t.Run(c.t, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Write(rec, New(c.t, "/v1/x", ""))
			if rec.Code != c.status {
				t.Errorf("Code = %d, want %d", rec.Code, c.status)
			}
			if rec.Header().Get("Content-Type") != ContentType {
				t.Errorf("Content-Type = %q", rec.Header().Get("Content-Type"))
			}
		})
	}
}

// TestTitlesAndStatusesAgree — every registered type must carry BOTH a title and a
// status. The two maps are hand-maintained and only-add ([problem.go] header), and
// [TestWrite_AllKnownTypes] enumerates a fixed nine — so a type added to one map
// and not the other slips through: a missing `statuses` entry makes New() answer
// with status 0, a missing `titles` entry ships problem+json with an empty
// `title`. Both are silent, and neither shows up until a client reads the reply.
// Key parity is the property that actually holds, so it is what is asserted.
func TestTitlesAndStatusesAgree(t *testing.T) {
	for typ := range statuses {
		if titles[typ] == "" {
			t.Errorf("type %q has a status but no title - problem+json would ship an empty `title`", typ)
		}
	}
	for typ := range titles {
		if statuses[typ] == 0 {
			t.Errorf("type %q has a title but no status - New() would answer with status 0", typ)
		}
	}
	if len(statuses) == 0 || len(titles) == 0 {
		t.Fatal("the type catalog is empty - this guard would pass vacuously")
	}
}
