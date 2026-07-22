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

// lineParams holds the parsed and validated params for one Apply call.
// Assembled once at the start of present/absent, then passed to the pure
// editing functions.
type lineParams struct {
	line         string
	regexp       *regexp.Regexp
	insertAfter  string // "" | "EOF" | literal
	insertBefore string // "" | "BOF" | literal
	create       bool
	mode         string
	owner        string
	group        string
}

func (m *Module) readParams(req *pluginv1.ApplyRequest) (lineParams, error) {
	return readParamsFromStruct(req.Params)
}

// readParamsFromStruct is the shared extractor for Apply and Plan (different
// request types share the same params structure). Apply used to do this
// inline.
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
	// Apply deliberately duplicates key validations (defense-in-depth: Apply
	// can be called without a preceding Validate).
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
		// create=true: new file = single line.
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

	// in-place edit of an existing file: preserve mode/owner/group by
	// default (ADR-015), explicit mode/owner/group override.
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
	// Apply deliberately duplicates key validations (defense-in-depth: Apply
	// can be called without a preceding Validate).
	if p.line == "" && p.regexp == nil {
		return util.SendFailed(stream, `state absent requires "line" or "regexp"`)
	}

	content, existed, rerr := readFile(path)
	if rerr != nil {
		return util.SendFailed(stream, rerr.Error())
	}
	if !existed {
		// absent + file missing → nothing to remove, no-op (create is ignored).
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

	// absent edits an existing file — preserve mode/owner/group (ADR-015);
	// absent doesn't accept explicit mode/owner/group, so current ones are
	// always kept.
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

// splitLines splits content into logical lines without \n terminators.
// trailingNL records whether the source file ended with a newline, so
// joinLines can restore it (and an empty file doesn't turn into a file with
// one empty line). CRLF isn't normalized: \r stays part of the line and is
// compared as-is (predictability — we don't guess intent).
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

// joinLines reassembles lines. trailingNL restores the final newline. An
// empty set of lines → empty file.
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
