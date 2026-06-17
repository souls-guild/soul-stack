//go:build linux

package beacon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/types/known/structpb"
)

// InotifyName — адрес core-beacon (`core.beacon.<name>`, VigilDef.check).
const InotifyName = beaconaddr.Inotify

const (
	stateInotifyQuiet  State = "quiet"
	stateInotifyEvents State = "events"
)

// inotifyBufferSize — буфер чтения kernel-events. Kernel-документация
// рекомендует BUF_LEN ≥ sizeof(struct inotify_event) + NAME_MAX + 1 ≈ 16+256+1.
// 4 KiB вмещает порядка 16 средних событий за один read — баланс между
// латентностью и числом read-syscall-ов.
const inotifyBufferSize = 4096

// inotifyReadIdle — задержка между read-syscall-ами, когда kernel-fd пуст.
// Кратко: read non-blocking → EAGAIN → ожидание перед следующей попыткой. Без
// pause горутина CPU-spins при затишье. 100 ms — компромисс между real-time
// откликом и cost-of-wakeup; меньше — больше syscall-нагрузка, больше — выше
// латентность первого события в окне Check.
const inotifyReadIdle = 100 * time.Millisecond

// InotifyBeacon — core-beacon для kernel inotify syscall (Linux-only, V5-3,
// ADR-030 amendment 2026-05-26).
//
// Fold-adapter (вариант α из architect-вердикта): per-path background-goroutine
// читает inotify-fd, аккумулирует события в буфер; Check на каждом тике
// scheduler-а возвращает «окно» накопленных событий + state quiet/events.
// Read-only по конструкции (ADR-030): kernel-fd только наблюдает, не пишет.
//
// MVP-ограничения (см. docs/module/core/beacon/README.md → core.beacon.inotify):
//   - recursive: false-only (param `recursive: true` принимается грамматикой,
//     но текущая imple НЕ рекурсивно регистрирует под-каталоги; отложен до
//     явного запроса оператора, потенциальный источник багов в новом коде).
//   - throttle игнорируется (поле есть в params для forward-compat, но в MVP
//     все события эмитятся; throttle планируется отдельным slice-ом).
//
// Singleton-семантика: один экземпляр InotifyBeacon обслуживает все Vigil-ы
// этого процесса (статический Registry, как у других core-beacon). State
// per-path хранится в `watches` map. Несколько Vigil-ов с разными path —
// независимые kernel-fd и независимые буферы. См. также «Lifecycle» ниже.
//
// Lifecycle (известный trade-off MVP): scheduler не сигнализирует beacon-у
// «этот Vigil удалён», поэтому fd для исчезнувшего path остаётся открытым до
// shutdown процесса (kernel сам освободит). В долгоживущем soul-демоне это
// ограниченный leak (мн-во уникальных path конечно). Явный hook Stop() в
// интерфейсе Beacon — отложен (см. observations к V5-3).
type InotifyBeacon struct {
	mu      sync.Mutex
	watches map[string]*inotifyWatch // path → активный watch
}

// inotifyWatch — одно зарегистрированное наблюдение за path. Принадлежит ровно
// одному Vigil (один InotifyBeacon → много Vigil → много watch).
type inotifyWatch struct {
	fd        int
	wd        int
	path      string
	eventMask uint32

	mu     sync.Mutex // защищает events
	events []inotifyEventBuf

	stopCh chan struct{}
	done   chan struct{}
}

// inotifyEventBuf — одно событие из kernel-fd, нормализованное Soul-side в
// стабильный type-string. Сырой mask наружу не светим (kernel-константы
// не должны просачиваться в where-CEL Decree).
type inotifyEventBuf struct {
	op   string
	name string
	at   int64
}

// NewInotify собирает beacon. Никаких kernel-fd на старте — fd создаётся
// lazily в первом Check для каждого уникального path.
func NewInotify() *InotifyBeacon {
	return &InotifyBeacon{watches: make(map[string]*inotifyWatch)}
}

// Check на первом вызове для path регистрирует kernel-fd и спавнит
// read-goroutine; на последующих — забирает накопленные события за окно
// (между предыдущим и текущим Check). Окно пустое → state "quiet",
// иначе "events". Edge-triggered Portent эмитится scheduler-ом при
// смене state quiet↔events.
//
// Params:
//   - `path` (string, required) — абсолютный путь к файлу или каталогу.
//   - `events` (list of string, optional) — фильтр типов событий:
//     "created" / "modified" / "deleted" / "moved" / "attrib". Default —
//     все пять. Невалидный элемент игнорируется (forward-compat).
//   - `recursive` (bool, optional, default false) — MVP принимает только
//     false; true возвращает ошибку валидации.
//   - `throttle` (string duration, optional) — принимается, в MVP игнорируется.
func (b *InotifyBeacon) Check(_ context.Context, params *structpb.Struct) (State, *structpb.Struct, error) {
	path, err := util.StringParam(params, "path")
	if err != nil {
		return "", nil, err
	}
	eventsFilter, err := util.OptStringSliceParam(params, "events")
	if err != nil {
		return "", nil, err
	}
	recursive, err := util.OptBoolParam(params, "recursive")
	if err != nil {
		return "", nil, err
	}
	if recursive {
		return "", nil, fmt.Errorf("param %q: recursive watch не поддерживается в MVP (V5-3)", "recursive")
	}
	mask := resolveInotifyMask(eventsFilter)

	b.mu.Lock()
	w, ok := b.watches[path]
	b.mu.Unlock()

	// Lazy-init / restart при смене mask.
	if !ok || w.eventMask != mask {
		nw, err := b.restartWatch(path, mask, w)
		if err != nil {
			return "", nil, err
		}
		w = nw
	}

	// Забираем окно событий (под локом self).
	w.mu.Lock()
	flushed := w.events
	w.events = nil
	w.mu.Unlock()

	if len(flushed) == 0 {
		return stateInotifyQuiet, inotifyData(path, nil), nil
	}
	return stateInotifyEvents, inotifyData(path, flushed), nil
}

// restartWatch закрывает старый watch (если был) и регистрирует новый. Возврат
// — новый watch. Под отдельным внутренним локом B.mu (без удержания на время
// startWatch — syscall может блокироваться на регистрации).
func (b *InotifyBeacon) restartWatch(path string, mask uint32, old *inotifyWatch) (*inotifyWatch, error) {
	if old != nil {
		old.stop()
	}
	w, err := b.startWatch(path, mask)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.watches[path] = w
	b.mu.Unlock()
	return w, nil
}

// startWatch создаёт inotify-fd, регистрирует watch на path и спавнит
// read-goroutine. ENOSPC (max_user_watches исчерпан) приходит из
// inotify_add_watch — конвертируем в понятную ошибку оператору.
func (b *InotifyBeacon) startWatch(path string, mask uint32) (*inotifyWatch, error) {
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("inotify_init: %w", err)
	}
	wd, err := unix.InotifyAddWatch(fd, path, mask)
	if err != nil {
		_ = unix.Close(fd)
		if errors.Is(err, syscall.ENOSPC) {
			return nil, fmt.Errorf("inotify_add_watch %s: max_user_watches исчерпан (sysctl fs.inotify.max_user_watches)", path)
		}
		return nil, fmt.Errorf("inotify_add_watch %s: %w", path, err)
	}
	w := &inotifyWatch{
		fd:        fd,
		wd:        wd,
		path:      path,
		eventMask: mask,
		stopCh:    make(chan struct{}),
		done:      make(chan struct{}),
	}
	go w.readLoop()
	return w, nil
}

// stop останавливает read-goroutine и закрывает kernel-fd. После stop watch
// больше не получит событий; вызывается из restartWatch (смена mask) либо
// при shutdown процесса (kernel сам очистит, но defensive cleanup полезен в
// тестах). Идемпотентен.
func (w *inotifyWatch) stop() {
	select {
	case <-w.stopCh:
		return // уже остановлен
	default:
	}
	close(w.stopCh)
	// Closing fd вызывает EBADF в read → readLoop выходит.
	_ = unix.Close(w.fd)
	<-w.done
}

// readLoop — фон. Читает inotify-fd через unix.Read (non-blocking), парсит
// inotify_event-структуры и складывает их в w.events под w.mu. На EAGAIN
// (пусто) — короткий sleep; на EBADF (closed fd) или stopCh — выход.
func (w *inotifyWatch) readLoop() {
	defer close(w.done)
	buf := make([]byte, inotifyBufferSize)
	for {
		select {
		case <-w.stopCh:
			return
		default:
		}
		n, err := unix.Read(w.fd, buf)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				select {
				case <-w.stopCh:
					return
				case <-time.After(inotifyReadIdle):
				}
				continue
			}
			// EBADF / EINTR / closed → выход.
			return
		}
		if n <= 0 {
			continue
		}
		w.parseAndAppend(buf[:n])
	}
}

// parseAndAppend разбирает буфер inotify_event-ов. Каждое событие имеет
// фиксированный header (SizeofInotifyEvent) + опц. имя длиной Len (для watch
// на каталог имя файла, для watch на отдельный файл — пусто).
func (w *inotifyWatch) parseAndAppend(buf []byte) {
	now := time.Now().Unix()
	var batch []inotifyEventBuf
	for offset := 0; offset+unix.SizeofInotifyEvent <= len(buf); {
		raw := (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset]))
		nameLen := int(raw.Len)
		op := mapInotifyMaskToOp(raw.Mask)
		var name string
		if nameLen > 0 {
			end := offset + unix.SizeofInotifyEvent + nameLen
			if end > len(buf) {
				break // битый буфер — kernel не делит события поперёк read, на всякий
			}
			name = strings.TrimRight(string(buf[offset+unix.SizeofInotifyEvent:end]), "\x00")
		}
		if op != "" {
			batch = append(batch, inotifyEventBuf{op: op, name: name, at: now})
		}
		offset += unix.SizeofInotifyEvent + nameLen
	}
	if len(batch) == 0 {
		return
	}
	w.mu.Lock()
	w.events = append(w.events, batch...)
	w.mu.Unlock()
}

// inotifyData собирает PortentEvent.data (legacy Struct-ветка). typed-payload
// формируется отдельно через fillTypedPayload (typed_payload.go).
func inotifyData(path string, events []inotifyEventBuf) *structpb.Struct {
	fields := map[string]any{
		"path":  path,
		"count": len(events),
	}
	if len(events) > 0 {
		list := make([]any, 0, len(events))
		for _, e := range events {
			list = append(list, map[string]any{
				"type": e.op,
				"file": e.name,
				"at":   e.at,
			})
		}
		fields["events"] = list
	}
	s, _ := structpb.NewStruct(fields)
	return s
}

// resolveInotifyMask преобразует фильтр оператора (`events: [...]`) в
// kernel-маску. Пустой фильтр → все 5 поддерживаемых типов событий.
// Неизвестный элемент молча игнорируется (forward-compat: новые типы
// добавляются грамматикой, старая imple их не видит).
func resolveInotifyMask(events []string) uint32 {
	if len(events) == 0 {
		return unix.IN_CREATE | unix.IN_MODIFY | unix.IN_DELETE |
			unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_ATTRIB
	}
	var mask uint32
	for _, e := range events {
		switch e {
		case "created":
			mask |= unix.IN_CREATE
		case "modified":
			mask |= unix.IN_MODIFY
		case "deleted":
			mask |= unix.IN_DELETE
		case "moved":
			mask |= unix.IN_MOVED_FROM | unix.IN_MOVED_TO
		case "attrib":
			mask |= unix.IN_ATTRIB
		}
	}
	return mask
}

// mapInotifyMaskToOp проецирует kernel-mask в стабильный type-string. Из
// нескольких бит выбирается «главный» по приоритету (created/deleted поверх
// modified — типичный паттерн edit→create→delete на одном файле). Системные
// IN_IGNORED/IN_Q_OVERFLOW отображаются в пустую строку, parseAndAppend их
// пропускает (это не пользовательские события).
func mapInotifyMaskToOp(mask uint32) string {
	switch {
	case mask&unix.IN_CREATE != 0:
		return "created"
	case mask&unix.IN_DELETE != 0, mask&unix.IN_DELETE_SELF != 0:
		return "deleted"
	case mask&unix.IN_MOVED_FROM != 0, mask&unix.IN_MOVED_TO != 0, mask&unix.IN_MOVE_SELF != 0:
		return "moved"
	case mask&unix.IN_MODIFY != 0:
		return "modified"
	case mask&unix.IN_ATTRIB != 0:
		return "attrib"
	}
	return ""
}
