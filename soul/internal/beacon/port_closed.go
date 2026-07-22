package beacon

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
	"google.golang.org/protobuf/types/known/structpb"
)

// PortClosedName is the core-beacon address (`core.beacon.<name>`, VigilDef.check).
const PortClosedName = beaconaddr.PortClosed

const (
	statePortOpen   State = "open"
	statePortClosed State = "closed"
)

// portClosedDefaultHost is the default target host: the service's local TCP
// port (the typical case is observing that a local daemon listens on its port).
const portClosedDefaultHost = "127.0.0.1"

// portClosedDefaultTimeout is the timeout for a single TCP dial. Short: a
// beacon check must fit within a scheduler tick — a hanging dial is itself
// an observed "unavailable".
const portClosedDefaultTimeout = 3 * time.Second

// PortClosed is a core-beacon observing TCP port availability (ADR-030).
// Read-only: a single TCP dial, no data sent over the socket. State: "open"
// if the connection succeeds, "closed" if the port refuses it
// (refused/timeout/host unreachable) — from the observer's view the port is
// closed, which is the event of interest, not a check error.
//
// Params:
//   - `port` (int, required) — TCP port 1..65535;
//   - `host` (string, optional, default "127.0.0.1") — target host/IP;
//   - `timeout` (string duration, optional, default "3s") — dial timeout.
type PortClosed struct {
	// Dial is a field so unit tests can substitute a deterministic fake with
	// no real socket. Production uses net.Dialer.DialContext.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
}

// NewPortClosed builds a beacon with the production dialer (net.Dialer).
func NewPortClosed() *PortClosed {
	return &PortClosed{Dial: (&net.Dialer{}).DialContext}
}

func (b *PortClosed) Check(ctx context.Context, params *structpb.Struct) (State, *structpb.Struct, error) {
	port, err := parseBeaconPort(params)
	if err != nil {
		return "", nil, err
	}
	host, err := util.OptStringParam(params, "host")
	if err != nil {
		return "", nil, err
	}
	if host == "" {
		host = portClosedDefaultHost
	}
	timeout, err := optBeaconTimeout(params, portClosedDefaultTimeout)
	if err != nil {
		return "", nil, err
	}

	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	address := net.JoinHostPort(host, strconv.Itoa(port))
	conn, derr := b.Dial(dialCtx, "tcp", address)
	if derr != nil {
		// refused/timeout/no-route — the port is unreachable to the observer →
		// "closed". A valid state, not a Check error.
		return statePortClosed, portData(host, port), nil
	}
	_ = conn.Close()
	return statePortOpen, portData(host, port), nil
}

func portData(host string, port int) *structpb.Struct {
	s, _ := structpb.NewStruct(map[string]any{
		"host": host,
		"port": port,
	})
	return s
}

// parseBeaconPort extracts the required TCP port (1..65535). Accepts a number
// (proto-json marshals numbers as float64) or a string (for ${...}
// interpolation, which yields a string) — mirrors core.firewall.parsePort.
func parseBeaconPort(params *structpb.Struct) (int, error) {
	if n, ok, err := util.OptIntParam(params, "port"); err == nil && ok {
		if n < 1 || n > 65535 {
			return 0, fmt.Errorf("param %q: must be 1..65535, got %d", "port", n)
		}
		return int(n), nil
	}
	s, serr := util.OptStringParam(params, "port")
	if serr != nil {
		return 0, fmt.Errorf("param %q: must be an integer", "port")
	}
	if s == "" {
		return 0, fmt.Errorf("param %q: required", "port")
	}
	n, cerr := strconv.Atoi(s)
	if cerr != nil {
		return 0, fmt.Errorf("param %q: invalid integer %q", "port", s)
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("param %q: must be 1..65535, got %d", "port", n)
	}
	return n, nil
}

// optBeaconTimeout parses the optional timeout param per the Soul Stack
// `duration` convention (shared/config — Go ParseDuration + `<N>d` suffix).
// Empty param → def. Shared parser for port_closed / http_unhealthy.
func optBeaconTimeout(params *structpb.Struct, def time.Duration) (time.Duration, error) {
	s, err := util.OptStringParam(params, "timeout")
	if err != nil {
		return 0, err
	}
	if s == "" {
		return def, nil
	}
	d, perr := config.ParseDuration(s)
	if perr != nil {
		return 0, fmt.Errorf("param %q: invalid duration %q", "timeout", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("param %q: must be positive, got %q", "timeout", s)
	}
	return d, nil
}
