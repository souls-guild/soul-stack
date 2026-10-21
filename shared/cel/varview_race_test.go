//go:build race

package cel

// The race detector slows TestVarView_TheFailureMemoChangesNoOutcome about twelvefold:
// a quarter of its views keep it under ten seconds.
func init() { memoViews = 600 }
