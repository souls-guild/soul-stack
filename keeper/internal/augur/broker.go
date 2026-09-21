package augur

// Vault broker MVP-1 (delegate=false, augur.md §2.1 / §5.3): for an already
// ALLOWED request ([Resolve] returned Decision{Allowed:true}), Keeper itself
// reads Vault KV and wraps the result in google.protobuf.Struct for
// AugurReply.inline_data. The external token/credential never reaches Soul.

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"
)

// KVReader — the narrow Vault-read surface the broker needs: the same ReadKV
// used by the render pipeline / core.vault.kv-read (augur.md §2.1). Narrowing
// to one method (instead of *vault.Client) lets unit tests fake it without
// standing up Vault; the real [*vault.Client] satisfies it automatically.
type KVReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// BrokerVault reads an allowed KV path and returns the data as
// google.protobuf.Struct (augur.md §5.3, verbatim map shape: secret keys
// become Struct keys).
//
// path is the normalized logical-path from [Decision.Query] (already went
// through ParseRef in Resolve; not re-normalized here). Scalar #field
// projection is post-Slice-B (here we broker the whole map).
//
// Returns an error on read failure (Vault unavailable / path vanished
// between resolve and read) — the caller returns AugurReply{status:ERROR}.
// The secret itself never lands in the error text (ReadFail wraps only path,
// which isn't secret).
func BrokerVault(ctx context.Context, kv KVReader, path string) (*structpb.Struct, error) {
	data, err := kv.ReadKV(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("augur: broker vault read %q: %w", path, err)
	}
	s, err := structpb.NewStruct(data)
	if err != nil {
		// The secret value doesn't serialize into Struct (type outside the JSON model).
		// path in the text isn't secret; the value itself is never logged.
		return nil, fmt.Errorf("augur: broker vault encode %q: %w", path, err)
	}
	return s, nil
}
