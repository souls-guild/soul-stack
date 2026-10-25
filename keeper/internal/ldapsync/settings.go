// Package ldapsync is the background LDAP group sync (ADR-058 amendment
// NIM-1053): one leader per cluster re-reads the groups of every
// `auth_method = ldap` operator once per interval, through the service account,
// and reconciles its roles with the code a login runs. Until it existed the
// groups were read only at login, so a token living up to auth.jwt.ttl_max kept
// a role its owner had already lost in the directory.
//
// It never revokes an operator — revocation cannot be undone and a broken
// filter would revoke everyone — and it never touches a role outside
// `group_role_map`. Authorization reads roles from the RBAC snapshot on every
// request, so a role it drops stops working for every live token of that
// operator within one snapshot refresh; no token has to be revoked.
package ldapsync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
)

// SettingsKey is the `keeper_settings` row holding the sync settings: one key
// whose value is a JSON object, so later settings (a cron `schedule` beside
// `interval`) join the group instead of becoming keys of their own.
const SettingsKey = "ldap_sync"

const (
	DefaultInterval = 15 * time.Minute
	MinInterval     = time.Minute
	MaxInterval     = 24 * time.Hour
)

// ErrInvalidSettings — the value is not a settings object this version accepts.
var ErrInvalidSettings = errors.New("ldapsync: invalid settings")

// Settings is the effective sync configuration.
type Settings struct {
	// Interval — how often each operator's groups are re-read. LDAP being
	// unreachable keeps the roles for one more interval: the second failed
	// attempt in a row drops them.
	Interval time.Duration
}

// DefaultSettings is what applies while the key is absent.
func DefaultSettings() Settings { return Settings{Interval: DefaultInterval} }

type settingsJSON struct {
	Interval string `json:"interval"`
}

// ParseSettings decodes the stored JSON object. Unknown fields are ignored: a
// newer Keeper may add a setting beside interval during a rolling upgrade, and
// an older leader must keep running on the interval rather than fall back to
// the default. PUT cannot write an unknown field — its body is typed.
func ParseSettings(raw string) (Settings, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	var v settingsJSON
	if err := dec.Decode(&v); err != nil {
		return Settings{}, fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Settings{}, fmt.Errorf("%w: trailing data after the object", ErrInvalidSettings)
	}
	interval, err := ParseInterval(v.Interval)
	if err != nil {
		return Settings{}, err
	}
	return Settings{Interval: interval}, nil
}

// ParseInterval accepts the duration grammar of keeper.yml (`15m`, `1h`, `1d`)
// inside [MinInterval, MaxInterval].
func ParseInterval(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("%w: interval is required", ErrInvalidSettings)
	}
	d, err := config.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%w: interval %q: %v", ErrInvalidSettings, s, err)
	}
	if d < MinInterval || d > MaxInterval {
		return 0, fmt.Errorf("%w: interval %q is outside [%s, %s]", ErrInvalidSettings, s, FormatInterval(MinInterval), FormatInterval(MaxInterval))
	}
	return d, nil
}

// Encode renders the stored form.
func (s Settings) Encode() string {
	b, _ := json.Marshal(settingsJSON{Interval: FormatInterval(s.Interval)})
	return string(b)
}

// FormatInterval renders a duration the way an operator writes it: `15m`
// rather than Go's `15m0s`.
func FormatInterval(d time.Duration) string {
	switch {
	case d > 0 && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d > 0 && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return d.String()
	}
}
