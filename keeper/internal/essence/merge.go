package essence

// mergeInto applies layer over on top of base destructively (mutates base)
// and returns the result. Deep-merge semantics (PM-decision 2):
//
//   - map + map  → recursive merge (later overrides earlier);
//   - everything else → over replaces the value wholesale (scalars and
//     lists are replaced, NOT appended).
//
// base is always our own accumulator (created in pipeline), so mutating it
// is safe; values from over are not shared by reference with the input
// layers only at the top level of maps — nested maps are reused as-is,
// which is fine since layers are never read again after merging.
func mergeInto(base, over map[string]any) map[string]any {
	if base == nil {
		base = make(map[string]any, len(over))
	}
	for k, ov := range over {
		bv, exists := base[k]
		if !exists {
			base[k] = ov
			continue
		}
		bm, bok := bv.(map[string]any)
		om, ook := ov.(map[string]any)
		if bok && ook {
			base[k] = mergeInto(bm, om)
			continue
		}
		base[k] = ov
	}
	return base
}
