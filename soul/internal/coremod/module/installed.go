package module

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"

	"github.com/souls-guild/soul-stack/shared/diag"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
)

// TaskError-reasons install-шага (открытый каталог, naming-rules → Error codes;
// едут префиксом в message финального failed-события — прецедент
// errand_module_not_allowed).
const (
	reasonNotAllowed   = "module_not_allowed"
	reasonFetchFailed  = "module_fetch_failed"
	reasonVerifyFailed = "module_verify_failed"
)

// applyInstalled реализует state `installed` (ADR-065(c,f,g)).
//
// Нормативный порядок: allow-check ДО fetch → идемпотентность по sha256 →
// fetch по content-адресу → полный Sigil-verify ДО материализации → atomic
// install в каталожный слот `<paths.modules>/<ns>-<name>/`.
func (m *Module) applyInstalled(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest) error {
	fullName, err := util.StringParam(req.GetParams(), "name")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	pin, err := util.OptStringParam(req.GetParams(), "ref")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	namespace, name, ok := splitFullName(fullName)
	if !ok {
		return util.SendFailed(stream, fmt.Sprintf("param %q: expected \"<namespace>.<name>\", got %q", "name", fullName))
	}
	if m.deps.ModulesRoot == "" {
		return util.SendFailed(stream, "paths.modules не задан в soul.yml — кешу модулей некуда материализоваться")
	}

	// (1) allow-check ДО единого сетевого байта.
	var rec *sharedhost.SigilRecord
	if m.deps.Sigils != nil {
		rec = m.deps.Sigils.Get(namespace, name)
	}
	if rec == nil {
		return util.SendFailed(stream, fmt.Sprintf(
			"%s: нет активного Sigil-допуска для %s (kind: soul_module); выполните `keeper.plugin.allow ns=%s name=%s ref=<ref>`",
			reasonNotAllowed, fullName, namespace, name))
	}
	if pin != "" && rec.Ref != pin {
		return util.SendFailed(stream, fmt.Sprintf(
			"%s: активный допуск %s на ref %q, задача ожидает ref %q (pin-сверка, ADR-065)",
			reasonNotAllowed, fullName, rec.Ref, pin))
	}
	manifest, diags := sharedplugin.LoadFromBytes(sharedplugin.FileName, rec.Manifest)
	if diag.HasErrors(diags) || manifest.Kind != sharedplugin.KindSoulModule {
		return util.SendFailed(stream, fmt.Sprintf(
			"%s: допуск %s не подтверждает kind: soul_module (manifest допуска битый или иного kind)",
			reasonNotAllowed, fullName))
	}

	slotDir := filepath.Join(m.deps.ModulesRoot, namespace+"-"+name)
	binPath := filepath.Join(slotDir, manifest.BinaryName())

	// (2) идемпотентность: установленный бинарь уже совпадает с активным допуском.
	if diskSHA, exists := sha256OfFile(binPath); exists && strings.EqualFold(diskSHA, rec.BinarySHA256hex) {
		return sendInstalled(stream, false, fullName, rec, binPath)
	}

	// (3) fetch по content-адресу через FetchModule текущей EventStream-сессии.
	fetcher, ok := fetcherFrom(stream.Context())
	if !ok {
		return util.SendFailed(stream, fmt.Sprintf(
			"%s: FetchModule недоступен в этом прогоне (нет EventStream-сессии; push-режим не поддержан)", reasonFetchFailed))
	}
	data, err := fetchAll(stream.Context(), fetcher, namespace, name, rec.BinarySHA256hex)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("%s: %s: %v", reasonFetchFailed, fullName, err))
	}

	// (4) полный Sigil-verify ДО материализации: sha256 байтов == допуск +
	// подпись + manifest-хеш (shared/pluginhost, ADR-065(f)).
	if err := sharedhost.VerifyArtifactBytes(data, rec, m.deps.Anchors); err != nil {
		return util.SendFailed(stream, fmt.Sprintf("%s: %s: %v", reasonVerifyFailed, fullName, err))
	}

	// (5) atomic install: manifest из manifest_raw допуска (НЕ из fetch) →
	// сброс digest-sidecar-а предыдущего бинаря → atomic rename бинаря.
	if err := installSlot(slotDir, binPath, rec.Manifest, data); err != nil {
		return util.SendFailed(stream, fmt.Sprintf("install %s: %v", fullName, err))
	}

	// (6) hot-register (ADR-065(d)) — только при реальной установке.
	if m.deps.Rescan != nil {
		m.deps.Rescan()
	}

	return sendInstalled(stream, true, fullName, rec, binPath)
}

// fetchAll собирает байты бинаря из server-streaming PluginChunk-ответа.
func fetchAll(ctx context.Context, fetcher Fetcher, namespace, name, sha string) ([]byte, error) {
	stream, err := fetcher.FetchModule(ctx, &keeperv1.PluginFetchRequest{
		Namespace:    namespace,
		Name:         name,
		BinarySha256: sha,
	})
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	for {
		chunk, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			return buf.Bytes(), nil
		}
		if rerr != nil {
			return nil, rerr
		}
		buf.Write(chunk.GetData())
	}
}

// installSlot материализует слот: manifest.yaml + бинарь, оба через atomic
// rename (util.AtomicWrite). Digest-sidecar прежнего бинаря удаляется ДО
// rename нового — иначе Spawn fail-closed-ил бы свежеустановленный бинарь по
// stale-sidecar-у (см. shared/pluginhost verifySigilAndSeal).
func installSlot(slotDir, binPath string, manifestRaw, binData []byte) error {
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		return err
	}
	if err := util.AtomicWrite(filepath.Join(slotDir, sharedplugin.FileName), manifestRaw, 0o644); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(slotDir, sharedhost.DigestSidecarName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return util.AtomicWrite(binPath, binData, 0o755)
}

// sha256OfFile — hex-digest файла; exists=false при отсутствии или любой ошибке
// чтения (перезапись через atomic rename исправит нечитаемый слот).
func sha256OfFile(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

func sendInstalled(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], changed bool, fullName string, rec *sharedhost.SigilRecord, binPath string) error {
	return util.SendFinal(stream, changed, map[string]any{
		"name":      fullName,
		"ref":       rec.Ref,
		"sha256":    rec.BinarySHA256hex,
		"path":      binPath,
		"installed": true,
		"changed":   changed,
	})
}
