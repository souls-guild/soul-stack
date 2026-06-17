package augur

import (
	"context"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// Fetcher — узкая поверхность Augur-клиента, нужная модулю core.augur.fetch.
// Выделена, чтобы модуль не зависел от конкретного *Client (тестируется
// fake-фетчером) и чтобы исключить доступ модуля к Deliver/Close (это
// инфраструктура recv-loop-а сессии, не модуля). *Client её удовлетворяет.
type Fetcher interface {
	Fetch(ctx context.Context, applyID, omen, query string) (*keeperv1.AugurReply, error)
}

// runContext несёт в apply-цикл Augur-клиент сессии и apply_id прогона. Кладётся
// в ctx перед вызовом модуля (через stream.Context()), потому что общий
// SoulModule-контракт (state+params) этого не выражает, а custom sub-process
// модулям доступ к Augur и не положен — это keeper-side доверенный канал только
// для Soul-side core.
type runContext struct {
	fetcher Fetcher
	applyID string
}

type ctxKey struct{}

// WithRun кладёт Augur-клиент и apply_id в ctx для одного прогона. fetcher может
// быть nil (push-режим / сессия без Augur) — тогда FromContext вернёт ok=false,
// а модуль вернёт понятную ошибку «Augur недоступен».
func WithRun(ctx context.Context, fetcher Fetcher, applyID string) context.Context {
	return context.WithValue(ctx, ctxKey{}, runContext{fetcher: fetcher, applyID: applyID})
}

// FromContext извлекает Augur-клиент и apply_id. ok=false, если в ctx ничего нет
// (apply без Augur-плумбинга) либо fetcher nil — модуль трактует это как
// «Augur недоступен в этом прогоне».
func FromContext(ctx context.Context) (fetcher Fetcher, applyID string, ok bool) {
	rc, present := ctx.Value(ctxKey{}).(runContext)
	if !present || rc.fetcher == nil {
		return nil, "", false
	}
	return rc.fetcher, rc.applyID, true
}
