// The `instance` object's implementation — one read of `system.local`.
//
// Liveness and version are one action rather than two (the redis artifact splits
// `pinged` and `role-probed`) because one row of `system.local` carries both: a
// second action would be a second connection fetching data the first already had.
package main

import (
	"context"

	"google.golang.org/protobuf/types/known/structpb"
)

// localQuery reads the coordinator's own row. `system.local` always has exactly one.
const localQuery = `SELECT release_version, cluster_name, data_center, rack, host_id FROM system.local`

// applyPinged probes the node the driver connected to. Read-only, changed=false by
// design: whether "up" is good enough is the scenario's call, through retry/until/
// failed_when over register.self.ok.
//
// Opening the session has already proved TCP, the CQL handshake and — when
// credentials were given — authentication, so this query is what proves the node is
// serving rather than merely accepting connections.
func (m *CassandraModule) applyPinged(ctx context.Context, stream eventStream, s cassSession, params *structpb.Struct) error {
	password := stringOrEmpty(params.GetFields()["password"])

	rows, err := s.Query(ctx, localQuery)
	if err != nil {
		return sendFailure(stream, "system.local: "+redactError(err, password))
	}
	if len(rows) == 0 {
		return sendFailure(stream, "system.local returned no row — the coordinator did not answer for itself")
	}
	row := rows[0]

	return sendOutcome(stream, false, "cassandra is up", map[string]any{
		"ok":              true,
		"release_version": rowString(row, "release_version"),
		"cluster_name":    rowString(row, "cluster_name"),
		"data_center":     rowString(row, "data_center"),
		"rack":            rowString(row, "rack"),
		"host_id":         rowString(row, "host_id"),
	})
}

// validatePinged — a probe needs nothing but somewhere to connect to.
func validatePinged(f map[string]*structpb.Value) []string {
	return validateConnect(f)
}
