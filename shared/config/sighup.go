package config

// SIGHUP-handler под [ADR-021](docs/architecture.md) (b): file-edit-path
// в MVP закрывается перечитыванием конфига по SIGHUP. inotify/fanotify —
// явно post-MVP.
//
// `WatchSIGHUP` — library API: бинари `keeper`/`soul` подключают watcher
// и обрабатывают `ReloadResult` через slog (`config.reload_succeeded` /
// `config.reload_failed`) — это будет в M0.4 / отдельных bin-slice-ах.
// В этом slice бинари остаются stub-ами.

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// reloadChanBuf — buffer-длина channel-а ReloadResult. Один свободный слот:
// если caller не успевает читать, последний reload-результат хранится в
// канале, следующие посылки от handler-а делаются неблокирующе (`default`-
// ветка select-а) — handler никогда не блокируется на slow consumer-е.
const reloadChanBuf = 1

// sighupChanBuf — buffer-длина внутреннего signal-канала. `signal.Notify`
// требует buffered-канал, иначе runtime может пропустить SIGHUP при
// одновременной обработке предыдущего. Один — стандартная рекомендация
// из `os/signal` godoc.
const sighupChanBuf = 1

// WatchSIGHUP запускает goroutine, которая на каждый SIGHUP вызывает
// `store.Reload(ctx, "signal")` и публикует `ReloadResult` в возвращаемый
// канал.
//
// Семантика:
//
//   - Канал buffered на 1. Если caller не успевает прочитать предыдущий
//     ReloadResult, последующая публикация делается неблокирующе и
//     теряется (не накапливаем очередь reload-ов: имеет смысл только
//     последний, файл всегда читается заново).
//   - На `ctx.Done()` handler делает `signal.Stop` (unregister), затем
//     `close(out)`. Caller может полагаться на закрытие канала как
//     сигнал завершения watcher-а.
//   - Несколько одновременных `WatchSIGHUP` для одного процесса допустимы:
//     каждый регистрирует свой signal-канал через `signal.Notify`, runtime
//     раздаёт SIGHUP во все зарегистрированные каналы.
//   - SIGHUP-сигнал доступен на unix-системах; на Windows константа
//     `syscall.SIGHUP` существует для совместимости, но реального
//     SIGHUP-источника нет — watcher просто не получит уведомлений.
func WatchSIGHUP[T any](ctx context.Context, store *Store[T]) <-chan ReloadResult {
	// Fail-fast в стеке вызывающего: nil-store — гарантированный footgun
	// (например, `LoadKeeperStore` вернул `(nil, diags, err)` на missing
	// конфиг, оператор забыл проверить err). Без этой проверки panic
	// прилетела бы асинхронно из watcher-goroutine при первом SIGHUP.
	if store == nil {
		panic("config.WatchSIGHUP: store is nil")
	}

	out := make(chan ReloadResult, reloadChanBuf)

	sig := make(chan os.Signal, sighupChanBuf)
	signal.Notify(sig, syscall.SIGHUP)

	go func() {
		defer close(out)
		defer signal.Stop(sig)

		for {
			select {
			case <-ctx.Done():
				return
			case <-sig:
				res := store.Reload(ctx, ReloadSourceSignal)
				select {
				case out <- res:
				default:
					// Caller не успевает читать: предыдущий результат
					// ещё в канале. Пропускаем — следующий SIGHUP
					// перечитает файл заново.
				}
			}
		}
	}()

	return out
}
