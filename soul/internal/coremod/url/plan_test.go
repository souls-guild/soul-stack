package url_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/url"
)

// TestURL_NotPlanReadSafe — core.url в MVP НЕ объявляет PlanReadSafe: pure-read
// drift «нужно ли скачать?» для бесчексумной ветки требует HEAD-запроса к
// remote-у, которого Apply ДО мутации не делает (см. doc Plan). Host применит
// default-deny (FAILED `plan.unsupported`) на dry_run — отдельный slice
// добавит HEAD-probe с opt-out-флагами симметрично Apply.
func TestURL_NotPlanReadSafe(t *testing.T) {
	m := url.New()
	if _, ok := any(m).(module.PlanReadSafe); ok {
		t.Fatal("core.url реализует PlanReadSafe (не должен в MVP — см. doc Plan)")
	}
}
