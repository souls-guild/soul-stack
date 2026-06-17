package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	vaultapi "github.com/hashicorp/vault/api"
)

// TokenRenewer — фоновый авто-продлеватель токена keeper-а.
//
// Без него short-lived токен (approle в staging/prod) протухает по TTL →
// все vault-резолвы (CEL vault(), vault:-ref, core.vault.kv-read, чтение
// JWT-signing-key) начинают падать → отказ Operator API. Watcher держит
// токен живым, пока процесс жив.
//
// Деградация: root/static dev-токен (частый случай локально) не renewable —
// тогда watcher не стартует (warn в лог), keeper работает дальше.
//
// Lifecycle: Start запускает goroutine под переданным ctx; на ctx.Done()
// (SIGTERM) goroutine останавливает vault-watcher и выходит. Stop() даёт
// синхронный wait — caller дожидается выхода goroutine в shutdown-defer-е,
// симметрично reaper-runner-у в keeper run.
type TokenRenewer struct {
	c      *vaultapi.Client
	logger *slog.Logger

	watcher *vaultapi.LifetimeWatcher
	done    chan struct{}
}

// StartTokenRenewer проверяет renewable-флаг текущего токена и, если он
// renewable, запускает фоновый LifetimeWatcher. Возвращает *TokenRenewer
// с методом Stop для graceful-остановки, либо nil, если watcher не нужен
// (non-renewable токен — штатная деградация, не ошибка).
//
// Ошибку возвращает только при сбое lookup-self или конструирования
// watcher-а — caller (keeper run) решает, фатально это или warn. Сам
// факт «токен не renewable» ошибкой НЕ считается.
func (c *Client) StartTokenRenewer(ctx context.Context, logger *slog.Logger) (*TokenRenewer, error) {
	if logger == nil {
		logger = slog.Default()
	}

	self, err := c.c.Auth().Token().LookupSelfWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("vault: token lookup-self: %w", err)
	}

	renewable, err := self.TokenIsRenewable()
	if err != nil {
		return nil, fmt.Errorf("vault: parse token renewable flag: %w", err)
	}
	if !renewable {
		// Root/static dev-токен — штатный кейс. Не падаем, просто живём
		// без auto-renew. В проде это сигнал, что approle настроен не
		// renewable — отсюда warn, а не info.
		logger.Warn("vault: token not renewable, auto-renew disabled")
		return nil, nil
	}

	// LifetimeWatcher продлевает по Secret. Для токена собираем Secret с
	// Auth.ClientToken — иначе watcher не знает, какой именно токен renew-ить.
	// RenewBehaviorIgnoreErrors: транзиентные сетевые ошибки не валят watcher
	// мгновенно — он добивает попытки до lifetime-threshold, после чего
	// штатно выходит через DoneCh (caller получит warn и keeper останется на
	// последнем валидном токене до его реального истечения).
	secret := &vaultapi.Secret{
		Auth: &vaultapi.SecretAuth{
			ClientToken:   c.c.Token(),
			Renewable:     true,
			LeaseDuration: selfTokenTTLSeconds(self),
		},
	}
	watcher, err := c.c.NewLifetimeWatcher(&vaultapi.LifetimeWatcherInput{
		Secret:        secret,
		RenewBehavior: vaultapi.RenewBehaviorIgnoreErrors,
	})
	if err != nil {
		return nil, fmt.Errorf("vault: new lifetime watcher: %w", err)
	}

	r := &TokenRenewer{
		c:       c.c,
		logger:  logger,
		watcher: watcher,
		done:    make(chan struct{}),
	}

	logger.Info("vault: token auto-renew enabled")
	go r.run(ctx)
	return r, nil
}

// run крутит vault-watcher до ctx.Done() либо до его собственного выхода
// (DoneCh — токен дошёл до lifetime-threshold и продлить дальше нельзя).
func (r *TokenRenewer) run(ctx context.Context) {
	defer close(r.done)

	go r.watcher.Start()
	defer r.watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			// Graceful shutdown: останавливаем watcher (defer) и выходим.
			r.logger.Info("vault: token auto-renew stopping (shutdown)")
			return

		case err := <-r.watcher.DoneCh():
			// Watcher завершился сам. err != nil — продление сорвалось;
			// err == nil — токен достиг threshold и renew не продлевает
			// (нечего больше делать). В обоих случаях токен скоро/уже
			// невалиден — это надо видеть в логах. Значение токена НЕ
			// логируем.
			if err != nil {
				r.logger.Error("vault: token auto-renew failed, token will expire", slog.Any("error", err))
			} else {
				r.logger.Warn("vault: token auto-renew exhausted (lease at threshold), token will expire")
			}
			return

		case renewal := <-r.watcher.RenewCh():
			// Успешное продление. Логируем только TTL, без токена.
			r.logger.Info("vault: token renewed",
				slog.Int("lease_duration_seconds", leaseDurationSeconds(renewal.Secret)))
		}
	}
}

// Stop блокирует до выхода фоновой goroutine. Caller вызывает её из
// shutdown-defer-а после отмены ctx. Безопасна при r == nil (когда
// watcher не стартовал — non-renewable токен).
func (r *TokenRenewer) Stop() {
	if r == nil {
		return
	}
	<-r.done
}

// leaseDurationSeconds достаёт оставшийся lease токена в секундах из
// renew-ответа watcher-а (RenewCh): там TTL приходит в Secret.Auth.LeaseDuration
// либо в top-level LeaseDuration. 0 — если данных нет (только лог). Для
// lookup-self ответа TTL лежит иначе (Data["ttl"]) — см. selfTokenTTLSeconds.
func leaseDurationSeconds(s *vaultapi.Secret) int {
	if s == nil {
		return 0
	}
	if s.Auth != nil && s.Auth.LeaseDuration > 0 {
		return s.Auth.LeaseDuration
	}
	return s.LeaseDuration
}

// selfTokenTTLSeconds достаёт оставшийся TTL токена из lookup-self ответа.
// Vault кладёт его в Data["ttl"] (JSON number), а Auth/top-level LeaseDuration
// в этом ответе нулевые — поэтому отдельная функция для seed-а watcher-а.
// 0 — если поля нет/распарсить нельзя (watcher продлит по первому renew и
// подставит реальный TTL сам, seed лишь подсказка для начального расписания).
func selfTokenTTLSeconds(s *vaultapi.Secret) int {
	if s == nil || s.Data == nil {
		return 0
	}
	raw, ok := s.Data["ttl"]
	if !ok {
		return 0
	}
	n, ok := raw.(json.Number)
	if !ok {
		return 0
	}
	ttl, err := n.Int64()
	if err != nil || ttl < 0 {
		return 0
	}
	return int(ttl)
}
