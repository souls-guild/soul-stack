package maintenance

import "time"

// WindowAuditPayload is the payload of `maintenance_window.created` /
// `.updated` / `.deleted`: the incarnation, the window id and the window's
// fields as they now are (as they were, for a delete). An update also carries
// `previous`, the fields before it. REST and MCP both build it here, so the two
// surfaces cannot record the same change differently.
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

func windowFields(w Window) map[string]any {
	return map[string]any{
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
