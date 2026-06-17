// Package artifact загружает git-артефакты Service-репозиториев на Keeper-сторону:
// клонирует/обновляет репозиторий, материализует immutable-снапшот по git-ref
// (ADR-007: ref = tag или branch, semver-range запрещён) и парсит корневой
// `service.yml` через нормативный `shared/config`-парсер.
//
// Снапшоты кешируются по `<cacheRoot>/<name>/<sha1>/`, где `sha1` — резолв ref
// в commit-hash. Тег immutable по своей природе; ветка резолвится в текущий tip
// при каждом [ServiceLoader.Load] (PM-decision: всегда fetch + checkout,
// throttle — отдельный slice). Снапшот не содержит `.git` — это чистое дерево
// файлов сервиса.
//
// Транспорт — pure Go (go-git): поддержаны `file://` (local-dev + тесты),
// `https://` и `ssh://`/`scp`-форма (auth через SSH-agent, Vault-auth —
// post-MVP). Зона по architect-recon slice .a.
package artifact

import "github.com/souls-guild/soul-stack/shared/config"

// ServiceRef — координаты Service-репозитория для загрузки.
//
// Name — kebab-case имя сервиса (совпадает с `service.yml → name`), используется
// как первый сегмент cache-пути. Git — URL репозитория (`file://`/`https://`/
// `ssh://`). Ref — git tag или branch (ADR-007); пустой Ref трактуется как
// `HEAD` репозитория по умолчанию.
type ServiceRef struct {
	Name string
	Git  string
	Ref  string
}

// ServiceArtifact — материализованный immutable-снапшот Service-репозитория на
// конкретном commit-е.
//
// LocalDir указывает на каталог снапшота (`<cacheRoot>/<name>/<sha1>`), готовый
// к чтению через [ServiceLoader.ReadFile]. Manifest — распарсенный корневой
// `service.yml`.
type ServiceArtifact struct {
	Ref      ServiceRef
	SHA1     string
	LocalDir string
	Manifest *config.ServiceManifest
}
