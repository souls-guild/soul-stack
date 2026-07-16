// Real connection for the community.mongo plugin via go-mongo-driver. Split from
// impl.go so L0 tests can use a fake mongoConn (without a live mongod).
//
// URI is built PROGRAMMATICALLY through options (NOT string interpolation of
// credentials: password in a URI string would require URL escaping and risk
// leaking into driver logs). host:port is options.SetHosts; auth is
// options.SetAuth; TLS is options.SetTLSConfig from buildTLSConfig.
package main

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// defaultConnect opens a real connection to mongod and immediately pings primary
// (fast fail on unavailable instance, symmetric with redis defaultConnect).
func defaultConnect(ctx context.Context, cfg connConfig) (mongoConn, error) {
	opts := options.Client().SetHosts([]string{cfg.addr})

	// Auth only when credentials are set: empty username/password -> anonymous
	// connection (localhost-exception bootstrap of first admin; see user.go).
	if cfg.username != "" || cfg.password != "" {
		authDB := cfg.authDB
		if authDB == "" {
			authDB = "admin"
		}
		opts.SetAuth(options.Credential{
			AuthSource: authDB,
			Username:   cfg.username,
			Password:   cfg.password,
		})
	}

	// TLS: when tls=true the driver connects over TLS. buildTLSConfig returns
	// nil,nil when TLS is disabled -> SetTLSConfig(nil) leaves plaintext.
	tlsCfg, err := buildTLSConfig(cfg.tls)
	if err != nil {
		return nil, err
	}
	if tlsCfg != nil {
		opts.SetTLSConfig(tlsCfg)
	}

	client, err := mongo.Connect(ctx, opts)
	if err != nil {
		return nil, err
	}
	// Ping primary: confirms TCP + (with auth) handshake. On localhost-exception
	// no-auth connection, Ping succeeds (ping itself does not require auth).
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		_ = client.Disconnect(ctx)
		return nil, err
	}
	return &realConn{c: client}, nil
}

// realConn wraps *mongo.Client as mongoConn.
type realConn struct {
	c *mongo.Client
}

func (r *realConn) Ping(ctx context.Context) error {
	return r.c.Ping(ctx, readpref.Primary())
}

// RunCommand executes a command in db and returns raw bson response. Command error
// (for example Unauthorized) arrives as mongo.CommandError and stays typed
// (errors.As in isAuthError recognizes it).
func (r *realConn) RunCommand(ctx context.Context, db string, cmd bson.D) (bson.Raw, error) {
	return r.c.Database(db).RunCommand(ctx, cmd).Raw()
}

func (r *realConn) Close(ctx context.Context) error {
	return r.c.Disconnect(ctx)
}
