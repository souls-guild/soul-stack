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

// fakeSender перехватывает отправленные FromSoul и (опционально) автоматически
// «отвечает» через client.Deliver, эмулируя recv-loop Keeper-а.
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
	// Эмулируем асинхронный recv-loop: ответ доставляется из отдельной горутины,
	// как делает reader-горутина handleSession (Deliver вызывается не из Fetch).
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
		// Эхо request_id — корреляция должна сработать ровно по нему.
		return okReply(req.GetRequestId(), map[string]any{"value": "secret-xyz"})
	}
	fs.deliver = c.Deliver

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reply, err := c.Fetch(ctx, "apply-1", "vault-prod", "secret/data/app#token")
	if err != nil {
		t.Fatalf("Fetch вернул ошибку: %v", err)
	}
	if got := reply.GetInlineData().AsMap()["value"]; got != "secret-xyz" {
		t.Fatalf("inline_data.value = %v, want secret-xyz", got)
	}

	sent := fs.sentRequests()
	if len(sent) != 1 {
		t.Fatalf("отправлено %d запросов, want 1", len(sent))
	}
	if sent[0].GetApplyId() != "apply-1" || sent[0].GetOmenName() != "vault-prod" {
		t.Fatalf("AugurRequest заполнен неверно: %+v", sent[0])
	}
	if sent[0].GetRequestId() == "" {
		t.Fatalf("request_id пуст — Soul обязан его сгенерировать")
	}
}

func TestFetch_WrongRequestIDNotDelivered_TimesOut(t *testing.T) {
	fs := &fakeSender{}
	c := NewClient(fs)
	// Отвечаем с ЧУЖИМ request_id — корреляция не должна сработать, Fetch ждёт
	// до отмены ctx.
	fs.auto = func(req *keeperv1.AugurRequest) *keeperv1.AugurReply {
		return okReply("не-тот-id", map[string]any{"value": 1})
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
		t.Fatalf("ожидался DeadlineExceeded, got %v", err)
	}
	select {
	case ok := <-delivered:
		if ok {
			t.Fatalf("Deliver с чужим request_id вернул true — корреляция сломана")
		}
	case <-time.After(time.Second):
		t.Fatalf("Deliver так и не был вызван")
	}
}

func TestFetch_Timeout_CleansUpPending(t *testing.T) {
	fs := &fakeSender{} // без auto-ответа: запрос «зависает»
	c := NewClient(fs)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.Fetch(ctx, "apply-1", "omen", "q")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ожидался DeadlineExceeded, got %v", err)
	}

	// pending-map очищен по таймауту (discard в defer Fetch).
	c.mu.Lock()
	n := len(c.pending)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("после таймаута в pending осталось %d записей, want 0 (утечка)", n)
	}
}

func TestFetch_ParallelRequests_EachGetsOwnReply(t *testing.T) {
	fs := &fakeSender{}
	c := NewClient(fs)
	// Каждому запросу — ответ с эхом его request_id и значением = его query,
	// чтобы проверить, что параллельные Fetch не перепутали ответы.
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
			t.Fatalf("запрос %d: ошибка %v", i, errs[i])
		}
		want := "query-" + string(rune('A'+i))
		if vals[i] != want {
			t.Fatalf("запрос %d получил value=%q, want %q — ответы перепутаны", i, vals[i], want)
		}
	}

	c.mu.Lock()
	leftover := len(c.pending)
	c.mu.Unlock()
	if leftover != 0 {
		t.Fatalf("после параллельных запросов в pending осталось %d, want 0", leftover)
	}
}

func TestRequestID_UniquePerStream(t *testing.T) {
	fs := &fakeSender{} // без ответа — запросы зависнут, но request_id уже сгенерён в Send
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
			t.Fatalf("пустой request_id")
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("дубль request_id %q — нарушена уникальность per-stream", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != n {
		t.Fatalf("уникальных request_id %d, отправлено %d", len(seen), n)
	}
}

func TestFetch_Denied(t *testing.T) {
	fs := &fakeSender{}
	c := NewClient(fs)
	fs.auto = func(req *keeperv1.AugurRequest) *keeperv1.AugurReply {
		return &keeperv1.AugurReply{
			RequestId: req.GetRequestId(),
			Status:    keeperv1.AugurStatus_AUGUR_STATUS_DENIED,
			Error:     "query вне allow-list",
		}
	}
	fs.deliver = c.Deliver

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := c.Fetch(ctx, "apply-1", "omen", "q")
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("ожидался ErrDenied, got %v", err)
	}
	if err.Error() == ErrDenied.Error() {
		t.Fatalf("причина Keeper-а потеряна — error не обёрнут reason-ом")
	}
}

func TestFetch_Error(t *testing.T) {
	fs := &fakeSender{}
	c := NewClient(fs)
	fs.auto = func(req *keeperv1.AugurRequest) *keeperv1.AugurReply {
		return &keeperv1.AugurReply{
			RequestId: req.GetRequestId(),
			Status:    keeperv1.AugurStatus_AUGUR_STATUS_ERROR,
			Error:     "Omen недоступен",
		}
	}
	fs.deliver = c.Deliver

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := c.Fetch(ctx, "apply-1", "omen", "q")
	if !errors.Is(err, ErrRemote) {
		t.Fatalf("ожидался ErrRemote, got %v", err)
	}
}

func TestFetch_UnspecifiedTreatedAsDeny(t *testing.T) {
	fs := &fakeSender{}
	c := NewClient(fs)
	// UNSPECIFIED (zero-value) — должен трактоваться как deny (защита).
	fs.auto = func(req *keeperv1.AugurRequest) *keeperv1.AugurReply {
		return &keeperv1.AugurReply{RequestId: req.GetRequestId()}
	}
	fs.deliver = c.Deliver

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := c.Fetch(ctx, "apply-1", "omen", "q")
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("UNSPECIFIED должен трактоваться как ErrDenied, got %v", err)
	}
}

func TestFetch_OKWithoutInlineData_IsError(t *testing.T) {
	fs := &fakeSender{}
	c := NewClient(fs)
	// OK без inline_data (delegate=true в MVP-1 не поддержан) — явная ошибка.
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
		t.Fatalf("OK без inline_data должен быть ошибкой")
	}
}

func TestClose_UnblocksPendingFetch(t *testing.T) {
	fs := &fakeSender{} // без ответа
	c := NewClient(fs)

	errCh := make(chan error, 1)
	go func() {
		errCh <- func() error {
			_, err := c.Fetch(context.Background(), "apply-1", "omen", "q")
			return err
		}()
	}()

	// Дать Fetch зарегистрировать pending до Close.
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
			t.Fatalf("Fetch не зарегистрировал pending за отведённое время")
		case <-time.After(time.Millisecond):
		}
	}

	c.Close()
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrClientClosed) {
			t.Fatalf("ожидался ErrClientClosed, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Fetch не разблокировался после Close")
	}
}

func TestFetch_AfterClose_Rejected(t *testing.T) {
	fs := &fakeSender{}
	c := NewClient(fs)
	c.Close()
	_, err := c.Fetch(context.Background(), "apply-1", "omen", "q")
	if !errors.Is(err, ErrClientClosed) {
		t.Fatalf("Fetch после Close должен вернуть ErrClientClosed, got %v", err)
	}
}

func TestDeliver_NilAndUnknown(t *testing.T) {
	c := NewClient(&fakeSender{})
	if c.Deliver(nil) {
		t.Fatalf("Deliver(nil) должен вернуть false")
	}
	if c.Deliver(&keeperv1.AugurReply{RequestId: "нет-такого"}) {
		t.Fatalf("Deliver неизвестного request_id должен вернуть false")
	}
}

func TestSend_ErrorPropagates(t *testing.T) {
	fs := &fakeSender{sendErr: errors.New("stream broken")}
	c := NewClient(fs)
	_, err := c.Fetch(context.Background(), "apply-1", "omen", "q")
	if err == nil {
		t.Fatalf("ошибка Send должна проброситься наружу")
	}
	// pending очищен даже при ошибке Send.
	c.mu.Lock()
	n := len(c.pending)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("после ошибки Send в pending осталось %d записей", n)
	}
}
