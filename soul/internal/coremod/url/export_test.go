package url

import (
	"net/http"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
)

// CheckRedirect экспортирует util.CheckRedirect для юнит-теста пакета url в
// изоляции: fake HTTPDoer-инъекция не прогоняет CheckRedirect реального
// клиента, поэтому downgrade-защиту проверяем либо здесь, либо реальным
// httptest-клиентом. Прямой util-тест на CheckRedirect — в util-пакете.
var CheckRedirect = util.CheckRedirect

// MaxRedirects экспортирует лимит редиректов (живёт в util после
// HTTPDoer→util-рефактора) для теста CheckRedirect в пакете url.
const MaxRedirects = util.MaxRedirects

// NewRealClient возвращает дефолтный *http.Client модуля (с CheckRedirect) для
// httptest-теста, прогоняющего реальную redirect-цепочку. Transport теста
// подменяется на доверяющий самоподписанному httptest-cert, CheckRedirect
// сохраняется. Строится через дефолтную фабрику New().NewClient с нулевыми
// флагами (максимально безопасный клиент).
func NewRealClient() *http.Client {
	return New().NewClient(util.HTTPClientOpts{}).(*http.Client)
}
