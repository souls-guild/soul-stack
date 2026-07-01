package soulinstall_test

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/soulinstall"

	"gopkg.in/yaml.v3"
)

const testCAPem = `-----BEGIN CERTIFICATE-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAcXamPlexamplePEMblock
ForUnitTestsOnlyNotARealCertificateThisIsJustForRenderCheckLength
PaddingPaddingPaddingPaddingPaddingPaddingPaddingPaddingPadding==
-----END CERTIFICATE-----`

func validBlueprint() soulinstall.Blueprint {
	return soulinstall.Blueprint{
		BootstrapEndpoint: "lb.keeper.example:9442",
		KeeperCAPem:       testCAPem,
		SoulBinaryURL:     "https://artifacts.example/soul/v1.0.0/soul",
		SoulVersion:       "v1.0.0",
	}
}

// TestRenderInstallScript проверяет порядок SSH-шагов full-install (каталоги →
// keeper-ca.pem → soul.yml → soul.service → curl-бинарь) и ARGV-LEAK-GUARD: PEM
// CA уходит через .Stdin, а не печатается в .Cmd (cat > path, не echo PEM).
func TestRenderInstallScript(t *testing.T) {
	steps, err := soulinstall.RenderInstallScript(validBlueprint())
	if err != nil {
		t.Fatalf("RenderInstallScript: %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("RenderInstallScript returned no steps")
	}

	// Порядок: каждый «маркер» обязан впервые появиться позже предыдущего.
	// Маркер ищется в .Cmd (Stdin несёт контент, а не путь записи).
	order := []struct {
		name   string
		marker string
	}{
		{"install-d dirs", "install -d"},
		{"keeper-ca.pem", soulinstall.KeeperCAPath},
		{"soul.yml", soulinstall.SoulConfigPath},
		{"soul.service", soulinstall.SoulServicePath},
		{"curl binary", "curl"},
	}
	prev := -1
	for _, want := range order {
		idx := firstStepCmdContains(steps, want.marker)
		if idx < 0 {
			t.Fatalf("step %q (marker %q) not found in install script", want.name, want.marker)
		}
		if idx <= prev {
			t.Errorf("step %q at index %d out of order (must be > %d)", want.name, idx, prev)
		}
		prev = idx
	}

	// curl-бинарь — последний по порядку среди маркеров; chmod 0755 идёт после.
	if !stepCmdContainsAny(steps, soulinstall.SoulBinaryPath) {
		t.Errorf("install script does not reference soul binary path %q", soulinstall.SoulBinaryPath)
	}

	// ARGV-LEAK-GUARD: тело CA (PEM) едет в .Stdin шага, ни один .Cmd не несёт PEM.
	caStdinSeen := false
	for i, s := range steps {
		if strings.Contains(s.Cmd, "BEGIN CERTIFICATE") || strings.Contains(s.Cmd, "END CERTIFICATE") {
			t.Errorf("step %d leaks PEM body into argv (.Cmd): %q", i, s.Cmd)
		}
		if strings.Contains(s.Cmd, "echo") && strings.Contains(s.Cmd, soulinstall.KeeperCAPath) {
			t.Errorf("step %d uses echo to write CA (argv leak): %q", i, s.Cmd)
		}
		if strings.Contains(string(s.Stdin), "BEGIN CERTIFICATE") {
			caStdinSeen = true
			// CA-write шаг должен писать в keeper-ca.pem через redirect, не echo.
			if !strings.Contains(s.Cmd, soulinstall.KeeperCAPath) {
				t.Errorf("CA-stdin step %d does not redirect into keeper-ca.pem: %q", i, s.Cmd)
			}
		}
	}
	if !caStdinSeen {
		t.Error("CA PEM body never delivered via .Stdin — ARGV-LEAK-GUARD cannot hold")
	}
}

// TestRenderInstallScript_CAmode: curl-шаг скачивания бинаря пинится на keeper-CA
// (--cacert) в режимах keeper/пусто и идёт без --cacert при system.
func TestRenderInstallScript_CAmode(t *testing.T) {
	t.Run("system → no --cacert", func(t *testing.T) {
		bp := validBlueprint()
		bp.SoulBinaryCA = soulinstall.SoulBinaryCASystem
		steps, err := soulinstall.RenderInstallScript(bp)
		if err != nil {
			t.Fatalf("RenderInstallScript: %v", err)
		}
		curl := binaryCurlStep(t, steps)
		if strings.Contains(curl, "--cacert") {
			t.Errorf("system mode must not pin --cacert, got: %q", curl)
		}
	})

	for _, ca := range []string{"", soulinstall.SoulBinaryCAKeeper} {
		t.Run("keeper/empty → --cacert "+ca, func(t *testing.T) {
			bp := validBlueprint()
			bp.SoulBinaryCA = ca
			steps, err := soulinstall.RenderInstallScript(bp)
			if err != nil {
				t.Fatalf("RenderInstallScript: %v", err)
			}
			curl := binaryCurlStep(t, steps)
			if !strings.Contains(curl, "--cacert") || !strings.Contains(curl, soulinstall.KeeperCAPath) {
				t.Errorf("keeper mode must pin --cacert on keeper-ca.pem, got: %q", curl)
			}
		})
	}
}

// TestRenderCloudInitYAML_Stable — байт-стабильность cloud-init после выноса
// blueprint: ключевые элементы userdata на месте и https-floor отвергает http.
func TestRenderCloudInitYAML_Stable(t *testing.T) {
	out, err := soulinstall.RenderCloudInitYAML(validBlueprint())
	if err != nil {
		t.Fatalf("RenderCloudInitYAML: %v", err)
	}
	if !strings.HasPrefix(out, "#cloud-config") {
		t.Errorf("output does not start with #cloud-config header")
	}
	for _, want := range []string{
		"write_files:",
		soulinstall.KeeperCAPath,
		"-----BEGIN CERTIFICATE-----",
		soulinstall.SoulConfigPath,
		soulinstall.SoulServicePath,
		"runcmd:",
		"curl",
		"https://artifacts.example/soul/v1.0.0/soul",
		"--cacert /etc/soul/tls/keeper-ca.pem",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cloud-init missing key element %q", want)
		}
	}

	// Валидный YAML с write_files + runcmd top-level (header игнорируется).
	var v map[string]any
	if err := yaml.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("rendered userdata is not valid YAML: %v", err)
	}
	if _, ok := v["write_files"]; !ok {
		t.Errorf("rendered YAML has no top-level write_files key")
	}
	if _, ok := v["runcmd"]; !ok {
		t.Errorf("rendered YAML has no top-level runcmd key")
	}

	// https-floor: plain http отвергается на render-е (security, независимо от CA).
	bp := validBlueprint()
	bp.SoulBinaryURL = "http://artifacts.example/soul"
	if _, err := soulinstall.RenderCloudInitYAML(bp); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("plain http URL must be rejected with https-floor error, got %v", err)
	}
}

// TestRenderCloudInitYAML_SelfOnboard — self-onboard «Вариант T» (ADR-017(h)):
// Blueprint несёт map FQDN→token, cloud-init запекает их и добавляет фазу
// `soul init` (токен по hostname) между установкой бинаря и `soul run`.
func TestRenderCloudInitYAML_SelfOnboard(t *testing.T) {
	bp := validBlueprint()
	bp.SelfOnboardTokens = map[string]string{
		"redis-0.fedorovstepan2-dev.vm.xc.clv3": "TOKEN-AAA",
		"redis-1.fedorovstepan2-dev.vm.xc.clv3": "TOKEN-BBB",
	}
	out, err := soulinstall.RenderCloudInitYAML(bp)
	if err != nil {
		t.Fatalf("RenderCloudInitYAML self-onboard: %v", err)
	}

	// Токены и FQDN запечены (map доставлен на VM).
	for fqdn, tok := range bp.SelfOnboardTokens {
		if !strings.Contains(out, fqdn) {
			t.Errorf("self-onboard userdata missing FQDN %q", fqdn)
		}
		if !strings.Contains(out, tok) {
			t.Errorf("self-onboard userdata missing token for %q", fqdn)
		}
	}

	// Фаза soul init присутствует и токен выбирается по hostname (не хардкод).
	if !strings.Contains(out, "soul init") {
		t.Error("self-onboard userdata has no `soul init` phase")
	}
	if !strings.Contains(out, "hostname") {
		t.Error("self-onboard must select token by hostname (no hostname reference found)")
	}
	// init ДО `soul run`/systemd start: соберём индексы и сверим порядок.
	initIdx := strings.Index(out, "soul init")
	startIdx := strings.Index(out, "systemctl start soul")
	if initIdx < 0 || startIdx < 0 {
		t.Fatalf("both soul-init and systemctl-start must be present (init=%d start=%d)", initIdx, startIdx)
	}
	if initIdx > startIdx {
		t.Errorf("`soul init` (%d) must run BEFORE `systemctl start soul` (%d)", initIdx, startIdx)
	}

	// Токен НЕ в argv `soul init` (env SOUL_BOOTSTRAP_TOKEN, не --token=<plain>):
	// argv виден в ps/journald на VM. Проверяем, что нет `--token=TOKEN-`.
	if strings.Contains(out, "--token=TOKEN-") {
		t.Errorf("self-onboard leaks token into `soul init --token=` argv (use env SOUL_BOOTSTRAP_TOKEN)")
	}

	// Валидный YAML.
	var v map[string]any
	if err := yaml.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("self-onboard userdata is not valid YAML: %v", err)
	}
}

// TestRenderCloudInitYAML_SecurityGuard_BlocksTokenWithoutSelfOnboard — без
// self-onboard security-floor сохранён: подстрока bootstrap_token в userdata
// (например, случайно протёкшая через Blueprint) валит рендер. Guard снимается
// ТОЛЬКО в self-onboard-режиме (где токены в userdata — намеренно, тест-стенд).
func TestRenderCloudInitYAML_SecurityGuard_BlocksTokenWithoutSelfOnboard(t *testing.T) {
	bp := validBlueprint()
	// Протёкший токен в поле, попадающем в userdata (SoulVersion идёт комментарием).
	bp.SoulVersion = "v1 bootstrap_token=leak"
	if _, err := soulinstall.RenderCloudInitYAML(bp); err == nil {
		t.Fatal("security guard must reject bootstrap_token substring when NOT self-onboard")
	}
}

// TestRenderCloudInitYAML_SelfOnboard_RejectsVaultRef — даже в self-onboard
// vault-ref в userdata остаётся запрещён (секреты резолвятся ДО рендера; только
// bootstrap-токены легитимны в self-onboard-userdata, не vault-refs).
func TestRenderCloudInitYAML_SelfOnboard_RejectsVaultRef(t *testing.T) {
	bp := validBlueprint()
	bp.SelfOnboardTokens = map[string]string{"h0.example.com": "TOK"}
	bp.SoulVersion = "vault:secret/leak"
	if _, err := soulinstall.RenderCloudInitYAML(bp); err == nil {
		t.Fatal("vault-ref must be rejected even in self-onboard mode")
	}
}

// firstStepCmdContains возвращает индекс первого шага, .Cmd которого содержит sub,
// или -1.
func firstStepCmdContains(steps []soulinstall.InstallStep, sub string) int {
	for i, s := range steps {
		if strings.Contains(s.Cmd, sub) {
			return i
		}
	}
	return -1
}

func stepCmdContainsAny(steps []soulinstall.InstallStep, sub string) bool {
	return firstStepCmdContains(steps, sub) >= 0
}

// binaryCurlStep возвращает .Cmd шага, скачивающего soul-бинарь (curl + путь
// бинаря). Изолирует assert от других шагов.
func binaryCurlStep(t *testing.T, steps []soulinstall.InstallStep) string {
	t.Helper()
	for _, s := range steps {
		if strings.Contains(s.Cmd, "curl") && strings.Contains(s.Cmd, soulinstall.SoulBinaryPath) {
			return s.Cmd
		}
	}
	t.Fatalf("no binary-download curl step found in install script")
	return ""
}
