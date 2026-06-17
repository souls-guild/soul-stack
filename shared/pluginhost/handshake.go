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

// readHandshake читает stdout плагина, игнорируя строки до первой с
// `soul_stack:"plugin-v1"` маркером (docs/keeper/plugins.md → Поведение
// host-а при handshake). Превышение startupTimeout — hard fail.
func readHandshake(stdout io.ReadCloser, startupTimeout time.Duration) (*pluginv1.Handshake, error) {
	type result struct {
		hs  *pluginv1.Handshake
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		// Увеличиваем буфер: handshake-строка может содержать длинный
		// server_cert в post-MVP. 64KB — с запасом.
		scanner.Buffer(make([]byte, 0, 4096), 64*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			// Поверхностная проверка через strings: parse JSON каждой
			// рандомной диагностической строки — лишняя работа.
			if !strings.Contains(line, `"soul_stack"`) {
				continue
			}
			hs := &pluginv1.Handshake{}
			if err := protojson.Unmarshal([]byte(line), hs); err != nil {
				// Строка с soul_stack-маркером, но невалидная — это уже
				// явная ошибка, не дренируем дальше.
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

// validateHandshake — cross-check матрица из docs/keeper/plugins.md.
//
// Проверяет, что (1) protocol_version плагина среди поддерживаемых host-ом,
// (2) protocol_version совпадает с тем, что объявлен в manifest-е,
// (3) kind в handshake совпадает с manifest.kind, (4) network=unix
// (единственное значение MVP), (5) address совпадает с тем сокетом, который
// host передал плагину через env (защита от случайного listen-а плагином не
// на нашем сокете).
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
