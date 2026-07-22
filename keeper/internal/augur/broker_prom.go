package augur

// Prometheus broker MVP-1 (delegate=false, augur.md §2.1 / §6): for an
// already ALLOWED request ([Resolve] returned Decision{Allowed:true}), Keeper
// itself runs a live instant query against Prometheus and wraps the JSON
// response in google.protobuf.Struct for AugurReply.inline_data. The external
// credential never reaches Soul.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"
)

// promQueryPath — the instant-query endpoint of the Prometheus HTTP API v1.
// The broker runs a read-only instant query; range-query / admin API are
// never used.
const promQueryPath = "/api/v1/query"

// BrokerPrometheus runs a promQL instant query against an Omen's Prometheus
// endpoint and returns the JSON response as Struct (augur.md §5.3, verbatim
// object).
//
// promQL (decision.Query) already passed an exact match against
// Rite.allow.queries in Resolve — not re-authorized here. endpoint is
// UNtrusted input from the DB: validateEndpoint (https-only + literal-IP
// block-list) plus the client's (doer's) dial-phase SSRF guard close the
// egress vector.
//
// credential is read from Omen.AuthRef (Vault) and attached to the request;
// it never reaches Soul (data flows through Keeper inline). A read/request
// failure → error (the caller returns AugurReply{ERROR}); neither the
// credential nor the response body ever lands in the error text.
func BrokerPrometheus(ctx context.Context, kv KVReader, doer HTTPDoer, endpoint, authRef, promQL string) (*structpb.Struct, error) {
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}
	cred, err := resolveCredential(ctx, kv, authRef)
	if err != nil {
		return nil, err
	}

	target, err := buildPromURL(endpoint, promQL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("augur: build prometheus request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	cred.apply(req)

	return doJSONStruct(doer, req)
}

// buildPromURL builds the instant-query URL:
// <endpoint>/api/v1/query?query=<promQL>. promQL is encoded via url.Values
// (no string concatenation) — injecting extra query params via promQL's
// content is excluded. A trailing slash on endpoint is normalized away to
// avoid `//api/v1/query`.
func buildPromURL(endpoint, promQL string) (string, error) {
	u, err := url.Parse(strings.TrimRight(endpoint, "/") + promQueryPath)
	if err != nil {
		return "", fmt.Errorf("augur: build prometheus url: %w", err)
	}
	q := url.Values{}
	q.Set("query", promQL)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
