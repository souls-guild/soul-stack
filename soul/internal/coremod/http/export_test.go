package http

import (
	"net/http"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
)

// NewRealClient возвращает дефолтный *http.Client модуля (с CheckRedirect) для
// httptest-теста, прогоняющего реальную redirect-цепочку: Transport теста
// подменяется на доверяющий самоподписанному httptest-cert, CheckRedirect
// сохраняется. Симметрично url.NewRealClient. Клиент строится фабрикой New() с
// нулевыми opts (максимально-безопасный default), как в проде.
func NewRealClient() *http.Client {
	return New().NewClient(util.HTTPClientOpts{}).(*http.Client)
}
