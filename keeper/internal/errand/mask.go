package errand

import "github.com/souls-guild/soul-stack/shared/audit"

// OutputCapBytes — потолок размера stdout/stderr на одной Errand-границе
// (ADR-033 §6 «Инварианты», 64 KiB / channel). Cap применяется на
// keeper-side при приёме ErrandResult от Soul-а и при записи в БД/audit-event.
// Soul-side errand-runner (slice E3) применяет тот же cap defense-in-depth.
const OutputCapBytes = 64 * 1024

// truncate возрезает строку до n байт и возвращает (cut, true) при превышении.
// Cap по байтам (не runes) — соответствует SQL-семантике (TEXT в PG хранит
// byte-stream, не codepoint), upstream-cap и downstream-cap должны совпадать.
// При обрезании внутри multi-byte UTF-8 сиквенса финальный байт может быть
// невалидным UTF-8 — это нормально для captured-вывода ([]byte stream-а),
// клиенту по-прежнему отдаётся как string (json допускает invalid UTF-8 в
// строках с escape �, но мы тут уже за пределами обрезанной части).
func truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	return s[:n], true
}

// MaskAndCapBytes пропускает stdout/stderr через secret-masking и cap.
// Сначала маскинг (поиск vault-ref и sensitive-ключей по словарю shared/audit),
// потом cap — иначе срез мог бы разрезать чувствительную подстроку и оставить
// half-leak. Возвращает (masked, truncated).
//
// Маскинг работает над map-payload (shared/audit.MaskSecrets), для одиночной
// строки оборачиваем как ключ "v" и достаём обратно (тот же паттерн, что в
// keeper/internal/grpc/events_taskevent.go::maskString).
func MaskAndCapBytes(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	m := audit.MaskSecrets(map[string]any{"v": s})
	masked, _ := m["v"].(string)
	if masked == "" {
		masked = s
	}
	cut, trunc := truncate(masked, OutputCapBytes)
	return cut, trunc
}

// MaskOutputMap пропускает структурный output read-safe-модулей через
// secret-masking. Для shell/exec output всегда nil — функция не вызывается.
// nil-вход → nil-выход (handler решает, писать ли NULL в БД).
func MaskOutputMap(out map[string]any) map[string]any {
	if out == nil {
		return nil
	}
	return audit.MaskSecrets(out)
}
