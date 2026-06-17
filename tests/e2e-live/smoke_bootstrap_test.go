//go:build e2e_live

package e2e_live_test

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

// TestL3bBootstrap_OneSoul — минимальный smoke L3b-2: реальный Bootstrap-flow
// одного soul-контейнера. Проверяет:
//   - soul init (CSR → Keeper.Bootstrap) проходит без ошибок;
//   - soul run выходит на souls.status='connected' (waitForSoulConnected внутри
//     SpawnSoulContainer);
//   - audit-event `soul.bootstrapped` записан Keeper-handler-ом (это
//     покрытие, которое L3a со stub-soul-ом не даёт — там Bootstrap RPC не
//     вызывается).
//
// Дальнейшие L3b-slice-ы (3+) поднимут на этой инфре реальный apply (nginx и
// т.п.); тут останавливаемся на онбординге.
func TestL3bBootstrap_OneSoul(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		// ExamplePath не нужен на L3b-2 (apply не запускаем); поле останется
		// пустым — git-seed скипается каноном NewStack.
		Souls: 1,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 1 {
		t.Fatalf("ожидался 1 soul-контейнер, получено %d", got)
	}
	sc := stack.SoulContainers[0]
	const wantSID = "soul-live-a.example.com"
	if sc.SID != wantSID {
		t.Errorf("SoulContainer.SID = %q, ожидалось %q", sc.SID, wantSID)
	}

	// soul.bootstrapped — пишется keeper-side bootstrapHandler-ом после
	// успешного COMMIT-а транзакции «burn token + insert seed + status flip».
	// Subset включает SID (стабильное поле payload-а).
	stack.AssertAuditEvent(t, "soul.bootstrapped", map[string]any{
		"sid": wantSID,
	})

	// Sanity-check: souls-строка в connected-статусе видна напрямую (waitFor*
	// внутри Spawn уже проверил, дополнительная гарантия после full-Cleanup-
	// gate-а: snapshot снят до teardown-а).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var status string
	if err := stack.DB().QueryRow(ctx,
		"SELECT status FROM souls WHERE sid = $1", wantSID).Scan(&status); err != nil {
		t.Fatalf("SELECT souls.status: %v", err)
	}
	if status != "connected" {
		t.Errorf("souls.status = %q, ожидался connected", status)
	}
}
