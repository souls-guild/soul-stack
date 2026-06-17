package config

// ValidateOptions — управление поведением валидаторов конфигов.
//
// MVP — один флаг. Закладка под reach-проверки на старте `keeper`/`soul`
// (например, `vault.addr` доступен): соответствующие validator-ы будут жить в
// `shared/config/runtime/` и будут активироваться только при
// `AllowNetworkCalls: true` (default `false`).
type ValidateOptions struct {
	// AllowNetworkCalls reserved for future semantic-validate phases
	// (Vault reachability, Postgres ping). Not used in M0.thin.
	AllowNetworkCalls bool
}
