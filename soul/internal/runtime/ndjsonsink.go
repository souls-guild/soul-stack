package runtime

import (
	"fmt"
	"io"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// NDJSONSink — реализация [EventSink] для push-режима (`soul apply`, ADR-004).
// Пишет каждый TaskEvent и финальный RunResult как одну строку protojson
// (NDJSON / JSON-lines), которую Keeper читает из stdout SSH-сессии.
//
// Те же proto-сообщения, что и pull-режим (EventStream), — единый контракт
// (ADR-012). Транспорт другой: вместо gRPC-стрима — построчный stdout.
type NDJSONSink struct {
	w   io.Writer
	enc protojson.MarshalOptions
	// lastStatus — статус последнего записанного RunResult. Caller (`soul
	// apply`) читает его после Run для exit-кода: ApplyRunner всегда шлёт ровно
	// один RunResult в конце прогона.
	lastStatus keeperv1.RunStatus
}

// NewNDJSONSink собирает sink поверх w (обычно os.Stdout). Каждое сообщение
// сериализуется в одну строку (без отступов) и завершается '\n'.
func NewNDJSONSink(w io.Writer) *NDJSONSink {
	return &NDJSONSink{
		w: w,
		// Multiline:false — гарантия «одно сообщение = одна строка» (NDJSON).
		// EmitUnpopulated:false — компактный вывод; Keeper-side reader
		// использует те же proto-дефолты при Unmarshal.
		enc: protojson.MarshalOptions{Multiline: false},
	}
}

func (s *NDJSONSink) SendTaskEvent(ev *keeperv1.TaskEvent) error {
	return s.writeLine(ev)
}

func (s *NDJSONSink) SendRunResult(rr *keeperv1.RunResult) error {
	s.lastStatus = rr.GetStatus()
	return s.writeLine(rr)
}

// LastStatus возвращает статус последнего записанного RunResult. До первого
// SendRunResult — RUN_STATUS_UNSPECIFIED.
func (s *NDJSONSink) LastStatus() keeperv1.RunStatus {
	return s.lastStatus
}

func (s *NDJSONSink) writeLine(m proto.Message) error {
	b, err := s.enc.Marshal(m)
	if err != nil {
		return fmt.Errorf("ndjson: marshal %T: %w", m, err)
	}
	b = append(b, '\n')
	if _, err := s.w.Write(b); err != nil {
		return fmt.Errorf("ndjson: write %T: %w", m, err)
	}
	return nil
}
