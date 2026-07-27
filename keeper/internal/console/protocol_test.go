package console

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// Wire-contract guard against the operator UI. The client half lives in
// `soul-stack-web/src/api/consoleProtocol.ts` and is written against exactly
// these field names and value spellings — a rename on this side breaks a
// terminal wall with no compile error anywhere.

func TestClientFrames_DecodeTheShapesTheUISends(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want ClientFrame
	}{
		{
			name: "open",
			raw:  `{"type":"open","session_id":"pane-1","sid":"host-a","cols":120,"rows":40}`,
			want: ClientFrame{Type: "open", SessionID: "pane-1", SID: "host-a", Cols: 120, Rows: 40},
		},
		{
			name: "open with shell override",
			raw:  `{"type":"open","session_id":"p","sid":"h","cols":80,"rows":24,"shell":"/bin/zsh"}`,
			want: ClientFrame{Type: "open", SessionID: "p", SID: "h", Cols: 80, Rows: 24, Shell: "/bin/zsh"},
		},
		{
			name: "stdin",
			raw:  `{"type":"stdin","session_id":"pane-1","data":"dG9wCg=="}`,
			want: ClientFrame{Type: "stdin", SessionID: "pane-1", Data: "dG9wCg=="},
		},
		{
			name: "resize",
			raw:  `{"type":"resize","session_id":"pane-1","cols":200,"rows":60}`,
			want: ClientFrame{Type: "resize", SessionID: "pane-1", Cols: 200, Rows: 60},
		},
		{
			name: "close",
			raw:  `{"type":"close","session_id":"pane-1"}`,
			want: ClientFrame{Type: "close", SessionID: "pane-1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeClientFrame([]byte(tc.raw))
			if err != nil {
				t.Fatalf("DecodeClientFrame: %v", err)
			}
			if *got != tc.want {
				t.Fatalf("decoded %+v, want %+v", *got, tc.want)
			}
		})
	}
}

func TestClientFrames_RejectMalformed(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"not json", `nope`},
		{"missing session_id", `{"type":"stdin","data":"AA=="}`},
		{"unknown type", `{"type":"teleport","session_id":"p"}`},
		{"open without sid", `{"type":"open","session_id":"p","cols":80,"rows":24}`},
		{"stdin with non-base64 data", `{"type":"stdin","session_id":"p","data":"not base64!!"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeClientFrame([]byte(tc.raw)); !errors.Is(err, ErrBadFrame) {
				t.Fatalf("err = %v, want ErrBadFrame", err)
			}
		})
	}
}

// A pty carries arbitrary bytes; base64 must survive every one of them.
func TestChunkFrame_Base64IsByteExact(t *testing.T) {
	payload := []byte{0x00, 0x1b, '[', '2', 'J', 0xff, 0xfe, '\n', 0x80}
	f := NewChunk("pane-1", keeperv1.ConsoleStream_CONSOLE_STREAM_STDOUT, payload, 0)

	got, err := base64.StdEncoding.DecodeString(f.Data)
	if err != nil {
		t.Fatalf("data is not valid base64: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("round-trip changed the bytes: %v -> %v", payload, got)
	}
}

// Field names and value spellings are the contract; a JSON-level check catches
// a rename that a struct-level one would not.
func TestServerFrames_JSONShape(t *testing.T) {
	t.Run("opened", func(t *testing.T) {
		m := marshalToMap(t, NewOpened("pane-1", "host-a", 4242))
		wantFields(t, m, map[string]any{
			"type": "opened", "session_id": "pane-1", "sid": "host-a", "pid": float64(4242),
		})
	})

	t.Run("chunk", func(t *testing.T) {
		m := marshalToMap(t, NewChunk("pane-1", keeperv1.ConsoleStream_CONSOLE_STREAM_STDOUT, []byte("hi"), 0))
		wantFields(t, m, map[string]any{
			"type": "chunk", "session_id": "pane-1", "stream": "stdout", "data": "aGk=",
		})
		// dropped_bytes is omitted in the normal case: the UI's union has no
		// such field yet, and a constant 0 would be noise on every chunk.
		if _, present := m["dropped_bytes"]; present {
			t.Fatal("dropped_bytes must be omitted when nothing was dropped")
		}
	})

	t.Run("chunk with drops", func(t *testing.T) {
		m := marshalToMap(t, NewChunk("pane-1", keeperv1.ConsoleStream_CONSOLE_STREAM_STDOUT, nil, 4096))
		if m["dropped_bytes"] != float64(4096) {
			t.Fatalf("dropped_bytes = %v, want 4096", m["dropped_bytes"])
		}
	})

	t.Run("exit", func(t *testing.T) {
		m := marshalToMap(t, NewExit("pane-1", 130,
			keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_PROCESS_EXITED, ""))
		wantFields(t, m, map[string]any{
			"type": "exit", "session_id": "pane-1", "code": float64(130), "reason": "process_exited",
		})
	})

	t.Run("error session-scoped", func(t *testing.T) {
		m := marshalToMap(t, NewError("pane-1", ErrCodeForbidden, "nope"))
		wantFields(t, m, map[string]any{
			"type": "error", "session_id": "pane-1", "code": ErrCodeForbidden, "message": "nope",
		})
	})

	t.Run("error socket-scoped", func(t *testing.T) {
		m := marshalToMap(t, NewError("", ErrCodeBadFrame, "bad"))
		if _, present := m["session_id"]; present {
			t.Fatal("a socket-scoped error must omit session_id — the UI keys panes off it")
		}
	})
}

// `code` is a NUMBER on exit and a STRING on error. The client's discriminated
// union depends on it, and one Go struct could not express both — this pins the
// split.
func TestExitAndErrorCodesHaveDifferentJSONTypes(t *testing.T) {
	exit := marshalToMap(t, NewExit("p", 7, keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_PROCESS_EXITED, ""))
	errFrame := marshalToMap(t, NewError("p", ErrCodeInternal, "boom"))

	if _, ok := exit["code"].(float64); !ok {
		t.Fatalf("exit.code is %T, want a number", exit["code"])
	}
	if _, ok := errFrame["code"].(string); !ok {
		t.Fatalf("error.code is %T, want a string", errFrame["code"])
	}
}

// A pty merges stdout and stderr onto one fd, so the client's union has only
// two members: an unspecified enum must not leak a third spelling.
func TestStreamName_NormalizesUnspecifiedToStdout(t *testing.T) {
	cases := map[keeperv1.ConsoleStream]string{
		keeperv1.ConsoleStream_CONSOLE_STREAM_UNSPECIFIED: "stdout",
		keeperv1.ConsoleStream_CONSOLE_STREAM_STDOUT:      "stdout",
		keeperv1.ConsoleStream_CONSOLE_STREAM_STDERR:      "stderr",
	}
	for in, want := range cases {
		if got := streamName(in); got != want {
			t.Fatalf("streamName(%v) = %q, want %q", in, got, want)
		}
	}
}

// Every ConsoleExitReason must map to a stable snake_case name; a new enum
// member arriving without one would silently show up as an empty reason.
func TestExitReasonName_CoversEveryEnumMember(t *testing.T) {
	want := map[keeperv1.ConsoleExitReason]string{
		keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_UNSPECIFIED:      "",
		keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_PROCESS_EXITED:   "process_exited",
		keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_CLOSED_BY_KEEPER: "closed_by_keeper",
		keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_OPEN_FAILED:      "open_failed",
		keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_SOUL_SHUTDOWN:    "soul_shutdown",
		keeperv1.ConsoleExitReason_CONSOLE_EXIT_REASON_LIMIT_EXCEEDED:   "limit_exceeded",
	}
	for value, name := range keeperv1.ConsoleExitReason_name {
		reason := keeperv1.ConsoleExitReason(value)
		expected, known := want[reason]
		if !known {
			t.Fatalf("ConsoleExitReason %s has no wire name — add it to exitReasonName", name)
		}
		if got := exitReasonName(reason); got != expected {
			t.Fatalf("exitReasonName(%s) = %q, want %q", name, got, expected)
		}
	}
}

// The subprotocol strings are shared with the browser client verbatim; the
// server must echo the first one back or the socket closes on connect.
func TestSubprotocolConstantsMatchTheClient(t *testing.T) {
	if Subprotocol != "soul-stack.console.v1" {
		t.Fatalf("Subprotocol = %q — consoleProtocol.ts pins soul-stack.console.v1", Subprotocol)
	}
	if BearerSubprotocolPrefix != "bearer." {
		t.Fatalf("BearerSubprotocolPrefix = %q — consoleProtocol.ts pins bearer.", BearerSubprotocolPrefix)
	}
}

func marshalToMap(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func wantFields(t *testing.T, got map[string]any, want map[string]any) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("field %q = %#v, want %#v (full frame: %#v)", k, got[k], v, got)
		}
	}
}
