package trial

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const redisExporterMultiInstanceCase = "../../../examples/destiny/redis-exporter/_trial/scenario/multi-instance/tests/two-apply-remove-one/case.yml"
const redisExporterRedissTLSCase = "../../../examples/destiny/redis-exporter/_trial/scenario/apply/tests/rediss-web-config/case.yml"

type exporterUnitGuard struct {
	Listen     string
	Target     string
	Dependency string
	User       string
}

// TestRedisExporterMultiInstanceIsolation is the executable NIM-606 guard behind the L0
// case. It checks the rendered product plan rather than merely searching YAML text: two
// present applies must converge the same binary into two independent units, while the
// subsequent Sentinel teardown must have no operation capable of stopping/deleting Redis.
func TestRedisExporterMultiInstanceIsolation(t *testing.T) {
	t.Parallel()

	c, file, err := LoadCase(redisExporterMultiInstanceCase)
	if err != nil {
		t.Fatalf("LoadCase: %v", err)
	}
	rendered, err := renderCase(context.Background(), c, file)
	if err != nil {
		t.Fatalf("renderCase: %v", err)
	}

	units := map[string]exporterUnitGuard{}
	var running, stopped, disabled []string
	binaryInstalls := 0
	secretFiles := map[string]bool{}

	for _, task := range rendered.tasks {
		if task.Params == nil { // skipped when/include branch placeholder
			continue
		}
		params := task.Params.AsMap()
		path, _ := params["path"].(string)

		switch task.Module {
		case "core.file.present":
			if path == "/usr/local/bin/redis_exporter" {
				binaryInstalls++
			}

		case "core.file.rendered":
			if strings.HasPrefix(path, "/etc/systemd/system/redis_exporter-") {
				vars := nestedMap(t, params, "render_context", "vars")
				units[path] = exporterUnitGuard{
					Listen:     nestedString(t, vars, "listen"),
					Target:     nestedString(t, vars, "redis_addr"),
					Dependency: nestedString(t, vars, "dependency_unit"),
					User:       nestedString(t, vars, "redis_user"),
				}
				content := nestedString(t, params, "template_content")
				for _, fragment := range []string{
					`printf "--redis.addr=%s" .vars.redis_addr | quote`,
					`printf "--web.listen-address=%s" .vars.listen | quote`,
					`range .vars.extra_args }} {{ . | quote }}`,
				} {
					if !strings.Contains(content, fragment) {
						t.Fatalf("unit template %s is missing argv quoting guard %q", path, fragment)
					}
				}
				if strings.Contains(content, "redis-monitoring-password-32") ||
					strings.Contains(content, "sentinel-monitoring-password-32") ||
					strings.Contains(content, "Environment=REDIS_PASSWORD=") {
					t.Fatalf("world-readable unit %s contains a Redis password", path)
				}
			}
			if strings.HasPrefix(path, "/etc/default/redis_exporter-") ||
				(strings.HasPrefix(path, "/etc/redis_exporter/") && strings.HasSuffix(path, "/web.yml")) {
				if mode := nestedString(t, params, "mode"); mode != "0600" {
					t.Fatalf("secret file %s mode = %q, want 0600", path, mode)
				}
				// `no_log:` is gone ([ADR-0083] §8). What protects this render is
				// narrower and does not depend on the author remembering a flag:
				// the file lands 0600 root-only (asserted above), the password
				// reaches it only through a Go-template reference resolved on the
				// host, and maskRunPlanParams drops template_content/render_context
				// from the stored plan outright. The assertion that bites is that
				// no credential VALUE is baked into the template text.
				if content := nestedString(t, params, "template_content"); strings.Contains(content, "change-me-please-32") {
					t.Fatalf("secret render %s bakes a plaintext password into template_content", path)
				}
				if strings.HasPrefix(path, "/etc/default/redis_exporter-") {
					content := nestedString(t, params, "template_content")
					if !strings.Contains(content, "REDIS_PASSWORD={{ .vars.password | quote }}") {
						t.Fatalf("EnvironmentFile template %s does not quote the password assignment", path)
					}
				}
				secretFiles[path] = true
			}

		case "core.service.running":
			running = append(running, nestedString(t, params, "name"))
		case "core.service.stopped":
			stopped = append(stopped, nestedString(t, params, "name"))
		case "core.service.disabled":
			disabled = append(disabled, nestedString(t, params, "name"))
		case "core.file.absent":
			if path == "/usr/local/bin/redis_exporter" {
				t.Fatal("instance teardown contains unconditional shared-binary deletion")
			}
			if strings.Contains(path, "redis_exporter-redis") || strings.Contains(path, "/redis_exporter/redis/") {
				t.Fatalf("Sentinel teardown reaches Redis-owned path %s", path)
			}
		}
	}

	wantUnits := map[string]exporterUnitGuard{
		"/etc/systemd/system/redis_exporter-redis.service": {
			Listen: ":9121", Target: "unix:///var/run/redis/redis-server.sock", Dependency: "redis-server.service", User: "monitoring",
		},
		"/etc/systemd/system/redis_exporter-sentinel.service": {
			Listen: ":9122", Target: "redis://127.0.0.1:26379", Dependency: "redis-sentinel.service", User: "monitoring",
		},
	}
	if !reflect.DeepEqual(units, wantUnits) {
		t.Fatalf("isolated unit contracts = %#v, want %#v", units, wantUnits)
	}
	if binaryInstalls != 2 {
		t.Fatalf("shared binary installs = %d, want 2 convergent core.file.present steps (one per apply)", binaryInstalls)
	}

	sort.Strings(running)
	if want := []string{"redis_exporter-redis", "redis_exporter-sentinel"}; !reflect.DeepEqual(running, want) {
		t.Fatalf("running exporter instances = %v, want %v", running, want)
	}
	if !reflect.DeepEqual(stopped, []string{"redis_exporter-sentinel"}) {
		t.Fatalf("stopped exporter instances = %v, want Sentinel only", stopped)
	}
	if !reflect.DeepEqual(disabled, []string{"redis_exporter-sentinel"}) {
		t.Fatalf("disabled exporter instances = %v, want Sentinel only", disabled)
	}

	wantSecretFiles := []string{
		"/etc/default/redis_exporter-redis",
		"/etc/default/redis_exporter-sentinel",
		"/etc/redis_exporter/redis/web.yml",
		"/etc/redis_exporter/sentinel/web.yml",
	}
	for _, path := range wantSecretFiles {
		if !secretFiles[path] {
			t.Errorf("missing root-only per-instance secret file %s", path)
		}
	}
}

// TestRedisExporterRedissWebTLS pins the third supported Redis transport and the
// systemd-credential bridge that lets DynamicUser consume root-only web/TLS sources.
func TestRedisExporterRedissWebTLS(t *testing.T) {
	t.Parallel()

	c, file, err := LoadCase(redisExporterRedissTLSCase)
	if err != nil {
		t.Fatalf("LoadCase: %v", err)
	}
	rendered, err := renderCase(context.Background(), c, file)
	if err != nil {
		t.Fatalf("renderCase: %v", err)
	}

	var unitFound, webFound bool
	for _, task := range rendered.tasks {
		if task.Params == nil {
			continue
		}
		params := task.Params.AsMap()
		path, _ := params["path"].(string)
		switch path {
		case "/etc/systemd/system/redis_exporter-redis-tls.service":
			unitFound = true
			vars := nestedMap(t, params, "render_context", "vars")
			if got := nestedString(t, vars, "redis_addr"); got != "rediss://127.0.0.1:6379" {
				t.Fatalf("rediss unit target = %q", got)
			}
			for key, want := range map[string]string{
				"unit_name":       "redis_exporter-redis-tls",
				"web_config_file": "/etc/redis_exporter/redis-tls/web.yml",
				"tls_cert_file":   "/etc/redis-exporter/tls/server.crt",
				"tls_key_file":    "/etc/redis-exporter/tls/server.key",
			} {
				if got := nestedString(t, vars, key); got != want {
					t.Errorf("TLS unit var %s = %q, want %q", key, got, want)
				}
			}
			if !nestedBool(t, vars, "web_config_enabled") || !nestedBool(t, vars, "tls_enabled") {
				t.Fatal("rediss/TLS unit did not enable web-config and TLS credential branches")
			}
			content := nestedString(t, params, "template_content")
			for _, fragment := range []string{
				"LoadCredential=web-config:{{ .vars.web_config_file }}",
				"LoadCredential=tls-cert:{{ .vars.tls_cert_file }}",
				"LoadCredential=tls-key:{{ .vars.tls_key_file }}",
				`printf "--web.config.file=/run/credentials/%s.service/web-config" .vars.unit_name | quote`,
			} {
				if !strings.Contains(content, fragment) {
					t.Errorf("TLS unit template is missing %q", fragment)
				}
			}
			if strings.Contains(content, "$2y$12$abcdefghijklmnopqrstuv") || strings.Contains(content, "rediss-monitoring-password-32") {
				t.Fatal("world-readable TLS unit contains a credential value")
			}

		case "/etc/redis_exporter/redis-tls/web.yml":
			webFound = true
			if nestedString(t, params, "mode") != "0600" {
				t.Fatal("TLS web-config must be a 0600 render")
			}
			if content := nestedString(t, params, "template_content"); strings.Contains(content, "$2y$12$abcdefghijklmnopqrstuv") {
				t.Fatal("TLS web-config bakes a plaintext credential into template_content")
			}
		}
	}
	if !unitFound || !webFound {
		t.Fatalf("rediss/TLS resources missing: unit=%v web=%v", unitFound, webFound)
	}
}

func nestedMap(t *testing.T, root map[string]any, path ...string) map[string]any {
	t.Helper()
	value := any(root)
	for _, key := range path {
		m, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%s: got %T, want map", strings.Join(path, "."), value)
		}
		value, ok = m[key]
		if !ok {
			t.Fatalf("%s: key %q missing", strings.Join(path, "."), key)
		}
	}
	m, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s: got %T, want map", strings.Join(path, "."), value)
	}
	return m
}

func nestedString(t *testing.T, root map[string]any, key string) string {
	t.Helper()
	value, ok := root[key]
	if !ok {
		t.Fatalf("key %q missing from %v", key, sortedMapKeys(root))
	}
	s, ok := value.(string)
	if !ok {
		t.Fatalf("key %q = %T, want string", key, value)
	}
	return s
}

func nestedBool(t *testing.T, root map[string]any, key string) bool {
	t.Helper()
	value, ok := root[key]
	if !ok {
		t.Fatalf("key %q missing from %v", key, sortedMapKeys(root))
	}
	b, ok := value.(bool)
	if !ok {
		t.Fatalf("key %q = %T, want bool", key, value)
	}
	return b
}

func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
