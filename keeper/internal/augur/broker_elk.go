package augur

// ELK broker MVP-1 (delegate=false, augur.md §2.1 / §6): for an already
// ALLOWED request ([Resolve] returned Decision{Allowed:true}), Keeper itself
// runs a read-only search against the Omen's Elasticsearch index and wraps
// the JSON response in google.protobuf.Struct for AugurReply.inline_data. The
// external credential never reaches Soul.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"
)

// elkSearchSuffix — the read-only Elasticsearch search endpoint. The broker
// does GET <endpoint>/<index>/_search (read-only); index-write / admin API
// are never used.
const elkSearchSuffix = "/_search"

// BrokerELK runs a read-only search against an Omen's allowed index and
// returns the JSON response as Struct (augur.md §5.3, verbatim object).
//
// index (decision.Query) already passed an exact match against
// Rite.allow.indices in Resolve — not re-authorized here. endpoint is
// UNtrusted input from the DB: validateEndpoint (https-only + literal-IP
// block-list) plus the client's dial-phase SSRF guard close the egress
// vector.
//
// credential is read from Omen.AuthRef (Vault) and attached to the request;
// it never reaches Soul. Failure → error (AugurReply{ERROR}); neither the
// credential nor the response body ever lands in the error text.
func BrokerELK(ctx context.Context, kv KVReader, doer HTTPDoer, endpoint, authRef, index string) (*structpb.Struct, error) {
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}
	cred, err := resolveCredential(ctx, kv, authRef)
	if err != nil {
		return nil, err
	}

	target, err := buildELKURL(endpoint, index)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("augur: build elk request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	cred.apply(req)

	return doJSONStruct(doer, req)
}

// buildELKURL builds the search URL: <endpoint>/<index>/_search. index is
// escaped via url.PathEscape (an UNtrusted value from the allow-list;
// path-injection via `../` / a stray slash is excluded). A trailing slash on
// endpoint is normalized away.
func buildELKURL(endpoint, index string) (string, error) {
	base := strings.TrimRight(endpoint, "/")
	target := base + "/" + url.PathEscape(index) + elkSearchSuffix
	u, err := url.Parse(target)
	if err != nil {
		return "", fmt.Errorf("augur: build elk url: %w", err)
	}
	return u.String(), nil
}
