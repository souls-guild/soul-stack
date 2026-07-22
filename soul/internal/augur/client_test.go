package augur

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeSender captures sent FromSoul messages and (optionally) automatically
// "replies" via client.Deliver, emulating the Keeper's recv loop.
type fakeSender struct {
	mu       sync.Mutex
	sent     []*keeperv1.AugurRequest
	sendErr  error
	auto     func(req *keeperv1.AugurRequest) *keeperv1.AugurReply
	deliver  func(*keeperv1.AugurReply) bool
	sendHook func()
}

func (f *fakeSender) SendFromSoul(msg *keeperv1.FromSoul) error {
	f.mu.Lock()
	if f.sendErr != nil {
		err := f.sendErr
		f.mu.Unlock()
		return err
	}
	req := msg.GetAugurRequest()
	f.sent = append(f.sent, req)
	auto := f.auto
	deliver := f.deliver
	hook := f.sendHook
	f.mu.Unlock()

	if hook != nil {
		hook()
	}
	// Emulate the async recv loop: the reply is delivered from a separate
	// goroutine, like handleSession's reader goroutine does (Deliver isn't
	// called from Fetch).
	if auto != nil && deliver != nil {
		go func() {
			if reply := auto(req); reply != nil {
				deliver(reply)
			}
		}()
	}
	return nil
}

func (f *fakeSender) sentRequests() []*keeperv1.AugurRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*keeperv1.AugurRequest, len(f.sent))
	copy(out, f.sent)
	return out
}

func okReply(reqID string, m map[string]any) *keeperv1.AugurReply {
	s, _ := structpb.NewStruct(m)
	return &keeperv1.AugurReply{
		RequestId: reqID,
		Status:    keeperv1.AugurStatus_AUGUR_STATUS_OK,
		Result:    &keeperv1.AugurReply_InlineData{InlineData: s},
	}
}

func TestFetch_OK_CorrelatesByRequestID(t *testing.T) {
	fs := &fakeSender{}
	c := NewClient(fs)
	fs.auto = func(req *keeperv1.AugurRequest) *keeperv1.AugurReply {
		// Echo request_id — correlation must key off exactly this.
		return okReply(req.GetRequestId(), map[string]any{"value": "secret-xyz"})
	}
	fs.deliver = c.Deliver

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reply, err := c.Fetch(ctx, "apply-1", "vault-prod", "secret/data/app#token")
	if err != nil {
		t.Fatalf("Fetch returned an error: %v", err)
	}
	if got := reply.GetInlineData().AsMap()["value"]; got != "secret-xyz" {
		t.Fatalf("inline_data.value = %v, want secret-xyz", got)
	}

	sent := fs.sentRequests()
	if len(sent) != 1 {
		t.Fatalf("sent %d requests, want 1", len(sent))
	}
	if sent[0].GetApplyId() != "apply-1" || sent[0].GetOmenName() != "vault-prod" {
		t.Fatalf("AugurRequest filled incorrectly: %+v", sent[0])
	}
	if sent[0].GetRequestId() == "" {
		t.Fatalf("request_id is empty - Soul must generate it")
	}
}

func TestFetch_WrongRequestIDNotDelivered_TimesOut(t *testing.T) {
	fs := &fakeSender{}
	c := NewClient(fs)
	// Reply with a FOREIGN request_id — correlation must not match; Fetch
	// waits until ctx is canceled.
	fs.auto = func(req *keeperv1.AugurRequest) *keeperv1.AugurReply {
		return okReply("wrong-id", map[string]any{"value": 1})
	}
	delivered := make(chan bool, 1)
	fs.deliver = func(r *keeperv1.AugurReply) bool {
		ok := c.Deliver(r)
		delivered <- ok
		return ok
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := c.Fetch(ctx, "apply-1", "omen", "q")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	select {
	case ok := <-delivered:
		if ok {
			t.Fatalf("Deliver with a foreign request_id returned true - correlation is broken")
		}
	case <-time.After(time.Second):
		t.Fatalf("Deliver was never called")
	}
}

func TestFetch_Timeout_CleansUpPending(t *testing.T) {
	fs := &fakeSender{} // no auto-reply: the request "hangs"
	c := NewClient(fs)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.Fetch(ctx, "apply-1", "omen", "q")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}

	// pending map is cleared on timeout (discard in Fetch's defer).
	c.mu.Lock()
	n := len(c.pending)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("after timeout, pending still has %d entries, want 0 (leak)", n)
	}
}

func TestFetch_ParallelRequests_EachGetsOwnReply(t *testing.T) {
	fs := &fakeSender{}
	c := NewClient(fs)
	// Each request gets a reply echoing its request_id with value = its
	// query, to check that parallel Fetches don't mix up replies.
	fs.auto = func(req *keeperv1.AugurRequest) *keeperv1.AugurReply {
		return okReply(req.GetRequestId(), map[string]any{"value": req.GetQuery()})
	}
	fs.deliver = c.Deliver

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	vals := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			q := "query-" + string(rune('A'+i))
			reply, err := c.Fetch(ctx, "apply-1", "omen", q)
			if err != nil {
				errs[i] = err
				return
			}
			if v, ok := reply.GetInlineData().AsMap()["value"].(string); ok {
				vals[i] = v
			}
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("request %d: error %v", i, errs[i])
		}
		want := "query-" + string(rune('A'+i))
		if vals[i] != want {
			t.Fatalf("request %d got value=%q, want %q - replies got mixed up", i, vals[i], want)
		}
	}

	c.mu.Lock()
	leftover := len(c.pending)
	c.mu.Unlock()
	if leftover != 0 {
		t.Fatalf("after parallel requests, pending still has %d, want 0", leftover)
	}
}

func TestRequestID_UniquePerStream(t *testing.T) {
	fs := &fakeSender{} // no reply — requests will hang, but request_id is already generated in Send
	c := NewClient(fs)

	const n = 500
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			_, _ = c.Fetch(ctx, "apply-1", "omen", "q")
		}()
	}
	wg.Wait()

	seen := make(map[string]struct{}, n)
	for _, req := range fs.sentRequests() {
		id := req.GetRequestId()
		if id == "" {
			t.Fatalf("empty request_id")
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate request_id %q - per-stream uniqueness violated", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != n {
		t.Fatalf("unique request_id count %d, sent %d", len(seen), n)
	}
}

func TestFetch_Denied(t *testing.T) {
	fs := &fakeSender{}
	c := NewClient(fs)
	fs.auto = func(req *keeperv1.AugurRequest) *keeperv1.AugurReply {
		return &keeperv1.AugurReply{
			RequestId: req.GetRequestId(),
			Status:    keeperv1.AugurStatus_AUGUR_STATUS_DENIED,
			Error:     "query outside allow-list",
		}
	}
	fs.deliver = c.Deliver

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := c.Fetch(ctx, "apply-1", "omen", "q")
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("expected ErrDenied, got %v", err)
	}
	if err.Error() == ErrDenied.Error() {
		t.Fatalf("Keeper's reason was lost - error is not wrapped with the reason")
	}
}

func TestFetch_Error(t *testing.T) {
	fs := &fakeSender{}
	c := NewClient(fs)
	fs.auto = func(req *keeperv1.AugurRequest) *keeperv1.AugurReply {
		return &keeperv1.AugurReply{
			RequestId: req.GetRequestId(),
			Status:    keeperv1.AugurStatus_AUGUR_STATUS_ERROR,
			Error:     "Omen unavailable",
		}
	}
	fs.deliver = c.Deliver

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := c.Fetch(ctx, "apply-1", "omen", "q")
	if !errors.Is(err, ErrRemote) {
		t.Fatalf("expected ErrRemote, got %v", err)
	}
}

func TestFetch_UnspecifiedTreatedAsDeny(t *testing.T) {
	fs := &fakeSender{}
	c := NewClient(fs)
	// UNSPECIFIED (zero-value) must be treated as deny (fail-safe).
	fs.auto = func(req *keeperv1.AugurRequest) *keeperv1.AugurReply {
		return &keeperv1.AugurReply{RequestId: req.GetRequestId()}
	}
	fs.deliver = c.Deliver

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := c.Fetch(ctx, "apply-1", "omen", "q")
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("UNSPECIFIED should be treated as ErrDenied, got %v", err)
	}
}

func TestFetch_OKWithoutInlineData_IsError(t *testing.T) {
	fs := &fakeSender{}
	c := NewClient(fs)
	// OK without inline_data (delegate=true isn't supported in MVP-1) — an
	// explicit error.
	fs.auto = func(req *keeperv1.AugurRequest) *keeperv1.AugurReply {
		return &keeperv1.AugurReply{
			RequestId: req.GetRequestId(),
			Status:    keeperv1.AugurStatus_AUGUR_STATUS_OK,
		}
	}
	fs.deliver = c.Deliver

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := c.Fetch(ctx, "apply-1", "omen", "q")
	if err == nil {
		t.Fatalf("OK without inline_data should be an error")
	}
}

func TestClose_UnblocksPendingFetch(t *testing.T) {
	fs := &fakeSender{} // no reply
	c := NewClient(fs)

	errCh := make(chan error, 1)
	go func() {
		errCh <- func() error {
			_, err := c.Fetch(context.Background(), "apply-1", "omen", "q")
			return err
		}()
	}()

	// Let Fetch register pending before Close.
	deadline := time.After(time.Second)
	for {
		c.mu.Lock()
		n := len(c.pending)
		c.mu.Unlock()
		if n == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("Fetch did not register pending within the allotted time")
		case <-time.After(time.Millisecond):
		}
	}

	c.Close()
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrClientClosed) {
			t.Fatalf("expected ErrClientClosed, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Fetch did not unblock after Close")
	}
}

func TestFetch_AfterClose_Rejected(t *testing.T) {
	fs := &fakeSender{}
	c := NewClient(fs)
	c.Close()
	_, err := c.Fetch(context.Background(), "apply-1", "omen", "q")
	if !errors.Is(err, ErrClientClosed) {
		t.Fatalf("Fetch after Close should return ErrClientClosed, got %v", err)
	}
}

func TestDeliver_NilAndUnknown(t *testing.T) {
	c := NewClient(&fakeSender{})
	if c.Deliver(nil) {
		t.Fatalf("Deliver(nil) should return false")
	}
	if c.Deliver(&keeperv1.AugurReply{RequestId: "no-such-id"}) {
		t.Fatalf("Deliver of unknown request_id should return false")
	}
}

func TestSend_ErrorPropagates(t *testing.T) {
	fs := &fakeSender{sendErr: errors.New("stream broken")}
	c := NewClient(fs)
	_, err := c.Fetch(context.Background(), "apply-1", "omen", "q")
	if err == nil {
		t.Fatalf("Send error should propagate outward")
	}
	// pending is cleared even when Send errors.
	c.mu.Lock()
	n := len(c.pending)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("after Send error, pending still has %d entries", n)
	}
}
