package handlers

// derefStrings dereferences an optional []string from the request body (oapi/native
// yields *[]string for an omitempty field); nil → nil slice ("no values"). Package
// request-decoding helper. Extracted from operator.go during the handler-native
// rollout of operator (T5d): stays shared across the other domains (oracle etc.).
func derefStrings(in *[]string) []string {
	if in == nil {
		return nil
	}
	return *in
}

// derefString dereferences an optional string from the request body; nil → "".
func derefString(in *string) string {
	if in == nil {
		return ""
	}
	return *in
}

// slicePtrIfNotEmpty returns nil for an empty/nil slice (json omitempty parity over
// an array), otherwise a pointer to the slice. Package reply-projection helper;
// extracted from mypermissions.go during the handler-native rollout of catalog (T5d):
// stays shared across the other domains (oracle etc.).
func slicePtrIfNotEmpty(s []string) *[]string {
	if len(s) == 0 {
		return nil
	}
	return &s
}
