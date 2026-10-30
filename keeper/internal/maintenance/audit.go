package maintenance

import "time"

// ExpiredReason is the `reason` of a `maintenance_window.deleted` or
// `freeze.deleted` the Reaper writes for a row past its retention (NIM-1137).
const ExpiredReason = "expired"

// WindowAuditPayload is the payload of `maintenance_window.created` /
// `.updated` / `.deleted`: the incarnation, the window id and the window's
// fields as they now are (as they were, for a delete). An update also carries
// `previous`, the fields before it. REST, MCP and the Reaper all build it here,
// so no two of them can record the same change differently.
func WindowAuditPayload(w Window, previous *Window) map[string]any {
	p := windowFields(w)
	p["incarnation_id"] = w.IncarnationID
	p["id"] = w.ID
	if previous != nil {
		p["previous"] = windowFields(*previous)
	}
	return p
}

// FreezeAuditPayload is the payload of `freeze.created` / `.updated` /
// `.deleted`, built the same way as [WindowAuditPayload].
func FreezeAuditPayload(f Freeze, previous *Freeze) map[string]any {
	p := freezeFields(f)
	p["incarnation_id"] = f.IncarnationID
	p["id"] = f.ID
	if previous != nil {
		p["previous"] = freezeFields(*previous)
	}
	return p
}

// windowFields are the fields of w's kind and no others, the shape the API
// gives the window.
func windowFields(w Window) map[string]any {
	if w.Kind == KindOnce {
		return map[string]any{
			"kind":      w.Kind,
			"starts_at": w.StartsAt.UTC().Format(time.RFC3339),
			"ends_at":   w.EndsAt.UTC().Format(time.RFC3339),
			"comment":   w.Comment,
		}
	}
	return map[string]any{
		"kind":             KindWeekly,
		"days":             w.Days,
		"start_time":       w.StartTime,
		"duration_minutes": w.DurationMinutes,
		"timezone":         w.Timezone,
		"comment":          w.Comment,
	}
}

func freezeFields(f Freeze) map[string]any {
	return map[string]any{
		"starts_at": f.StartsAt.UTC().Format(time.RFC3339),
		"ends_at":   f.EndsAt.UTC().Format(time.RFC3339),
		"comment":   f.Comment,
	}
}
