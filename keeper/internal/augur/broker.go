package augur

// Vault-broker MVP-1 (delegate=false, augur.md §2.1 / §5.3): по уже
// РАЗРЕШЁННОМУ запросу ([Resolve] вернул Decision{Allowed:true}) Keeper сам
// читает Vault KV и заворачивает результат в google.protobuf.Struct для
// AugurReply.inline_data. На Soul внешний токен/credential не попадает.

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"
)

// KVReader — узкая поверхность Vault-чтения, нужная брокеру: тот же ReadKV,
// что у render-pipeline / core.vault.kv-read (augur.md §2.1). Сужение до одного
// метода (вместо *vault.Client) даёт fake в unit-тестах без подъёма Vault;
// реальный [*vault.Client] удовлетворяет автоматически.
type KVReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// BrokerVault читает разрешённый KV-путь и возвращает данные как
// google.protobuf.Struct (augur.md §5.3, map-форма «как есть»: ключи секрета
// становятся ключами Struct).
//
// path — нормализованный logical-path из [Decision.Query] (уже прошёл
// ParseRef в Resolve; повторно не нормализуется). #field-проекция скаляра —
// post-Slice-B (здесь брокерим map целиком).
//
// Возвращает error на сбой чтения (Vault недоступен / путь исчез между
// resolve и read) — caller отдаёт AugurReply{status:ERROR}. Сам секрет в текст
// ошибки НЕ попадает (ReadFail оборачивает только path, который не секрет).
func BrokerVault(ctx context.Context, kv KVReader, path string) (*structpb.Struct, error) {
	data, err := kv.ReadKV(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("augur: broker vault read %q: %w", path, err)
	}
	s, err := structpb.NewStruct(data)
	if err != nil {
		// Значение секрета не сериализуется в Struct (тип вне JSON-модели).
		// Path в тексте — не секрет; само значение не логируем.
		return nil, fmt.Errorf("augur: broker vault encode %q: %w", path, err)
	}
	return s, nil
}
