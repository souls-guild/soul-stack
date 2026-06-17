package sshprovider

import "fmt"

// DenyReason — стабильный машинно-читаемый код отказа Authorize, общий для всех
// SshProvider-плагинов тиража (static-key / Vault SSH CA / Teleport). Кладётся в
// [pluginv1.AuthorizeReply.Reason]; Keeper пишет его в audit fail-closed-отказа.
// Без общего словаря тираж провайдеров даст N диалектов deny-сообщений — Keeper
// не сможет агрегировать причины отказа по флоту.
//
// Это НЕ часть proto-контракта: на wire едет открытая строка
// [pluginv1.AuthorizeReply.Reason]; словарь — соглашение SDK-стороны, расширяемое
// без правки proto.
type DenyReason string

const (
	// DenyExplicitDeny — пара (host, user) явно в deny-list провайдера.
	DenyExplicitDeny DenyReason = "explicit_deny"

	// DenyNotInAllowlist — провайдер работает в allowlist-режиме, пара не в нём.
	DenyNotInAllowlist DenyReason = "not_in_allowlist"

	// DenyPolicy — отказ по политике провайдера (например, root-login запрещён).
	DenyPolicy DenyReason = "policy"
)

// DenyMessage — консолидированное reason-поле AuthorizeReply: `<reason>: <detail>`.
// Формат един для всех провайдеров, чтобы Keeper/оператор видели одинаковую
// структуру отказа. `detail` — человекочитаемое уточнение (имя пользователя,
// сработавшее правило); пустой detail допустим (тогда reason без суффикса).
func DenyMessage(reason DenyReason, detail string) string {
	if detail == "" {
		return string(reason)
	}
	return fmt.Sprintf("%s: %s", reason, detail)
}

// SignFailReason — стабильный код ошибки Sign, общий для тиража. Sign возвращает
// ошибку (не reply), поэтому код едет внутри error-сообщения через [SignError];
// Keeper маппит шаг в failed и пишет код в диагностический канал.
type SignFailReason string

const (
	// SignFailReadKey — провайдер не смог прочитать/распарсить материал ключа
	// (битый PEM, отсутствует файл, недоступен Vault). Fail-closed: Keeper НЕ
	// открывает SSH-сессию.
	SignFailReadKey SignFailReason = "read_key"

	// SignFailIssue — провайдер не смог выпустить/подписать credentials
	// (CA-провайдеры: отказ Vault SSH CA, истёкшая роль).
	SignFailIssue SignFailReason = "issue"
)

// SignError — error для Sign с пришитым стабильным [SignFailReason]:
// `<reason>: <err>`. Возвращается провайдером из Sign на любой fail-closed-ветке;
// формат един для тиража.
func SignError(reason SignFailReason, err error) error {
	return fmt.Errorf("%s: %w", reason, err)
}
