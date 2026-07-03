// Package soulinstall — canonical install-blueprint для разворачивания
// soul-агента на свежей VM. Единый источник правды для двух путей доставки:
//
//   - cloud-init userdata (B-flat, [ADR-017(h)](../../../docs/adr/0017-keeper-side-core.md)):
//     [RenderCloudInitYAML] печатает cloud-config YAML, провайдер кладёт в
//     metadata VM при Create. Используется `core.cloud.created`.
//   - full-install по SSH (Teleport, [ADR-063 amendment](../../../docs/adr/0063-bootstrap-token-delivery.md)):
//     [RenderInstallScript] выдаёт последовательность SSH-команд для платформ
//     без cloud-init userdata (напр. WB namespace без `ci_user_data`). Секреты
//     (CA, soul.yml) идут через STDIN, не argv. ПОКА фундамент — вызовётся в
//     Слайсе 2 (install-режим `core.bootstrap.delivered`).
//
// Blueprint описывает ОДИН и тот же install-результат: те же файлы по тем же
// путям с теми же правами (константы ниже), тот же soul.yml и systemd-unit.
// Истинный единый источник: содержимое soul.yml/unit задают функции
// SoulConfigYAML/SystemdUnit, а cloud-init-шаблон рендерит их через
// {{ .SoulConfigYAMLIndented }} / {{ .SystemdUnitIndented }} (не текстовая
// копия) — оба рендерера физически берут один и тот же материал, drift
// невозможен. Единственное намеренное расхождение между путями — права
// keeper-ca.pem: 0600 при SSH-install (floor построже) vs 0644 в cloud-init
// (CA публичен); см. KeeperCAMode.
//
// ★ Per-VM bootstrap-токен blueprint НЕ несёт (ни в одном рендерере): userdata
// логируется провайдером (security floor), токен — отдельный шаг scenario
// (см. ADR-017(h) B-flat). RenderInstallScript НЕ включает token-write и
// `systemctl start` — это добавляет delivered-режим в Слайсе 2.
package soulinstall

import (
	"embed"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"text/template"
)

//go:embed templates/cloud-init.tmpl
var templatesFS embed.FS

// parsedTemplate — один раз распарсенный cloud-init template из embed.FS.
// parse-time проверяется тестом happy-path; ошибка тут — bug в коде
// (template byte-compiled из go:embed), а не runtime-issue.
var parsedTemplate = template.Must(
	template.New("cloud-init.tmpl").ParseFS(templatesFS, "templates/cloud-init.tmpl"),
)

// Канонические пути и права install-результата — единый источник для обоих
// рендереров. RenderInstallScript собирает install-команды по этим константам
// напрямую; cloud-init.tmpl держит пути в write_files/runcmd, а тело файлов
// (soul.yml/unit) рендерит из SoulConfigYAML/SystemdUnit (см. RenderCloudInitYAML).
const (
	// TLSDir — каталог TLS-материала soul-агента на VM.
	TLSDir = "/etc/soul/tls"
	// KeeperCAPath — PEM CA Keeper-а (pin Bootstrap-канала souls↔keeper mTLS).
	KeeperCAPath = "/etc/soul/tls/keeper-ca.pem"
	// SoulConfigPath — минимальный конфиг soul-агента (keeper.endpoints + ca).
	SoulConfigPath = "/etc/soul/soul.yml"
	// SoulServicePath — systemd-unit soul-агента.
	SoulServicePath = "/etc/systemd/system/soul.service"
	// SoulBinaryPath — путь установки soul-бинаря.
	SoulBinaryPath = "/usr/local/bin/soul"
	// SeedCertPath — cert.pem активного SoulSeed на VM: <paths.seed из
	// SoulConfigYAML>/current/cert.pem (layout soul/internal/seed, симлинк
	// `current` + CertFile). Существование файла = токен уже redeem-нут — guard
	// идемпотентности `soul init` в core.bootstrap.delivered (токен single-use).
	// Sync-guard: TestSeedCertPath_SyncWithSoulSeedLayout.
	SeedCertPath = "/var/lib/soul-stack/seed/current/cert.pem"
	// SelfOnboardTokensPath — файл map FQDN→token на VM (self-onboard Вариант T).
	// cloud-init выбирает свой токен по hostname и делает `soul init`. Права 0600
	// (несёт секреты, тест-стенд). См. Blueprint.SelfOnboardTokens.
	SelfOnboardTokensPath = "/etc/soul/self-onboard-tokens"

	// KeeperCAMode — права keeper-ca.pem при SSH-install (0600; в cloud-init
	// тот же файл пишется write_files-ом с 0644 — оба варианта приватнее не
	// требуются: CA публичен, но SSH-install ставит floor построже).
	KeeperCAMode = "0600"
	// SoulBinaryMode — права soul-бинаря (исполняемый).
	SoulBinaryMode = "0755"
)

// Значения SoulBinaryCA — какой trust-store использует curl при скачивании
// soul-бинаря. Пустое значение трактуется как SoulBinaryCAKeeper (back-compat
// secure-default). Ослабляет ТОЛЬКО верификацию cert artifact-хоста; Bootstrap-
// канал (souls↔keeper mTLS) и SHA256-verify бинаря не затрагиваются.
const (
	// SoulBinaryCAKeeper — pin на PEM-CA Keeper-а (`curl --cacert keeper-ca.pem`).
	SoulBinaryCAKeeper = "keeper"
	// SoulBinaryCASystem — OS-trust bundle (`curl` без `--cacert`); для artifact-
	// хостов с публичным CA (например, Nexus за GlobalSign).
	SoulBinaryCASystem = "system"
)

// Blueprint — резолвленные параметры install-результата (cloud-init или SSH).
// Собирается из [shared/config.KeeperCloudInit] на каждом рендер-вызове
// (hot-reload-friendly). Источник полей — один блок keeper.yml::cloud_init,
// общий для обоих путей доставки (config-reuse, DRY).
type Blueprint struct {
	// BootstrapEndpoint — `host:port` LB Keeper-а (Bootstrap-RPC listener).
	// host идёт в soul.yml keeper.endpoints[0].host, port — в bootstrap_port.
	BootstrapEndpoint string

	// EventStreamPort — TCP-порт EventStream-фазы (mTLS-listener) того же
	// host-а; идёт в soul.yml event_stream_port. 0 → порт bootstrap_endpoint
	// (back-compat: single-port LB). См. 6-ю стену ADR-063: оба порта из одного
	// endpoint → soul dial-ил EventStream на Bootstrap-порт.
	EventStreamPort int

	// KeeperCAPem — PEM-encoded CA Keeper-а (содержимое `ca`-поля из Vault KV).
	// Пишется на VM по [KeeperCAPath]; затем curl --cacert использует его при
	// скачивании soul-бинаря (в режиме SoulBinaryCAKeeper).
	KeeperCAPem string

	// SoulBinaryURL — HTTPS URL для скачивания `soul`-бинаря. Plain http
	// отвергается (security: только над TLS, независимо от SoulBinaryCA).
	SoulBinaryURL string

	// SoulBinaryCA — trust-store для curl при скачивании бинаря:
	// SoulBinaryCAKeeper (default/пусто) → `--cacert keeper-ca.pem`;
	// SoulBinaryCASystem → системный bundle (без `--cacert`, для публичных CA).
	// Ослабляет ТОЛЬКО верификацию cert artifact-хоста; Bootstrap-канал и
	// SHA256-verify бинаря не затрагиваются.
	SoulBinaryCA string

	// SoulVersion — опц. строка-метка (для диагностики). В cloud-init попадает
	// комментарием. Sig-verify бинаря отложен (ADR-017(h) amendment).
	SoulVersion string

	// SelfOnboardTokens — map FQDN→plain-bootstrap-token для self-onboard
	// «Вариант T» (ADR-017(h) amendment). Непустой → cloud-init запекает эти
	// токены в userdata (общий blob) и добавляет фазу `soul init` (токен
	// выбирается по hostname VM), между установкой бинаря и `soul run`. VM сама
	// онбордится в один цикл cloud-init, без claim-callback и без keeper.push.
	//
	// ★ БЕЗОПАСНОСТЬ (тест-стенд): в этом режиме plain-токены попадают в userdata,
	// который cloud-провайдер хранит в plaintext metadata. Это осознанный компромисс
	// self-onboard тест-стенда (снимает security-guard `bootstrap_token` — см.
	// RenderCloudInitYAML). TODO(prod): для прода вернуть late-binding claim (Вариант
	// C) или per-VM userdata (individual blob на VM вместо общего map).
	//
	// Пустой/nil → обычный B-flat рендер (токенов в userdata нет, guard активен).
	SelfOnboardTokens map[string]string
}

// selfOnboard — режим self-onboard (Blueprint несёт per-VM токены).
func (b Blueprint) selfOnboard() bool { return len(b.SelfOnboardTokens) > 0 }

// Validate проверяет, что Blueprint заполнен достаточно для рендера. Возвращает
// первую найденную ошибку с понятным сообщением (несколько ошибок поднимать не
// нужно: config-фаза уже отловила format-issues, тут — runtime-precondition).
func (b Blueprint) Validate() error {
	if b.BootstrapEndpoint == "" {
		return errors.New("cloud_init.bootstrap_endpoint is empty")
	}
	if _, _, err := net.SplitHostPort(b.BootstrapEndpoint); err != nil {
		return fmt.Errorf("cloud_init.bootstrap_endpoint %q is not host:port: %w", b.BootstrapEndpoint, err)
	}
	if !strings.Contains(b.KeeperCAPem, "BEGIN CERTIFICATE") {
		return errors.New("cloud_init.tls_ca is not a PEM-encoded certificate")
	}
	if b.SoulBinaryURL == "" {
		return errors.New("cloud_init.soul_binary_url is empty")
	}
	if !strings.HasPrefix(b.SoulBinaryURL, "https://") {
		return fmt.Errorf("cloud_init.soul_binary_url %q must be https (CA over TLS only, plain http rejected)", b.SoulBinaryURL)
	}
	if b.EventStreamPort < 0 || b.EventStreamPort > 65535 {
		return fmt.Errorf("cloud_init.event_stream_port %d must be in 1..65535 (0 = порт bootstrap_endpoint)", b.EventStreamPort)
	}
	switch b.SoulBinaryCA {
	case "", SoulBinaryCAKeeper, SoulBinaryCASystem:
	default:
		return fmt.Errorf("cloud_init.soul_binary_ca %q must be %q or %q (empty defaults to %q)",
			b.SoulBinaryCA, SoulBinaryCAKeeper, SoulBinaryCASystem, SoulBinaryCAKeeper)
	}
	return nil
}

// useSystemCA — curl без `--cacert` (системный trust-store). Влияет ТОЛЬКО на
// скачивание бинаря; keeper-ca.pem всё равно пишется на VM — Bootstrap-канал
// (souls↔keeper mTLS) пинится на keeper-CA всегда.
func (b Blueprint) useSystemCA() bool {
	return b.SoulBinaryCA == SoulBinaryCASystem
}

// hostPort разбирает BootstrapEndpoint в host + валидированный TCP-порт.
// Вызывается после Validate (формат уже проверен), но port-range пере-проверяет.
func (b Blueprint) hostPort() (string, int, error) {
	host, portStr, _ := net.SplitHostPort(b.BootstrapEndpoint)
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("cloud_init.bootstrap_endpoint %q: port not valid: %v", b.BootstrapEndpoint, err)
	}
	return host, port, nil
}

// soulEndpoint — host + пара портов для soul.yml. EventStreamPort==0 →
// fallback на bootstrap-порт (back-compat: single-port LB).
func (b Blueprint) soulEndpoint() (host string, eventPort, bootstrapPort int, err error) {
	host, bootstrapPort, err = b.hostPort()
	if err != nil {
		return "", 0, 0, err
	}
	eventPort = b.EventStreamPort
	if eventPort == 0 {
		eventPort = bootstrapPort
	}
	return host, eventPort, bootstrapPort, nil
}

// RenderCloudInitYAML рендерит cloud-config YAML по cloud-init template.
// Идемпотентна: на тех же входах даёт байт-идентичный вывод.
//
// Безопасность: вывод проверяется на отсутствие подстроки `bootstrap_token` /
// `vault:` — защита от случайной утечки секретов из template-а (например, если
// в Blueprint попадёт строка с такой подстрокой). Это инвариант для всех
// каналов, куда уходит userdata (cloud-provider metadata, audit-payload, OTel).
func RenderCloudInitYAML(bp Blueprint) (string, error) {
	if err := bp.Validate(); err != nil {
		return "", err
	}
	host, eventPort, bootstrapPort, err := bp.soulEndpoint()
	if err != nil {
		return "", err
	}

	// Тело soul.yml и systemd-unit рендерятся ИЗ тех же Go-функций
	// (SoulConfigYAML/SystemdUnit), что использует RenderInstallScript, —
	// единый источник правды без текстового дубля в шаблоне. Indent 6 пробелов
	// кладёт блок под YAML-ключ `content: |` (как indentBlock для CA PEM).
	view := struct {
		TLSCAPemIndented          string
		SoulConfigYAMLIndented    string
		SystemdUnitIndented       string
		SoulBinaryURL             string
		UseSystemCA               bool
		SoulVersion               string
		SelfOnboard               bool
		SelfOnboardTokensIndented string
	}{
		TLSCAPemIndented:       indentBlock(bp.KeeperCAPem, "      "),
		SoulConfigYAMLIndented: indentBlock(SoulConfigYAML(host, eventPort, bootstrapPort), "      "),
		SystemdUnitIndented:    indentBlock(SystemdUnit(), "      "),
		SoulBinaryURL:          bp.SoulBinaryURL,
		UseSystemCA:            bp.useSystemCA(),
		SoulVersion:            bp.SoulVersion,
		SelfOnboard:            bp.selfOnboard(),
	}
	if bp.selfOnboard() {
		view.SelfOnboardTokensIndented = indentBlock(selfOnboardTokensFile(bp.SelfOnboardTokens), "      ")
	}

	var sb strings.Builder
	if err := parsedTemplate.Execute(&sb, view); err != nil {
		return "", fmt.Errorf("cloud-init render: %w", err)
	}
	out := sb.String()

	// Security floor: userdata НЕ должен нести vault-ref (секреты резолвятся ДО
	// рендера) — держится ВСЕГДА, включая self-onboard (в userdata легитимны
	// только bootstrap-токены, но не vault-пути).
	if strings.Contains(out, "vault:") {
		return "", errors.New("cloud-init render: output contains 'vault:' substring — vault-refs must be resolved before render")
	}
	// Security floor `bootstrap_token`: userdata НЕ должен нести токены (B-flat).
	// ★ СНИМАЕТСЯ в self-onboard-режиме (Вариант T, тест-стенд): там per-VM токены
	// в userdata — намеренно (VM онбордится в один цикл cloud-init). Вне
	// self-onboard guard активен как раньше (защита от случайной утечки).
	// TODO(prod): вернуть guard для прода (late-binding claim / per-VM userdata).
	if !bp.selfOnboard() && strings.Contains(out, "bootstrap_token") {
		return "", errors.New("cloud-init render: output contains 'bootstrap_token' substring — userdata must not carry per-VM tokens (B-flat invariant)")
	}
	return out, nil
}

// InstallStep — одна SSH-команда install-последовательности. Cmd — строка для
// `session.Run` (попадёт в argv процесса на VM); Stdin — данные, скармливаемые
// процессу через stdin (nil — без stdin). Секреты ОБЯЗАНЫ идти через Stdin, а
// НЕ через Cmd — argv виден в `ps`/audit.log/journald на самой VM (ADR-063 §A1).
type InstallStep struct {
	Cmd   string
	Stdin []byte
}

// RenderInstallScript выдаёт последовательность SSH-команд, ставящих тот же
// install-результат, что [RenderCloudInitYAML], для платформ без cloud-init
// userdata (full-install по SSH, ADR-063 amendment). Порядок:
//
//  1. install -d каталоги (TLS-dir 0600 + soul-state-каталоги).
//  2. cat > keeper-ca.pem (Stdin=CA PEM) + chmod 0600.
//  3. cat > soul.yml (Stdin=soul.yml-контент).
//  4. cat > soul.service (Stdin=systemd-unit).
//  5. curl soul-бинарь (--cacert keeper-ca.pem в keeper-режиме, без — в system)
//     + chmod 0755.
//
// ★ НЕ включает token-write и `systemctl start` — это добавляет delivered-режим
// (Слайс 2). ★ CA и soul.yml идут через Stdin (не argv) — secret-write floor.
// ПОКА никем не вызывается: фундамент под Слайс 2.
func RenderInstallScript(bp Blueprint) ([]InstallStep, error) {
	if err := bp.Validate(); err != nil {
		return nil, err
	}
	host, eventPort, bootstrapPort, err := bp.soulEndpoint()
	if err != nil {
		return nil, err
	}

	steps := []InstallStep{
		{Cmd: fmt.Sprintf(
			"install -d -m %s %s && install -d -m 0755 /etc/soul /var/lib/soul-stack /var/lib/soul-stack/modules /var/lib/soul-stack/seed",
			KeeperCAMode, TLSDir)},
		{Cmd: fmt.Sprintf("umask 077 && cat > %s && chmod %s %s", KeeperCAPath, KeeperCAMode, KeeperCAPath),
			Stdin: []byte(bp.KeeperCAPem)},
		{Cmd: fmt.Sprintf("cat > %s", SoulConfigPath),
			Stdin: []byte(SoulConfigYAML(host, eventPort, bootstrapPort))},
		{Cmd: fmt.Sprintf("cat > %s", SoulServicePath),
			Stdin: []byte(SystemdUnit())},
		{Cmd: binaryCurlCmd(bp.SoulBinaryURL, bp.useSystemCA())},
		{Cmd: fmt.Sprintf("chmod %s %s", SoulBinaryMode, SoulBinaryPath)},
	}
	return steps, nil
}

// binaryCurlCmd — curl-команда скачивания soul-бинаря. В keeper-режиме пинится
// на keeper-CA (`--cacert`); в system-режиме — системный trust-store (без
// `--cacert`). Та же семантика, что runcmd cloud-init-шаблона.
func binaryCurlCmd(url string, systemCA bool) string {
	if systemCA {
		return fmt.Sprintf("curl --fail --show-error --silent --output %s %s", SoulBinaryPath, url)
	}
	return fmt.Sprintf("curl --fail --show-error --silent --cacert %s --output %s %s", KeeperCAPath, SoulBinaryPath, url)
}

// selfOnboardTokensFile сериализует map FQDN→token в построчный формат
// `<fqdn> <token>` (self-onboard Вариант T). Формат grep/awk-friendly: cloud-init
// выбирает свою строку по `$(hostname -f)` первым полем. FQDN отсортированы —
// байт-стабильность рендера (map iteration в Go недетерминирован). Токены
// base64url без пробелов (bootstraptoken.Generate), поэтому split по пробелу
// однозначен.
func selfOnboardTokensFile(tokens map[string]string) string {
	fqdns := make([]string, 0, len(tokens))
	for fqdn := range tokens {
		fqdns = append(fqdns, fqdn)
	}
	sort.Strings(fqdns)
	var b strings.Builder
	for _, fqdn := range fqdns {
		b.WriteString(fqdn)
		b.WriteByte(' ')
		b.WriteString(tokens[fqdn])
		b.WriteByte('\n')
	}
	return b.String()
}

// SoulConfigYAML — содержимое soul.yml (минимальный конфиг soul-агента для
// bootstrap: keeper.endpoints + pin CA). Единый источник: эта же функция даёт
// тело soul.yml и для SSH-install (RenderInstallScript), и для cloud-init
// (RenderCloudInitYAML подставляет вывод под write_files /etc/soul/soul.yml).
// Порты фаз разные: eventStreamPort — EventStream (mTLS), bootstrapPort —
// Bootstrap-RPC (см. 6-ю стену ADR-063).
func SoulConfigYAML(host string, eventStreamPort, bootstrapPort int) string {
	return fmt.Sprintf(`# Минимальный конфиг soul-агента для cloud-init bootstrap.
# Per-VM-токен и SoulSeed-сертификат добавит следующий шаг scenario.
paths:
  modules: /var/lib/soul-stack/modules
  seed:    /var/lib/soul-stack/seed
keeper:
  endpoints:
    - host: %s
      event_stream_port: %d
      bootstrap_port: %d
  tls:
    ca: %s
`, host, eventStreamPort, bootstrapPort, KeeperCAPath)
}

// SystemdUnit — содержимое soul.service. Единый источник: эта же функция даёт
// тело unit-а и для SSH-install (RenderInstallScript), и для cloud-init
// (RenderCloudInitYAML подставляет вывод под write_files soul.service).
func SystemdUnit() string {
	return fmt.Sprintf(`[Unit]
Description=Soul Stack agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s run --config %s
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
`, SoulBinaryPath, SoulConfigPath)
}

// indentBlock сдвигает каждую строку текстового блока на `prefix`, чтобы он лёг
// под YAML-ключ с heredoc-style `content: |` (cloud-config). Используется для CA
// PEM, soul.yml и systemd-unit — все три кладутся в шаблон одним механизмом.
// Trailing newline отбрасывается (TrimRight), чтобы между блоком и следующим
// YAML-ключом был ровно один break, а не пустая строка со сдвигом.
func indentBlock(block, prefix string) string {
	lines := strings.Split(strings.TrimRight(block, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
