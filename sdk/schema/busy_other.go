//go:build !unix

package schema

// isTextFileBusy has no equivalent outside unix: platforms without ETXTBSY report a
// generic sharing violation, which is not safe to retry blindly.
func isTextFileBusy(error) bool { return false }
