package render

import (
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
)

// wireTimeout is the timeout a task carries to the Soul: its effective
// `task_timeout` (settings.md §2.3) under the rendered scenario's level, "" for no
// limit. task must already carry its enclosing blocks' settings. When the task's
// own `timeout:` is the effective value it travels as written — so a scenario
// without `settings:`, and a destiny task, whose render's Scenario carries none,
// put on the wire exactly what they did before settings existed.
func wireTimeout(in RenderInput, task config.Task) string {
	var scenario *config.ScenarioSettings
	if in.Scenario != nil {
		scenario = in.Scenario.Settings
	}
	d := config.EffectiveTaskTimeout(scenario, task)
	if d == 0 {
		return ""
	}
	if own, err := config.ParseDuration(task.Timeout); err == nil && own == d {
		return task.Timeout
	}
	return formatDuration(d)
}

// formatDuration is time.Duration.String without the zero tails it appends to a
// whole hour or minute: 1h, not 1h0m0s.
func formatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
