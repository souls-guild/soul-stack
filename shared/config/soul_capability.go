package config

// Soul-capabilities — канонические строковые имена фичей протокола Keeper↔Soul,
// которые Soul-бинарь анонсирует в Hello.capabilities (ADR-056 §S5 forward-compat).
// Keeper персистит анонс рядом с presence и сверяет ДО dispatch-а фич-зависимых
// прогонов. Реестр — naming-rules.md → «Soul-capabilities».
//
// Константы живут в shared/config (а не в keeper- или soul-internal): и keeper
// (staged-гейт run.go), и Soul (анонс grpc-клиента) обязаны ссылаться на ОДНУ
// строку — рассинхрон литералов = молчаливый fail-closed на каждом staged-прогоне.
const (
	// CapabilityPassage — Soul эхает ApplyRequest.passage в TaskEvent/RunResult,
	// то есть умеет участвовать в staged-render (N>1 Passage, ADR-056). Soul без
	// этой capability под staged-сценарием отвергается keeper-ом ДО dispatch
	// (soul_passage_unsupported, fail-closed): иначе barrier следующего Passage
	// ждал бы терминал, которого старый бинарь не пришлёт.
	CapabilityPassage = "passage"
)

// SoulCapabilities — набор capabilities, которые ЭТА сборка soul-бинаря
// поддерживает (анонсируется в Hello.capabilities). Все souls беты собираются
// вместе с keeper-ом, поэтому набор статичен; форвард-safety — для будущих
// mixed-version флотов, где старый бинарь пришлёт пустой/урезанный набор.
func SoulCapabilities() []string {
	return []string{CapabilityPassage}
}
