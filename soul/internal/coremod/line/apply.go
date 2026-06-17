package line

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// lineParams — разобранные и провалидированные параметры одного Apply-вызова.
// Собираются один раз в начале present/absent, дальше передаются в чистые
// функции редактирования.
type lineParams struct {
	line         string
	regexp       *regexp.Regexp
	insertAfter  string // "" | "EOF" | литерал
	insertBefore string // "" | "BOF" | литерал
	create       bool
	mode         string
	owner        string
	group        string
}

func (m *Module) readParams(req *pluginv1.ApplyRequest) (lineParams, error) {
	return readParamsFromStruct(req.Params)
}

// readParamsFromStruct — общий extractor для Apply и Plan (разные request-типы
// делят одинаковую структуру params). Apply раньше делал то же самое inline.
func readParamsFromStruct(params *structpb.Struct) (lineParams, error) {
	var p lineParams
	var err error

	if p.line, err = util.OptStringParam(params, "line"); err != nil {
		return p, err
	}
	rx, err := util.OptStringParam(params, "regexp")
	if err != nil {
		return p, err
	}
	if rx != "" {
		compiled, cerr := regexp.Compile(rx)
		if cerr != nil {
			return p, fmt.Errorf("param %q: invalid regexp: %v", "regexp", cerr)
		}
		p.regexp = compiled
	}
	if p.insertAfter, err = util.OptStringParam(params, "insertafter"); err != nil {
		return p, err
	}
	if p.insertBefore, err = util.OptStringParam(params, "insertbefore"); err != nil {
		return p, err
	}
	if p.insertAfter != "" && p.insertBefore != "" {
		return p, errors.New(`params "insertafter" and "insertbefore" are mutually exclusive`)
	}
	if p.create, err = util.OptBoolParam(params, "create"); err != nil {
		return p, err
	}
	if p.mode, err = util.OptStringParam(params, "mode"); err != nil {
		return p, err
	}
	if p.owner, err = util.OptStringParam(params, "owner"); err != nil {
		return p, err
	}
	if p.group, err = util.OptStringParam(params, "group"); err != nil {
		return p, err
	}
	return p, nil
}

func (m *Module) applyPresent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest, path string) error {
	p, err := m.readParams(req)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// Apply дублирует ключевые валидации намеренно (defense-in-depth: Apply
	// может вызываться без предшествующего Validate).
	if p.line == "" {
		return util.SendFailed(stream, `param "line": required for state present`)
	}

	content, existed, rerr := readFile(path)
	if rerr != nil {
		return util.SendFailed(stream, rerr.Error())
	}
	if !existed {
		if !p.create {
			return util.SendFailed(stream, fmt.Sprintf("%s: file not found, set create:true to create it", path))
		}
		// create=true: новый файл = единственная строка.
		mode, perr := util.ParseMode(p.mode)
		if perr != nil {
			return util.SendFailed(stream, perr.Error())
		}
		if werr := util.AtomicWrite(path, []byte(ensureTrailingNL(p.line)), mode); werr != nil {
			return util.SendFailed(stream, werr.Error())
		}
		if oerr := m.applyOwnership(path, p); oerr != nil {
			return util.SendFailed(stream, oerr.Error())
		}
		return util.SendFinal(stream, true, map[string]any{
			"path":    path,
			"changed": true,
			"created": true,
		})
	}

	lines, trailingNL := splitLines(content)
	out := presentEdit(lines, p)

	if !out.changed {
		return util.SendFinal(stream, false, map[string]any{
			"path":    path,
			"changed": false,
			"matched": out.matched,
		})
	}

	// in-place правка существующего файла: preserve mode/owner/group по
	// умолчанию (ADR-015), явные mode/owner/group — override.
	if werr := util.AtomicWritePreserving(
		path, []byte(joinLines(out.lines, trailingNL)),
		p.mode, p.owner, p.group, m.LookupUser, m.LookupGroup,
	); werr != nil {
		return util.SendFailed(stream, werr.Error())
	}

	output := map[string]any{
		"path":     path,
		"changed":  true,
		"matched":  out.matched,
		"replaced": out.replaced,
	}
	if out.warning != "" {
		output["warning"] = out.warning
	}
	return util.SendFinal(stream, true, output)
}

func (m *Module) applyAbsent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest, path string) error {
	p, err := m.readParams(req)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// Apply дублирует ключевые валидации намеренно (defense-in-depth: Apply
	// может вызываться без предшествующего Validate).
	if p.line == "" && p.regexp == nil {
		return util.SendFailed(stream, `state absent requires "line" or "regexp"`)
	}

	content, existed, rerr := readFile(path)
	if rerr != nil {
		return util.SendFailed(stream, rerr.Error())
	}
	if !existed {
		// absent + файла нет → нечего удалять, no-op (create игнорируется).
		return util.SendFinal(stream, false, map[string]any{
			"path":    path,
			"changed": false,
			"removed": 0,
		})
	}

	lines, trailingNL := splitLines(content)
	out := absentEdit(lines, p)

	if !out.changed {
		return util.SendFinal(stream, false, map[string]any{
			"path":    path,
			"changed": false,
			"removed": 0,
		})
	}

	// absent правит существующий файл — preserve mode/owner/group (ADR-015);
	// absent не принимает явных mode/owner/group, поэтому всегда сохраняем
	// текущие.
	if werr := util.AtomicWritePreserving(
		path, []byte(joinLines(out.lines, trailingNL)),
		"", "", "", m.LookupUser, m.LookupGroup,
	); werr != nil {
		return util.SendFailed(stream, werr.Error())
	}

	return util.SendFinal(stream, true, map[string]any{
		"path":    path,
		"changed": true,
		"removed": out.removed,
	})
}

func (m *Module) applyOwnership(path string, p lineParams) error {
	if p.owner == "" && p.group == "" {
		return nil
	}
	if _, err := util.ApplyOwnership(path, p.owner, p.group, m.LookupUser, m.LookupGroup); err != nil {
		return err
	}
	return nil
}

func readFile(path string) (content []byte, existed bool, err error) {
	b, rerr := os.ReadFile(path)
	switch {
	case rerr == nil:
		return b, true, nil
	case errors.Is(rerr, fs.ErrNotExist):
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("read %s: %v", path, rerr)
	}
}

// splitLines режет содержимое на логические строки без терминаторов \n.
// trailingNL фиксирует, оканчивался ли исходный файл переводом строки, чтобы
// joinLines восстановил это (и пустой файл не превращался в файл с пустой
// строкой). CRLF не нормализуется: \r остаётся частью строки и сравнивается
// как есть (предсказуемость — не угадываем намерения).
func splitLines(content []byte) (lines []string, trailingNL bool) {
	if len(content) == 0 {
		return nil, false
	}
	s := string(content)
	trailingNL = strings.HasSuffix(s, "\n")
	if trailingNL {
		s = strings.TrimSuffix(s, "\n")
	}
	return strings.Split(s, "\n"), trailingNL
}

// joinLines собирает строки обратно. trailingNL восстанавливает финальный
// перевод строки. Пустой набор строк → пустой файл.
func joinLines(lines []string, trailingNL bool) string {
	if len(lines) == 0 {
		return ""
	}
	s := strings.Join(lines, "\n")
	if trailingNL {
		s += "\n"
	}
	return s
}

func ensureTrailingNL(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
