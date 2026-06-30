package render

import (
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
)

// TestKeeperRegisterChannel_Isolated — ★ HOST-FALLBACK GUARD Слайса 2 (handoff-флаг
// от review Слайса 1). keeper→keeper register-chaining льёт register keeper-задач
// предыдущих Passage в ИЗОЛИРОВАННЫЙ канал RenderInput.KeeperRegister. Инвариант:
//   - keeperVars видит KeeperRegister (keeper-задача активного Passage читает
//     register.<prev>.* keeper-задач прошлых Passage);
//   - hostRegister при ПУСТОМ per-host bucket НЕ видит KeeperRegister, остаётся на
//     плоской in.Register — host-задача смешанного Passage НЕ прочитает keeper-
//     register случайно через fallback (иначе host получил бы register.provision.*
//     keeper-задачи, к которому не обращался).
//
// До разделения каналов (Слайс 1 лил keeper-bucket в плоскую Register, которую
// читают ОБА — keeperVars и host-fallback) этот тест поймал бы утечку.
func TestKeeperRegisterChannel_Isolated(t *testing.T) {
	keeperReg := map[string]any{
		"provision": map[string]any{"ip": "10.0.0.7", "changed": true},
	}
	flatReg := map[string]any{
		"hostprobe": map[string]any{"stdout": "ok"},
	}

	in := RenderInput{
		Register:       flatReg,
		KeeperRegister: keeperReg,
	}

	// keeper-задача видит KeeperRegister (НЕ плоскую Register).
	kv := keeperVars(in)
	if _, ok := kv.Register["provision"]; !ok {
		t.Errorf("keeperVars.Register = %v, want содержащее keeper-register 'provision'", kv.Register)
	}
	if _, ok := kv.Register["hostprobe"]; ok {
		t.Errorf("keeperVars.Register протёк host-register 'hostprobe' — канал не изолирован: %v", kv.Register)
	}

	// host-задача с ПУСТЫМ per-host bucket → fallback на плоскую Register, НЕ на
	// KeeperRegister. keeper-register к ней не протекает.
	host := &topology.HostFacts{SID: "host-a.example.com"}
	hr := hostRegister(in, host)
	if _, ok := hr["provision"]; ok {
		t.Fatalf("★ hostRegister протёк keeper-register 'provision' через fallback — host случайно прочитал бы register.provision.* (handoff-флаг): %v", hr)
	}
	if _, ok := hr["hostprobe"]; !ok {
		t.Errorf("hostRegister = %v, want плоскую Register (fallback 'hostprobe') при пустом per-host bucket", hr)
	}
}

// TestKeeperRegisterChannel_PerHostBucketWins — host-задача со СВОИМ per-host
// bucket берёт его (а не KeeperRegister и не плоскую Register): keeper-канал не
// перебивает реальный per-host register хоста.
func TestKeeperRegisterChannel_PerHostBucketWins(t *testing.T) {
	host := &topology.HostFacts{SID: "host-a.example.com"}
	in := RenderInput{
		Register:       map[string]any{"flat": map[string]any{"v": 1}},
		KeeperRegister: map[string]any{"provision": map[string]any{"ip": "10.0.0.7"}},
		RegisterByHost: map[string]map[string]any{
			"host-a.example.com": {"role": map[string]any{"stdout": "master"}},
		},
	}
	hr := hostRegister(in, host)
	if _, ok := hr["role"]; !ok {
		t.Errorf("hostRegister = %v, want per-host bucket ('role')", hr)
	}
	if _, ok := hr["provision"]; ok {
		t.Errorf("hostRegister протёк keeper-register 'provision' поверх per-host bucket: %v", hr)
	}
}

// TestKeeperVars_FallbackToFlatRegister — backward-compat: KeeperRegister пуст
// (P0 / N=1 / не-staged / host-only Passage) → keeperVars деградирует к плоской
// Register (trial/push/прочие caller-ы, выставляющие только Register, видят
// register тем же путём БИТ-В-БИТ).
func TestKeeperVars_FallbackToFlatRegister(t *testing.T) {
	in := RenderInput{
		Register: map[string]any{"prev": map[string]any{"out": "x"}},
		// KeeperRegister == nil
	}
	kv := keeperVars(in)
	if _, ok := kv.Register["prev"]; !ok {
		t.Errorf("keeperVars.Register = %v, want fallback на плоскую Register ('prev') при пустом KeeperRegister", kv.Register)
	}
}
