package cmd

// Adapters between cobra flags and the optional fields of shared/api/wire.
//
// A wire field spelled `*T` with omitempty distinguishes "the operator did not
// ask" (key absent — the server applies its own default) from "the operator
// asked for the zero value". A cobra flag has only the zero value to work with,
// so the mapping is: unset flag → nil → key omitted. soulctl used to type these
// bodies itself with plain `T` + omitempty, which collapses the two: an
// explicit `--concurrency 0` and no flag at all left the wire identically, and
// no field could ever be sent as its zero value on purpose (NIM-776).

// optString maps an unset string flag to an omitted key.
func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// optInt maps a non-positive int flag to an omitted key. Every int field these
// feed carries `minimum:"1"`, so 0 is not a value the server would accept
// anyway — it is this CLI's spelling of "unset".
func optInt(n int) *int {
	if n <= 0 {
		return nil
	}
	return &n
}
