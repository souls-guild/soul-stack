package consolerunner

import "time"

// Default resource limits for console sessions on one Soul. They are
// deliberately code constants rather than `soul.yml` surface: the operator-facing
// policy (idle timeout, per-operator console budget, RBAC) lives Keeper-side, and
// these are only the host's self-defence floor.
const (
	// DefaultMaxSessions caps live consoles on a single host. The product ceiling
	// is "tens of consoles per operator" spread across hosts; more than a handful
	// of shells on ONE machine is a runaway UI or an attack, not an operator.
	DefaultMaxSessions = 8

	// DefaultReadBufferBytes is both the pty read size and the ceiling on one
	// ConsoleChunk payload. 32 KiB is well under the gRPC message limit and large
	// enough that a full-screen redraw (top/vim) usually fits in one chunk.
	DefaultReadBufferBytes = 32 * 1024

	// DefaultQueueChunks is the depth of the per-session output queue that
	// decouples the pty reader from the stream sender. At the default buffer size
	// this holds 1 MiB of in-flight output — enough to ride out a burst, small
	// enough that a flood is dropped instead of buffered forever.
	DefaultQueueChunks = 32

	// DefaultRateBytesPerSec paces a session's output. A human terminal produces
	// orders of magnitude less; the cap exists so `yes` or `cat /dev/urandom`
	// cannot saturate the EventStream that also carries apply traffic.
	DefaultRateBytesPerSec = 1 << 20

	// DefaultBurstBytes lets an idle session emit a full screen instantly instead
	// of trickling it out at the sustained rate.
	DefaultBurstBytes = 256 * 1024

	// DefaultCols / DefaultRows are the classic terminal geometry, used when
	// ConsoleOpen carries zeros.
	DefaultCols = 80
	DefaultRows = 24

	// DefaultKillGrace is how long a session may take to die after SIGHUP before
	// it is SIGKILLed, and again before the pty master is force-closed to unblock
	// a stuck read.
	DefaultKillGrace = 2 * time.Second
)

// Limits is the Soul-side resource envelope for console sessions. Operator-facing
// fields come from the `console:` block of `soul.yml`
// (see [config.SoulConsole]); the rest are internal tuning.
//
// The zero value is not usable directly — call [Limits.withDefaults] (New does
// it) to fill every unset field from the Default* constants. Disabled is the one
// exception: its zero value is the permissive default, so a caller that builds
// Limits by hand (tests) gets a working console.
type Limits struct {
	// Disabled forbids consoles on this host outright (`console.enabled: false`).
	// Phrased negatively ONLY here, at the struct boundary, so that the zero
	// Limits value stays usable; the config surface is the positive `enabled`.
	Disabled bool

	MaxSessions     int
	ReadBufferBytes int
	QueueChunks     int
	RateBytesPerSec int
	BurstBytes      int
	Cols            uint32
	Rows            uint32
	KillGrace       time.Duration

	// Shell overrides the default shell for sessions that don't name one.
	// Empty → [resolveDefaultShell]. Set by tests to run a deterministic
	// program instead of an interactive shell.
	Shell string
}

// withDefaults returns a copy with every unset (zero) field filled in.
func (l Limits) withDefaults() Limits {
	if l.MaxSessions <= 0 {
		l.MaxSessions = DefaultMaxSessions
	}
	if l.ReadBufferBytes <= 0 {
		l.ReadBufferBytes = DefaultReadBufferBytes
	}
	if l.QueueChunks <= 0 {
		l.QueueChunks = DefaultQueueChunks
	}
	if l.RateBytesPerSec <= 0 {
		l.RateBytesPerSec = DefaultRateBytesPerSec
	}
	if l.BurstBytes <= 0 {
		l.BurstBytes = DefaultBurstBytes
	}
	if l.Cols == 0 {
		l.Cols = DefaultCols
	}
	if l.Rows == 0 {
		l.Rows = DefaultRows
	}
	if l.KillGrace <= 0 {
		l.KillGrace = DefaultKillGrace
	}
	return l
}
