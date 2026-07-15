package push

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"golang.org/x/crypto/ssh"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/config"
)

type fakeKVReader struct {
	data map[string]map[string]any
	err  error
}

func (f *fakeKVReader) ReadKV(_ context.Context, path string) (map[string]any, error) {
	if f.err != nil {
		return nil, f.err
	}
	v, ok := f.data[path]
	if !ok {
		return nil, errors.New("not found")
	}
	return v, nil
}

func genHostCAAuthorizedKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 genkey: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("ssh signer: %v", err)
	}
	return string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
}

func TestLoadHostCA_HappyPath(t *testing.T) {
	pem := genHostCAAuthorizedKey(t)
	vc := &fakeKVReader{data: map[string]map[string]any{
		"secret/keeper/ssh-host-ca": {"public_key": pem},
	}}
	ha, err := LoadHostCA(context.Background(), vc, "vault:secret/keeper/ssh-host-ca")
	if err != nil {
		t.Fatalf("LoadHostCA: %v", err)
	}
	if ha.CAPublicKey == nil {
		t.Fatal("CAPublicKey is nil")
	}
}

func TestLoadHostCA_Validation(t *testing.T) {
	pem := genHostCAAuthorizedKey(t)
	cases := []struct {
		name    string
		vc      KVReader
		ref     string
		wantErr string // substring
		wantIs  error
	}{
		{name: "nil vault", vc: nil, ref: "vault:secret/x/y", wantErr: "vault client is nil"},
		{name: "empty ref", vc: &fakeKVReader{}, ref: "", wantErr: "empty vault ref"},
		{name: "bad ref format", vc: &fakeKVReader{}, ref: "not-a-vault-ref", wantIs: keepervault.ErrInvalidVaultRef},
		{name: "kv missing field", vc: &fakeKVReader{data: map[string]map[string]any{
			"secret/keeper/ssh-host-ca": {"other": pem},
		}}, ref: "vault:secret/keeper/ssh-host-ca", wantErr: "has no \"public_key\" field"},
		{name: "empty public_key", vc: &fakeKVReader{data: map[string]map[string]any{
			"secret/keeper/ssh-host-ca": {"public_key": ""},
		}}, ref: "vault:secret/keeper/ssh-host-ca", wantErr: "is empty or not a string"},
		{name: "non-string public_key", vc: &fakeKVReader{data: map[string]map[string]any{
			"secret/keeper/ssh-host-ca": {"public_key": 42},
		}}, ref: "vault:secret/keeper/ssh-host-ca", wantErr: "is empty or not a string"},
		{name: "garbage PEM", vc: &fakeKVReader{data: map[string]map[string]any{
			"secret/keeper/ssh-host-ca": {"public_key": "garbage"},
		}}, ref: "vault:secret/keeper/ssh-host-ca", wantErr: "parse host-CA public key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadHostCA(context.Background(), tc.vc, tc.ref)
			if err == nil {
				t.Fatal("LoadHostCA: expected error, got nil")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("err = %v, want errors.Is(_, %v)", err, tc.wantIs)
			}
			if tc.wantErr != "" && !contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestLoadHostCAs_HappyPath — S7-3: two refs → two NamedHostKeyAuthority
// values with correctly propagated Name/SourceRef and parsed CAPubKey.
func TestLoadHostCAs_HappyPath(t *testing.T) {
	pem1 := genHostCAAuthorizedKey(t)
	pem2 := genHostCAAuthorizedKey(t)
	vc := &fakeKVReader{data: map[string]map[string]any{
		"secret/keeper/ssh-host-ca-prod":  {"public_key": pem1},
		"secret/keeper/ssh-host-ca-stage": {"public_key": pem2},
	}}
	refs := []config.KeeperPushCARef{
		{Ref: "vault:secret/keeper/ssh-host-ca-prod", Name: "prod"},
		{Ref: "vault:secret/keeper/ssh-host-ca-stage", Name: "stage"},
	}
	out, err := LoadHostCAs(context.Background(), vc, refs)
	if err != nil {
		t.Fatalf("LoadHostCAs: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2", len(out))
	}
	if out[0].Name != "prod" || out[1].Name != "stage" {
		t.Errorf("Names = %q,%q; want prod,stage", out[0].Name, out[1].Name)
	}
	if out[0].SourceRef != refs[0].Ref || out[1].SourceRef != refs[1].Ref {
		t.Errorf("SourceRef не пробросился: %+v vs %+v", out, refs)
	}
	if out[0].CAPubKey == nil || out[1].CAPubKey == nil {
		t.Error("CAPubKey nil в одном из элементов")
	}
}

// TestLoadHostCAs_PartialFail — S7-3: a vault error on the second ref → the
// whole LoadHostCAs fails fast with the failing CA's name in the wrapper
// (caller aborts startup).
func TestLoadHostCAs_PartialFail(t *testing.T) {
	pem := genHostCAAuthorizedKey(t)
	vc := &fakeKVReader{data: map[string]map[string]any{
		"secret/keeper/ssh-host-ca-prod": {"public_key": pem},
		// stage-CA is missing → LoadHostCA will return a "not found" error.
	}}
	refs := []config.KeeperPushCARef{
		{Ref: "vault:secret/keeper/ssh-host-ca-prod", Name: "prod"},
		{Ref: "vault:secret/keeper/ssh-host-ca-stage", Name: "stage"},
	}
	_, err := LoadHostCAs(context.Background(), vc, refs)
	if err == nil {
		t.Fatal("LoadHostCAs: ждали ошибку на missing stage-CA")
	}
	if !contains(err.Error(), "LoadHostCAs[stage]") {
		t.Errorf("err не содержит имени сбойного CA: %v", err)
	}
}

// TestLoadHostCAs_EmptyRefs — an empty set → (nil, nil). The caller decides
// whether that's a failure or a valid case (singular path / push disabled).
func TestLoadHostCAs_EmptyRefs(t *testing.T) {
	out, err := LoadHostCAs(context.Background(), &fakeKVReader{}, nil)
	if err != nil {
		t.Fatalf("LoadHostCAs(nil): %v", err)
	}
	if out != nil {
		t.Errorf("out = %v, want nil", out)
	}
}

// TestLoadHostCAs_NilVault — defensive guard for a nil vault client (the
// caller guarantees non-nil, but we check it for symmetry with the single
// LoadHostCA).
func TestLoadHostCAs_NilVault(t *testing.T) {
	refs := []config.KeeperPushCARef{{Ref: "vault:secret/x/y", Name: "x"}}
	_, err := LoadHostCAs(context.Background(), nil, refs)
	if err == nil {
		t.Fatal("LoadHostCAs(nil vault): ждали ошибку")
	}
	if !contains(err.Error(), "vault client is nil") {
		t.Errorf("err не про nil vault: %v", err)
	}
}
