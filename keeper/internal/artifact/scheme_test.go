package artifact

import (
	"errors"
	"testing"
)

func TestValidateGitScheme_Allowed(t *testing.T) {
	cases := []string{
		"https://example.com/org/repo.git",
		"ssh://git@example.com/org/repo.git",
		"git@example.com:org/repo.git",
		"git@github.com:soul-stack/soul-stack.git",
	}
	for _, url := range cases {
		if err := validateGitScheme(url); err != nil {
			t.Errorf("validateGitScheme(%q) = %v, want nil", url, err)
		}
	}
}

func TestValidateGitScheme_Rejected(t *testing.T) {
	cases := []string{
		"http://example.com/org/repo.git", // unencrypted — not in the allowlist
		"ftp://example.com/repo.git",
		"/local/path/repo", // bare path, no scheme and no scp form
		"javascript:alert(1)",
	}
	for _, url := range cases {
		err := validateGitScheme(url)
		if err == nil {
			t.Errorf("validateGitScheme(%q) = nil, want ErrUnsupportedGitScheme", url)
			continue
		}
		if !errors.Is(err, ErrUnsupportedGitScheme) {
			t.Errorf("validateGitScheme(%q) = %v, want ErrUnsupportedGitScheme", url, err)
		}
	}
}

func TestValidateGitScheme_FileRequiresFlag(t *testing.T) {
	const url = "file:///tmp/repo"

	t.Setenv(allowFileReposEnv, "")
	if err := validateGitScheme(url); err == nil {
		t.Fatalf("file:// без флага: ожидалась ошибка")
	} else if !errors.Is(err, ErrUnsupportedGitScheme) {
		t.Fatalf("file:// без флага: err = %v, want ErrUnsupportedGitScheme", err)
	}

	t.Setenv(allowFileReposEnv, "1")
	if err := validateGitScheme(url); err != nil {
		t.Fatalf("file:// с флагом=1: err = %v, want nil", err)
	}

	t.Setenv(allowFileReposEnv, "0")
	if err := validateGitScheme(url); err == nil {
		t.Fatalf("file:// с флагом=0: ожидалась ошибка")
	}
}

// TestLoad_FileRejectedWithoutFlag checks that Load rejects a file:// URL
// when the flag is unset, before any git operations (security review L2).
func TestLoad_FileRejectedWithoutFlag(t *testing.T) {
	tr := newTestRepo(t)
	loader := newLoader(t)

	t.Setenv(allowFileReposEnv, "")
	_, err := loader.Load(t.Context(), ServiceRef{Name: "web-app", Git: tr.fileURL()})
	if err == nil {
		t.Fatalf("Load с file:// без флага: ожидалась ошибка")
	}
	if !errors.Is(err, ErrUnsupportedGitScheme) {
		t.Fatalf("Load с file:// без флага: err = %v, want ErrUnsupportedGitScheme", err)
	}
}
