package pluginhost

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/handshake"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	"google.golang.org/protobuf/encoding/protojson"
)

// readHandshake reads the plugin's stdout, ignoring lines until the first one with
// the `soul_stack:"plugin-v1"` marker (docs/keeper/plugins.md → Host behavior on
// handshake). Exceeding startupTimeout is a hard fail.
func readHandshake(stdout io.ReadCloser, startupTimeout time.Duration) (*pluginv1.Handshake, error) {
	type result struct {
		hs  *pluginv1.Handshake
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		// Grow the buffer: a handshake line may contain a long server_cert in
		// post-MVP. 64KB — with headroom.
		scanner.Buffer(make([]byte, 0, 4096), 64*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			// Shallow check via strings: parsing JSON of every random diagnostic
			// line would be wasted work.
			if !strings.Contains(line, `"soul_stack"`) {
				continue
			}
			hs := &pluginv1.Handshake{}
			if err := protojson.Unmarshal([]byte(line), hs); err != nil {
				// A line with the soul_stack marker but invalid — this is already
				// a clear error, we don't drain further.
				resCh <- result{nil, fmt.Errorf("handshake: parse json: %w", err)}
				return
			}
			if hs.GetSoulStack() != handshake.Magic {
				continue
			}
			resCh <- result{hs, nil}
			return
		}
		if err := scanner.Err(); err != nil {
			resCh <- result{nil, fmt.Errorf("handshake: read stdout: %w", err)}
			return
		}
		resCh <- result{nil, errors.New("handshake: stdout closed without handshake")}
	}()
	select {
	case r := <-resCh:
		return r.hs, r.err
	case <-time.After(startupTimeout):
		return nil, fmt.Errorf("handshake: startup_timeout %s exceeded", startupTimeout)
	}
}

// validateHandshake — the cross-check matrix from docs/keeper/plugins.md.
//
// Checks that (1) the plugin's protocol_version is among those supported by the
// host, (2) protocol_version matches the one declared in the manifest, (3) kind in
// the handshake matches manifest.kind, (4) network=unix (the only MVP value),
// (5) address matches the socket the host passed to the plugin via env (guards
// against the plugin accidentally listening on a socket other than ours).
func validateHandshake(m *sharedplugin.Manifest, hs *pluginv1.Handshake, expectedAddr string) error {
	if !containsInt32(sharedplugin.SupportedProtocolVersions, hs.GetProtocolVersion()) {
		return fmt.Errorf("handshake: protocol_version=%d, host supports %v",
			hs.GetProtocolVersion(), sharedplugin.SupportedProtocolVersions)
	}
	if hs.GetProtocolVersion() != m.ProtocolVersion {
		return fmt.Errorf("handshake: protocol_version drift manifest=%d handshake=%d",
			m.ProtocolVersion, hs.GetProtocolVersion())
	}
	if hs.GetKind() != m.ProtoKind() {
		return fmt.Errorf("handshake: kind drift manifest=%s handshake=%s", m.Kind, hs.GetKind())
	}
	if hs.GetNetwork() != handshake.NetworkUnix {
		return fmt.Errorf("handshake: network=%q, host supports only %q", hs.GetNetwork(), handshake.NetworkUnix)
	}
	if hs.GetAddress() != expectedAddr {
		return fmt.Errorf("handshake: address drift host=%q plugin=%q", expectedAddr, hs.GetAddress())
	}
	return nil
}

func containsInt32(xs []int32, x int32) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
