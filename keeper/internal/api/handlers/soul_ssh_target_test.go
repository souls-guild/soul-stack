package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// doUpdateSshTarget разбирает JSON-тело строго (DisallowUnknownFields, как прежний
// (w,r)-роут — bad/unknown JSON → 400) и вызывает UpdateSshTargetTyped напрямую
// (handler-native T5d), сериализуя результат в recorder.
func doUpdateSshTarget(t *testing.T, h *SoulHandler, sid, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/v1/souls/"+sid+"/ssh-target", strings.NewReader(body))
	rec := httptest.NewRecorder()

	var raw struct {
		SSHPort     int    `json:"ssh_port"`
		SSHUser     string `json:"ssh_user"`
		SoulPath    string `json:"soul_path"`
		SSHProvider string `json:"ssh_provider"`
	}
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		problem.Write(rec, problem.New(problem.TypeMalformedRequest, req.URL.Path, "invalid JSON body: "+err.Error()))
		return rec
	}
	reply, err := h.UpdateSshTargetTyped(req.Context(), sid, SoulSshTargetInput{
		SSHPort:     raw.SSHPort,
		SSHUser:     raw.SSHUser,
		SoulPath:    raw.SoulPath,
		SSHProvider: raw.SSHProvider,
	})
	if err != nil {
		writeProblemError(rec, req, err)
		return rec
	}
	writeJSON(rec, http.StatusOK, soulSshTargetViewJSON(reply.Body), h.logger)
	return rec
}

// soulSshTargetViewJSON проецирует доменный SoulSshTargetView в map с json-ключами native
// SoulSshTargetReply (sid + nested ssh_target; ssh_provider omitempty).
func soulSshTargetViewJSON(v SoulSshTargetView) map[string]any {
	tgt := map[string]any{
		"soul_path": v.SoulPath,
		"ssh_port":  v.SSHPort,
		"ssh_user":  v.SSHUser,
	}
	if v.SSHProvider != "" {
		tgt["ssh_provider"] = v.SSHProvider
	}
	return map[string]any{"sid": v.SID, "ssh_target": tgt}
}

func TestUpdateSshTarget_HappyPath(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doUpdateSshTarget(t, h, "soul-a.example.com",
		`{"ssh_port":2222,"ssh_user":"deploy","soul_path":"/opt/soul/bin/soul"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		SID       string `json:"sid"`
		SSHTarget struct {
			SSHPort  int    `json:"ssh_port"`
			SSHUser  string `json:"ssh_user"`
			SoulPath string `json:"soul_path"`
		} `json:"ssh_target"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.SID != "soul-a.example.com" {
		t.Errorf("sid = %q, want soul-a.example.com", out.SID)
	}
	if out.SSHTarget.SSHPort != 2222 || out.SSHTarget.SSHUser != "deploy" || out.SSHTarget.SoulPath != "/opt/soul/bin/soul" {
		t.Errorf("ssh_target = %+v, want explicit values", out.SSHTarget)
	}
	if pool.updateSshTargetCalls != 1 {
		t.Errorf("updateSshTargetCalls = %d, want 1", pool.updateSshTargetCalls)
	}
}

func TestUpdateSshTarget_NotFound(t *testing.T) {
	pool := &fakeSoulPool{updateSshTargetNotFound: true}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doUpdateSshTarget(t, h, "ghost.example.com",
		`{"ssh_port":22,"ssh_user":"root","soul_path":"/usr/local/bin/soul"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

func TestUpdateSshTarget_BadJSON_400(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doUpdateSshTarget(t, h, "soul-a.example.com", `{not-json`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if pool.updateSshTargetCalls != 0 {
		t.Errorf("updateSshTargetCalls = %d, want 0 (validation before DB)", pool.updateSshTargetCalls)
	}
}

func TestUpdateSshTarget_BadSID_422(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doUpdateSshTarget(t, h, "BAD_SID",
		`{"ssh_port":22,"ssh_user":"root","soul_path":"/usr/local/bin/soul"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
}

func TestUpdateSshTarget_BadPort_422(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	cases := []string{
		`{"ssh_port":0,"ssh_user":"root","soul_path":"/x"}`,
		`{"ssh_port":-1,"ssh_user":"root","soul_path":"/x"}`,
		`{"ssh_port":65536,"ssh_user":"root","soul_path":"/x"}`,
	}
	for _, body := range cases {
		rec := doUpdateSshTarget(t, h, "soul-a.example.com", body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("body=%s → status = %d, want 422", body, rec.Code)
		}
	}
	if pool.updateSshTargetCalls != 0 {
		t.Errorf("updateSshTargetCalls = %d, want 0", pool.updateSshTargetCalls)
	}
}

func TestUpdateSshTarget_EmptyUser_422(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doUpdateSshTarget(t, h, "soul-a.example.com",
		`{"ssh_port":22,"ssh_user":"","soul_path":"/x"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
}

func TestUpdateSshTarget_RelativePath_422(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doUpdateSshTarget(t, h, "soul-a.example.com",
		`{"ssh_port":22,"ssh_user":"root","soul_path":"relative/path"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (relative path), body=%s", rec.Code, rec.Body.String())
	}
	if pool.updateSshTargetCalls != 0 {
		t.Errorf("updateSshTargetCalls = %d, want 0", pool.updateSshTargetCalls)
	}
}

func TestUpdateSshTarget_UnknownField_400(t *testing.T) {
	// DisallowUnknownFields на JSON-decoder → unknown поле → 400.
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doUpdateSshTarget(t, h, "soul-a.example.com",
		`{"ssh_port":22,"ssh_user":"root","soul_path":"/x","unknown":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}
