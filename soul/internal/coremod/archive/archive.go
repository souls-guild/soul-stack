// Package archive реализует core-модуль `core.archive` ([ADR-015]).
//
// Состояние:
//   - extracted: распаковать архив `path` в каталог `dest`.
//
// Поддерживаемые форматы: tar / tar.gz (.tgz) / tar.bz2 (.tbz2) / zip.
// `format` опционален — auto-detect по расширению. Распаковка делается
// in-process средствами Go stdlib (`archive/tar`, `archive/zip`,
// `compress/gzip`, `compress/bzip2`): без внешних утилит (`tar`/`unzip`) и без
// порождения подпроцессов. Это снимает зависимость от хостовых бинарей и даёт
// per-entry контроль безопасности (zip-slip / zip-bomb / symlink-политика),
// который backend-утилитам недоступен.
//
// Security-инварианты (жёсткие, не настраиваемые флагами):
//   - zip-slip: запись с `..` или абсолютным путём, выводящим за пределы
//     `dest`, → fail-fast (вся распаковка прерывается, НЕ тихий clamp).
//   - zip-bomb: суммарный распакованный размер ограничен `max_size`
//     (дефолт 1 GiB), число записей — `max_entries` (дефолт 100000), а
//     отношение распакованных байт к сжатым — `max_ratio` (дефолт 100).
//     Ratio-лимит ловит бомбу, обходящую max_size маленьким сжатым размером
//     (10 KiB → 10 GiB). Проверка fail-fast В ПРОЦЕССЕ распаковки.
//   - symlink: создаётся только если резолвнутый target остаётся внутри `dest`.
//   - setuid/setgid/sticky биты из архива всегда маскируются (anti-privesc).
//   - hardlink / devnode / char / fifo → ошибка (в MVP не поддерживаем).
//   - owner/group из архива не берутся — файлы получают владельца процесса soul.
//
// Idempotency: SHA-256 исходного архива записывается в `<dest>/.soul-archive.sha256`
// после распаковки. На повторных применениях, если хеш совпадает — no-op.
// Это grounded-проверка «архив тот же», а не «файлы внутри dest все на месте»
// (последнее требует полного walk + сравнение чексумм каждого файла — over-kill
// для MVP).
package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	securejoin "github.com/cyphar/filepath-securejoin"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

const Name = "core.archive"

// MarkerFile — имя файла-маркера для idempotency-проверки. Помещается в dest.
const MarkerFile = ".soul-archive.sha256"

// Дефолты zip-bomb-лимитов: суммарный распакованный размер, число записей и
// максимальное отношение распакованных байт к сжатым (compression ratio).
// Переопределяются params `max_size` / `max_entries` / `max_ratio`.
// max_ratio=0 отключает проверку (выбор пользователя — escape для легитимных
// высокосжимаемых архивов, текст/логи дают ratio в сотни).
const (
	defaultMaxSize    int64 = 1 << 30 // 1 GiB
	defaultMaxEntries int64 = 100000
	defaultMaxRatio   int64 = 100
)

type Module struct{}

func New() *Module { return &Module{} }

// Validate НЕ делегирован целиком в util.ValidateAgainstManifest (в отличие от
// core.exec): сверх known-state + required (которые manifest выражает) у
// core.archive есть enum-проверка `format` (tar|tar.gz|tar.bz2|zip) — её
// урезанный plugin.InputParamDef DSL не выражает (enum отложен, см. H1). Чтобы
// runtime ловил неизвестный format до распаковки, оставляем ручную форму.
// known-state/required дублируются с manifest осознанно — единый источник
// невозможен без enum-поддержки в DSL.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	if req.State != "extracted" {
		errs = append(errs, fmt.Sprintf("unknown state %q (want extracted)", req.State))
	}
	if _, err := util.StringParam(req.Params, "path"); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := util.StringParam(req.Params, "dest"); err != nil {
		errs = append(errs, err.Error())
	}
	if fmtStr, err := util.OptStringParam(req.Params, "format"); err == nil && fmtStr != "" {
		if !knownFormat(fmtStr) {
			errs = append(errs, fmt.Sprintf("param %q: unknown format %q (want tar|tar.gz|tar.bz2|zip)", "format", fmtStr))
		}
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// PlanReadSafe объявляет, что core.archive.Plan — pure-read (ADR-031 Scry):
// читает hashFile(src) + marker, НЕ распаковывает архив (маркер для host-а,
// default-deny).
func (m *Module) PlanReadSafe() {}

// Plan — pure-read dry-run (ADR-031 Scry): хэширует исходный архив (читающая
// операция) и сравнивает с marker-файлом в dest — тот же сравнительный read,
// что выполняет Apply ДО распаковки. НЕ мутирует: ни MkdirAll(dest), ни
// extract, ни запись marker.
//
// Семантика drift симметрична Apply-idempotency:
//   - drift=false, если marker существует и его содержимое == sha256(src);
//   - drift=true в любом другом случае (marker нет, sha не совпал, dest нет).
//
// Замечание о точности (ADR-015 «grounded-проверка»): marker-инвариант проверяет
// «архив тот же», а НЕ «все файлы внутри dest на месте» — это ограничение MVP,
// унаследованное от Apply. Plan честно отражает то же поведение: если файл из
// dest удалён руками, marker-инвариант clean, drift=false (Apply тоже не
// заметил бы). Полный walk dest+чексумминг каждого файла — отдельный slice.
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	if req.State != "extracted" {
		return util.PlanFailed(fmt.Sprintf("unknown state %q", req.State))
	}
	src, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	dest, err := util.StringParam(req.Params, "dest")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	format, err := util.OptStringParam(req.Params, "format")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	if format == "" {
		format = detectFormat(src)
		if format == "" {
			return util.PlanFailed(fmt.Sprintf("cannot auto-detect format for %q (use param `format`)", src))
		}
	}
	if !knownFormat(format) {
		return util.PlanFailed(fmt.Sprintf("unknown format %q", format))
	}

	srcHash, herr := hashFile(src)
	if herr != nil {
		return util.PlanFailed(herr.Error())
	}
	markerPath := filepath.Join(dest, MarkerFile)
	existing, rerr := os.ReadFile(markerPath)
	switch {
	case rerr == nil:
		return util.SendPlanFinal(stream, strings.TrimSpace(string(existing)) != srcHash)
	case errors.Is(rerr, fs.ErrNotExist):
		return util.SendPlanFinal(stream, true)
	default:
		return util.PlanFailed(fmt.Sprintf("read marker %s: %v", markerPath, rerr))
	}
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	if req.State != "extracted" {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
	src, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	dest, err := util.StringParam(req.Params, "dest")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	format, err := util.OptStringParam(req.Params, "format")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if format == "" {
		format = detectFormat(src)
		if format == "" {
			return util.SendFailed(stream, fmt.Sprintf("cannot auto-detect format for %q (use param `format`)", src))
		}
	}
	if !knownFormat(format) {
		return util.SendFailed(stream, fmt.Sprintf("unknown format %q", format))
	}

	limits, err := parseLimits(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	srcHash, err := hashFile(src)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	markerPath := filepath.Join(dest, MarkerFile)
	if existing, rerr := os.ReadFile(markerPath); rerr == nil {
		if strings.TrimSpace(string(existing)) == srcHash {
			return util.SendFinal(stream, false, map[string]any{
				"path":      src,
				"dest":      dest,
				"sha256":    srcHash,
				"extracted": true,
			})
		}
	} else if !errors.Is(rerr, fs.ErrNotExist) {
		return util.SendFailed(stream, fmt.Sprintf("read marker %s: %v", markerPath, rerr))
	}

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return util.SendFailed(stream, fmt.Sprintf("mkdir %s: %v", dest, err))
	}
	if err := extract(format, src, dest, limits); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if err := os.WriteFile(markerPath, []byte(srcHash+"\n"), 0o644); err != nil {
		return util.SendFailed(stream, fmt.Sprintf("write marker %s: %v", markerPath, err))
	}
	return util.SendFinal(stream, true, map[string]any{
		"path":      src,
		"dest":      dest,
		"sha256":    srcHash,
		"extracted": true,
	})
}

// limits — zip-bomb-бюджет распаковки. maxSize/maxEntries — абсолютные потолки;
// maxRatio — потолок отношения распакованных байт к сжатым (0 = выключено).
// usedSize/entries/compressed аккумулируются в ходе распаковки. compressed —
// число прочитанных СЖАТЫХ байт источника: для zip берётся per-entry из
// заголовка (CompressedSize64), для tar.gz/tar.bz2 — счётчиком на исходном
// потоке (countingReader); для голого tar остаётся 0 (ratio не проверяется —
// несжатый формат бомбой по ratio быть не может).
type limits struct {
	maxSize    int64
	maxEntries int64
	maxRatio   int64
	usedSize   int64
	entries    int64
	compressed int64
}

// countEntry учитывает очередную запись против maxEntries.
func (l *limits) countEntry() error {
	l.entries++
	if l.entries > l.maxEntries {
		return fmt.Errorf("archive: exceeded max_entries (%d)", l.maxEntries)
	}
	return nil
}

// addSize учитывает n распакованных байт против maxSize.
func (l *limits) addSize(n int64) error {
	l.usedSize += n
	if l.usedSize > l.maxSize {
		return fmt.Errorf("archive: exceeded max_size (%d bytes)", l.maxSize)
	}
	return nil
}

// addCompressed учитывает n прочитанных СЖАТЫХ байт (per-entry для zip).
func (l *limits) addCompressed(n int64) {
	l.compressed += n
}

// checkRatio проверяет отношение распакованных байт к сжатым против maxRatio.
// Вызывается ПОСЛЕ учёта очередного файла (usedSize и compressed обновлены) —
// так превышение ловится в процессе, до конца распаковки. maxRatio=0 — проверка
// выключена. compressed=0 при ненулевом usedSize (голый tar или ещё не прочитан
// ни один сжатый байт) трактуется как «ratio неопределён» → пропуск: голый tar
// несжат, бомбой по ratio не бывает, а max_size его и так ограничивает.
func (l *limits) checkRatio() error {
	if l.maxRatio == 0 || l.compressed == 0 {
		return nil
	}
	if l.usedSize > l.compressed*l.maxRatio {
		return fmt.Errorf("archive: exceeded max_ratio (%d): %d uncompressed / %d compressed bytes",
			l.maxRatio, l.usedSize, l.compressed)
	}
	return nil
}

func parseLimits(params *structpb.Struct) (*limits, error) {
	maxSizeStr, err := util.OptStringParam(params, "max_size")
	if err != nil {
		return nil, err
	}
	l := &limits{maxSize: defaultMaxSize, maxEntries: defaultMaxEntries, maxRatio: defaultMaxRatio}
	if maxSizeStr != "" {
		n, perr := parseSize(maxSizeStr)
		if perr != nil {
			return nil, perr
		}
		if n <= 0 {
			return nil, fmt.Errorf("param %q: must be positive, got %d", "max_size", n)
		}
		l.maxSize = n
	}
	if n, present, ierr := util.OptIntParam(params, "max_entries"); ierr != nil {
		return nil, ierr
	} else if present {
		if n <= 0 {
			return nil, fmt.Errorf("param %q: must be positive, got %d", "max_entries", n)
		}
		l.maxEntries = n
	}
	// max_ratio: 0 разрешён (disable), отрицательное — ошибка конфигурации.
	if n, present, ierr := util.OptIntParam(params, "max_ratio"); ierr != nil {
		return nil, ierr
	} else if present {
		if n < 0 {
			return nil, fmt.Errorf("param %q: must be >= 0 (0 disables), got %d", "max_ratio", n)
		}
		l.maxRatio = n
	}
	return l, nil
}

// countingReader оборачивает сжатый поток источника и аккумулирует число
// фактически прочитанных СЖАТЫХ байт в lim.compressed. Для tar.gz/tar.bz2 это
// единственный способ узнать compressed-размер инкрементально (per-entry его в
// потоке нет) — gzip/bzip2-декодер читает ровно столько сжатого, сколько нужно
// для уже выданных распакованных байт, поэтому ratio проверяется в процессе.
type countingReader struct {
	r   io.Reader
	lim *limits
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.lim.addCompressed(int64(n))
	return n, err
}

func extract(format, src, dest string, lim *limits) error {
	switch format {
	case "tar":
		f, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("open %s: %v", src, err)
		}
		defer f.Close()
		return extractTar(f, dest, lim)
	case "tar.gz", "tgz":
		f, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("open %s: %v", src, err)
		}
		defer f.Close()
		gz, err := gzip.NewReader(&countingReader{r: f, lim: lim})
		if err != nil {
			return fmt.Errorf("archive: gzip %s: %v", src, err)
		}
		defer gz.Close()
		return extractTar(gz, dest, lim)
	case "tar.bz2", "tbz2":
		f, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("open %s: %v", src, err)
		}
		defer f.Close()
		return extractTar(bzip2.NewReader(&countingReader{r: f, lim: lim}), dest, lim)
	case "zip":
		return extractZip(src, dest, lim)
	default:
		return fmt.Errorf("archive: unknown format %q", format)
	}
}

func extractTar(r io.Reader, dest string, lim *limits) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("archive: read tar: %v", err)
		}
		if err := lim.countEntry(); err != nil {
			return err
		}
		target, err := resolveEntry(dest, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, dirMode(fs.FileMode(hdr.Mode))); err != nil {
				return fmt.Errorf("archive: mkdir %s: %v", target, err)
			}
		case tar.TypeReg:
			if err := writeFileEntry(target, tr, safeFileMode(fs.FileMode(hdr.Mode)), lim); err != nil {
				return err
			}
			if err := lim.checkRatio(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := writeSymlink(dest, target, hdr.Linkname); err != nil {
				return err
			}
		case tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			return fmt.Errorf("archive: entry %q: unsupported type", hdr.Name)
		default:
			// PAX/GNU служебные заголовки (x/g/sparse-метаданные): не несут
			// файлов, молча пропускаем — состав извлечённого дерева не меняют.
			continue
		}
	}
}

func extractZip(src, dest string, lim *limits) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("archive: open zip %s: %v", src, err)
	}
	defer zr.Close()
	for _, zf := range zr.File {
		if err := lim.countEntry(); err != nil {
			return err
		}
		target, err := resolveEntry(dest, zf.Name)
		if err != nil {
			return err
		}
		mode := zf.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(target, dirMode(mode)); err != nil {
				return fmt.Errorf("archive: mkdir %s: %v", target, err)
			}
		case mode&fs.ModeSymlink != 0:
			link, rerr := readZipSymlink(zf)
			if rerr != nil {
				return rerr
			}
			if err := writeSymlink(dest, target, link); err != nil {
				return err
			}
		case mode&fs.ModeNamedPipe != 0, mode&fs.ModeDevice != 0,
			mode&fs.ModeCharDevice != 0, mode&fs.ModeSocket != 0:
			return fmt.Errorf("archive: entry %q: unsupported type", zf.Name)
		default:
			rc, oerr := zf.Open()
			if oerr != nil {
				return fmt.Errorf("archive: open zip entry %q: %v", zf.Name, oerr)
			}
			werr := writeFileEntry(target, rc, safeFileMode(mode), lim)
			rc.Close()
			if werr != nil {
				return werr
			}
			// compressed-размер записи известен из заголовка zip (per-entry) —
			// учитываем и проверяем ratio fail-fast после каждого файла.
			lim.addCompressed(int64(zf.CompressedSize64))
			if err := lim.checkRatio(); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolveEntry строит безопасный путь записи внутри dest и FAIL-FAST падает,
// если запись выводит за пределы dest (`..` / абсолютный путь). SecureJoin
// возвращает clamped-путь (молча зажимает escape) — поэтому на clamp не
// полагаемся: детектируем escape лексически (выбор пользователя — явная ошибка,
// не тихий clamp). Абсолютный путь Join схлопывает невидимо, поэтому ловится
// отдельно через IsAbs; `..` — через выход результата за пределы dest.
func resolveEntry(dest, name string) (string, error) {
	clean := filepath.ToSlash(name)
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("archive: entry %q escapes dest", name)
	}
	naive := filepath.Clean(filepath.Join(dest, clean))
	if !withinDest(dest, naive) {
		return "", fmt.Errorf("archive: entry %q escapes dest", name)
	}
	joined, err := securejoin.SecureJoin(dest, name)
	if err != nil || joined != naive {
		return "", fmt.Errorf("archive: entry %q escapes dest", name)
	}
	return joined, nil
}

// writeSymlink создаёт symlink target → link, только если резолвнутый link
// остаётся внутри dest (within-dest политика). target — уже безопасный путь
// самого symlink-файла внутри dest; link резолвится относительно директории
// symlink-а (linkDir). Абсолютный target по определению указывает вне dest →
// ошибка; относительный — резолвится лексически от linkDir и проверяется на
// выход за dest.
func writeSymlink(dest, target, link string) error {
	if filepath.IsAbs(link) {
		return fmt.Errorf("archive: symlink %q target escapes dest", link)
	}
	linkDir := filepath.Dir(target)
	resolved := filepath.Clean(filepath.Join(linkDir, link))
	if !withinDest(dest, resolved) {
		return fmt.Errorf("archive: symlink %q target escapes dest", link)
	}
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		return fmt.Errorf("archive: mkdir %s: %v", linkDir, err)
	}
	_ = os.Remove(target)
	if err := os.Symlink(link, target); err != nil {
		return fmt.Errorf("archive: symlink %s: %v", target, err)
	}
	return nil
}

func withinDest(dest, p string) bool {
	rel, err := filepath.Rel(dest, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// writeFileEntry материализует обычный файл записи. Размер контролируется через
// io.LimitReader: читаем не больше остатка бюджета+1, аккумулируем фактически
// записанное; превышение maxSize → ошибка (zip-bomb).
func writeFileEntry(target string, r io.Reader, mode fs.FileMode, lim *limits) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("archive: mkdir %s: %v", filepath.Dir(target), err)
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("archive: create %s: %v", target, err)
	}
	// budget+1 позволяет обнаружить превышение ровно на лимите (io.Copy дочитает
	// один лишний байт, addSize его поймает).
	budget := lim.maxSize - lim.usedSize + 1
	n, cerr := io.Copy(f, io.LimitReader(r, budget))
	if closeErr := f.Close(); closeErr != nil && cerr == nil {
		cerr = closeErr
	}
	if cerr != nil {
		return fmt.Errorf("archive: write %s: %v", target, cerr)
	}
	if err := os.Chmod(target, mode); err != nil {
		return fmt.Errorf("archive: chmod %s: %v", target, err)
	}
	return lim.addSize(n)
}

// readZipSymlink читает тело zip-записи symlink — это путь target.
func readZipSymlink(zf *zip.File) (string, error) {
	rc, err := zf.Open()
	if err != nil {
		return "", fmt.Errorf("archive: open zip symlink %q: %v", zf.Name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, 4096))
	if err != nil {
		return "", fmt.Errorf("archive: read zip symlink %q: %v", zf.Name, err)
	}
	return string(data), nil
}

// safeFileMode маскирует setuid/setgid/sticky из mode архива (anti-privesc,
// жёсткий инвариант) и оставляет только permission-биты.
func safeFileMode(mode fs.FileMode) fs.FileMode {
	return mode & 0o777
}

// dirMode — mode каталога из архива (маскированный); пустой/нулевой → 0o755.
func dirMode(mode fs.FileMode) fs.FileMode {
	perm := mode & 0o777
	if perm == 0 {
		return 0o755
	}
	return perm
}

func detectFormat(path string) string {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return "tar.gz"
	case strings.HasSuffix(lower, ".tar.bz2"), strings.HasSuffix(lower, ".tbz2"):
		return "tar.bz2"
	case strings.HasSuffix(lower, ".tar"):
		return "tar"
	case strings.HasSuffix(lower, ".zip"):
		return "zip"
	}
	return ""
}

func knownFormat(f string) bool {
	switch f {
	case "tar", "tar.gz", "tgz", "tar.bz2", "tbz2", "zip":
		return true
	}
	return false
}

// parseSize разбирает человекочитаемый размер: голое число (байты) или число с
// суффиксом KiB/MiB/GiB (бинарные множители; регистр суффикса игнорируется).
// Десятичные SI-суффиксы (KB/MB) и дроби не поддерживаются — для max_size это
// явный отказ (invalid size), а НЕ тихий partial-parse: остаток после снятия
// известного суффикса обязан быть чистым целым, иначе ошибка конфигурации.
func parseSize(s string) (int64, error) {
	str := strings.TrimSpace(s)
	if str == "" {
		return 0, fmt.Errorf("param %q: empty", "max_size")
	}
	upper := strings.ToUpper(str)
	var mult int64 = 1
	switch {
	case strings.HasSuffix(upper, "GIB"):
		mult, upper = 1<<30, strings.TrimSuffix(upper, "GIB")
	case strings.HasSuffix(upper, "MIB"):
		mult, upper = 1<<20, strings.TrimSuffix(upper, "MIB")
	case strings.HasSuffix(upper, "KIB"):
		mult, upper = 1<<10, strings.TrimSuffix(upper, "KIB")
	}
	upper = strings.TrimSpace(upper)
	// ParseInt по всему остатку: любой нераспознанный суффикс/мусор/дробь даёт
	// ошибку, а не молчаливый partial-parse префикса (footgun: "10MB" → 10 байт).
	n, err := strconv.ParseInt(upper, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("param %q: invalid size %q (want число или N[KiB|MiB|GiB])", "max_size", s)
	}
	// Защита от переполнения при умножении.
	if mult != 1 && n > (1<<62)/mult {
		return 0, fmt.Errorf("param %q: size %q too large", "max_size", s)
	}
	return n * mult, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
