// Реальный коннект плагина community.mongo через go-mongo-driver. Отделён от
// impl.go, чтобы L0-тесты работали с фейковым mongoConn (без живого mongod).
//
// URI собирается ПРОГРАММНО через options (НЕ строковая интерполяция credential —
// пароль в URI-строке требовал бы URL-экранирования и рисковал утечь в лог
// драйвера). host:port — options.SetHosts; auth — options.SetAuth; TLS —
// options.SetTLSConfig из buildTLSConfig.
package main

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// defaultConnect открывает реальный коннект к mongod и сразу пингует primary
// (быстрый fail на недоступном инстансе, симметрия с redis defaultConnect).
func defaultConnect(ctx context.Context, cfg connConfig) (mongoConn, error) {
	opts := options.Client().SetHosts([]string{cfg.addr})

	// Auth только при заданных кредлах: пустой username/password → анонимный
	// коннект (localhost-exception bootstrap первого admin — см. user.go).
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

	// TLS: при tls=true драйвер коннектится по TLS. buildTLSConfig возвращает
	// nil,nil когда TLS выключен → SetTLSConfig(nil) оставляет plaintext.
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
	// Ping primary: подтверждает TCP + (при auth) handshake. На localhost-exception
	// no-auth-коннекте Ping проходит (сам ping auth не требует).
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		_ = client.Disconnect(ctx)
		return nil, err
	}
	return &realConn{c: client}, nil
}

// realConn — обёртка *mongo.Client под mongoConn.
type realConn struct {
	c *mongo.Client
}

func (r *realConn) Ping(ctx context.Context) error {
	return r.c.Ping(ctx, readpref.Primary())
}

// RunCommand выполняет команду в БД db, возвращает сырой bson-ответ. Ошибка
// команды (напр. Unauthorized) приходит как mongo.CommandError — сохраняется
// типизированной (errors.As в isAuthError её распознаёт).
func (r *realConn) RunCommand(ctx context.Context, db string, cmd bson.D) (bson.Raw, error) {
	return r.c.Database(db).RunCommand(ctx, cmd).Raw()
}

func (r *realConn) Close(ctx context.Context) error {
	return r.c.Disconnect(ctx)
}
