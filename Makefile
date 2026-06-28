# Makefile для Soul Stack. POSIX-совместимые таргеты для macOS-dev и Linux-CI.
#
# protoc-плагины ставятся через `go install` и попадают в $(go env GOPATH)/bin,
# которого может не быть в PATH. Прокидываем явно, чтобы `protoc --go_out`
# нашёл `protoc-gen-go` и `protoc-gen-go-grpc`.

SHELL := /bin/sh

GOPATH_BIN := $(shell go env GOPATH)/bin
export PATH := $(GOPATH_BIN):$(PATH)

# Keeper-протоколы и plugin-протоколы.
# Keeper живёт в общем модуле proto/ (ADR-011) → один вызов protoc с
# proto_path=proto и output=proto/gen/go/. Plugin — отдельный вложенный
# go.mod-подмодуль → второй вызов protoc, свой proto_path/output.
KEEPER_PROTO_ROOT := proto
KEEPER_PROTO_OUT  := proto/gen/go
KEEPER_PROTO_FILES := $(shell find $(KEEPER_PROTO_ROOT)/keeper -name '*.proto')

PLUGIN_PROTO_ROOT := proto/plugin
PLUGIN_PROTO_OUT  := proto/plugin/gen/go
PLUGIN_PROTO_FILES := $(shell find $(PLUGIN_PROTO_ROOT)/v1 -name '*.proto')

# govulncheck — supply-chain CI-гейт (орг-рекомендация ИБ-аудита до беты).
# Бинарь ставится через `go install` в $(GOPATH)/bin (паттерн protoc-плагинов).
# Версия pinned — float версии всплыл бы как нестабильность гейта. Полный путь к
# бинарю (macOS-make минует exported PATH для простых recipe-строк).
GOVULNCHECK_VERSION := v1.3.0
GOVULNCHECK := $(GOPATH_BIN)/govulncheck

MODULES := proto proto/plugin shared sdk keeper soul soul-lint soulctl

# Каталог для собранных бинарей относительно корня каждого модуля с `main`.
# Покрывается `.gitignore` (`*/bin/`).
BIN_DIR := bin

# Путь к собранному офлайн-линтеру (используется таргетом `lint`).
LINT_BIN := soul-lint/$(BIN_DIR)/soul-lint

# Путь к собранному L0-runner-у (используется таргетом `trial`). Бинарь собирается
# в рамках `build`, как и soul-lint.
TRIAL_BIN := keeper/$(BIN_DIR)/soul-trial

# Сервисы, ИСКЛЮЧЁННЫЕ из гейтового прогона `trial` (см. таргет). Это НЕ «зелёный
# по умолчанию» — список явный и громкий: каждый skip печатается с причиной, чтобы
# исключение было видно в логе CI и не маскировало новый регресс. Сейчас пуст:
# единственный обитатель (redis-monitored) удалён вместе с сервисом при
# redis-консолидации.
TRIAL_SKIP :=

# Версия сборки. По умолчанию выводится из git: ближайший тег + короткий хеш,
# суффикс `-dirty` при незакоммиченных правках (`git describe`). На голом
# чекауте без тегов выпадет короткий хеш (`--always`). Переопределяется снаружи:
# `make build VERSION=v1.2.3` (так делает release-pipeline).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)

# ldflags-инъекция версии. `-X <pkg>.<var>=<value>` перезаписывает package-level
# string-переменную на этапе линковки — без правки исходника. ВАЖНО: внутри
# собираемого бинаря entrypoint-пакет адресуется как `main`, а НЕ как его
# import-path — поэтому путь к переменной `main.<var>`, а не полный
# `github.com/.../cmd/<x>.<var>` (последний линкер молча игнорирует — строка
# попадает в data-секцию, но к символу не привязывается). Каждый бинарь
# собирается отдельной `go build ./cmd/<x>`, в каждой ровно один `main` — `-X
# main.<var>` однозначен. Один git-тег = версия всех модулей (ADR-011), поэтому
# $(VERSION) общий. Имя version-переменной у каждого своё (исторически):
# `soul` → soulVersion, `keeper` → version, `soulctl` → soulctlVersion.
# `soul-lint` version-переменной не имеет (офлайн-линтер, версию нигде не печатает).
SOUL_LDFLAGS := -X main.soulVersion=$(VERSION)
KEEPER_LDFLAGS := -X main.version=$(VERSION)
SOULCTL_LDFLAGS := -X main.soulctlVersion=$(VERSION)

# --- Release/packaging ---
# Корень build-артефактов (SBOM, нативные пакеты). Целиком в .gitignore (dist/) —
# артефакты сборки не коммитим.
DIST_DIR := dist
SBOM_DIR := $(DIST_DIR)/sbom
PKG_DIR  := $(DIST_DIR)/pkg

# Целевая Linux-архитектура нативных пакетов (deb/rpm всегда под Linux).
# Переопределяется снаружи: `make pkg PKG_ARCH=arm64`. nfpm читает ${ARCH} из
# окружения для ${ARCH}-подстановки в deploy/nfpm/*.yaml.
PKG_ARCH ?= amd64

# Имя прод-образа keeper (таргет docker-keeper). Локальный тег по умолчанию —
# `soul-stack/keeper`; оператор перетегирует под свой registry перед push
# (`docker tag soul-stack/keeper:$(VERSION) <registry>/keeper:$(VERSION)`) либо
# собирает сразу под него: `make docker-keeper KEEPER_IMAGE=<registry>/keeper`.
# Тег версии — общий $(VERSION) (git describe / release-override), он же уходит в
# ldflags бинаря и OCI-метку образа.
KEEPER_IMAGE ?= soul-stack/keeper

.PHONY: gen build build-soulctl build-linux test test-plugins test-race test-integration e2e e2e-live e2e-k8s docker-build-keeper docker-build-soul docker-keeper tidy check check-fmt vet check-gen check-doc-links check-vuln lint trial dev-up dev-down dev-stop dev-reset dev-provision dev-smoke dev-keeper dev-jwt dev-souls dev-web dev-stand gen-openapi check-openapi check-template sync-webui check-webui sbom pkg sign stress load-test help

gen: gen-openapi
	@mkdir -p $(KEEPER_PROTO_OUT) $(PLUGIN_PROTO_OUT)
	@if [ -z "$(KEEPER_PROTO_FILES)" ]; then \
		echo "no .proto files under $(KEEPER_PROTO_ROOT)/keeper"; \
	else \
		echo "protoc keeper: $(KEEPER_PROTO_FILES)"; \
		protoc \
			--proto_path=$(KEEPER_PROTO_ROOT) \
			--go_out=$(KEEPER_PROTO_OUT) \
			--go_opt=paths=source_relative \
			--go-grpc_out=$(KEEPER_PROTO_OUT) \
			--go-grpc_opt=paths=source_relative \
			$(KEEPER_PROTO_FILES); \
	fi
	@if [ -z "$(PLUGIN_PROTO_FILES)" ]; then \
		echo "no .proto files under $(PLUGIN_PROTO_ROOT)/v1"; \
	else \
		echo "protoc plugin: $(PLUGIN_PROTO_FILES)"; \
		protoc \
			--proto_path=$(PLUGIN_PROTO_ROOT) \
			--go_out=$(PLUGIN_PROTO_OUT) \
			--go_opt=paths=source_relative \
			--go-grpc_out=$(PLUGIN_PROTO_OUT) \
			--go-grpc_opt=paths=source_relative \
			$(PLUGIN_PROTO_FILES); \
	fi

# gen-openapi — committed docs/keeper/openapi.yaml как ПРОИЗВОДНЫЙ huma-генерат
# (OpenAPI 3.1, для UI-vendor + git-ревью). Источник правды — huma-агрегатор в
# коде (HumaFullSpecYAML); рукописи openapi.yaml больше нет. Запись делает
# generate-тест в пакете api под GEN_OPENAPI=1 (без отдельного cmd-бинаря).
# `-count=1` отключает кеш (тест пишет файл — кешировать нечего).
gen-openapi:
	@echo "huma-dump -> $(OPENAPI_COMMITTED)"
	@GEN_OPENAPI=1 go test ./keeper/internal/api/ -run TestCommittedOpenAPI_NoDrift -count=1 >/dev/null

# Сборка трёх бинарей (`keeper`, `soul`, `soul-lint`) с явным `-o <module>/bin/<name>`,
# чтобы артефакты не падали в корень модуля и не цеплялись git-ом.
# Library-модули (`proto`, `proto/plugin`, `shared`, `sdk`) собираются без
# `-o` через `go build ./...` для верификации компиляции; они не порождают
# исполняемых файлов. Модули без go-пакетов (например, `proto/plugin/` до
# появления первых .proto под него) пропускаются по `go list ./...`, иначе
# `go build ./...` падает с "matched no packages".
build:
	@for m in proto proto/plugin shared sdk; do \
		if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then \
			echo "skip $$m (no Go packages)"; \
			continue; \
		fi; \
		echo "go build ./... in $$m"; \
		(cd $$m && go build ./...) || exit 1; \
	done
	@echo "go build -o keeper/$(BIN_DIR)/keeper ./cmd/keeper in keeper (VERSION=$(VERSION))"
	@cd keeper && go build -ldflags '$(KEEPER_LDFLAGS)' -o $(BIN_DIR)/keeper ./cmd/keeper
	@echo "go build -o keeper/$(BIN_DIR)/soul-trial ./cmd/soul-trial in keeper"
	@cd keeper && go build -o $(BIN_DIR)/soul-trial ./cmd/soul-trial
	@echo "go build -o soul/$(BIN_DIR)/soul ./cmd/soul in soul (VERSION=$(VERSION))"
	@cd soul && go build -ldflags '$(SOUL_LDFLAGS)' -o $(BIN_DIR)/soul ./cmd/soul
	@echo "go build -o soul-lint/$(BIN_DIR)/soul-lint ./cmd/soul-lint in soul-lint"
	@cd soul-lint && go build -o $(BIN_DIR)/soul-lint ./cmd/soul-lint
	@$(MAKE) build-soulctl

# Сборка клиентского CLI оператора (см. docs/naming-rules.md → soulctl).
# Каркас на cobra без реализованных тел команд — отдельный таргет, чтобы можно
# было собирать независимо от keeper/soul (другой жизненный цикл, без depend-инфры).
build-soulctl:
	@echo "go build -o soulctl/$(BIN_DIR)/soulctl ./cmd/soulctl in soulctl (VERSION=$(VERSION))"
	@cd soulctl && go build -ldflags '$(SOULCTL_LDFLAGS)' -o $(BIN_DIR)/soulctl ./cmd/soulctl

# Модули без go-пакетов пропускаются по `go list ./...` — то же правило,
# что и в `build`. На текущем этапе под фильтр попадает `proto/plugin/`.
#
# `-count=1` отключает go-test-кеш. КРИТИЧНО для гейта, не оптимизация: go кеширует
# результат пакета по хешу его `.go`-исходников (+ объявленных входов), но НЕ по
# содержимому произвольных файлов, прочитанных в рантайме через `os.ReadFile` по
# пути (напр. keeper/internal/render рендерит examples/destiny/*/templates/*.tmpl).
# Без `-count=1` правка такого .tmpl (без правки .go-теста) оставляет результат
# `(cached) ok` — красный тест проходит гейт молча (так сломанный redis-render
# проехал в f40da00: conf_dir/data_dir-волна сменила .tmpl, не тронув .go-тест).
# Тот же приём уже стоит в test-plugins / test-integration / gen-openapi.
test:
	@for m in $(MODULES); do \
		if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then \
			echo "skip $$m (no Go packages)"; \
			continue; \
		fi; \
		echo "go test -count=1 ./... in $$m"; \
		(cd $$m && go test -count=1 ./...) || exit 1; \
	done

# Тесты community-плагинов examples/module/* — каждый ОТДЕЛЬНЫЙ go.mod ВНЕ go.work
# (ADR-016: community-плагины тянут ядро как обычную зависимость, не workspace-member).
# Поэтому гоняются с `GOWORK=off` per-module, а НЕ через MODULES-список `make test`
# (тот их вообще не видит). Покрывает в т.ч. security-guard на маскинг секретов
# плагина community.redis (59 тест-функций), которые иначе остались бы вне гейта.
#
# Skip-on-unresolvable: cloud/ssh-плагины (soul-cloud-*/soul-ssh-*) standalone-offline
# не резолвятся (workspace-пины go.mod расходятся со standalone-tidy, нужна сеть).
# `go list ./...` под GOWORK=off у них падает → пропускаем ГРОМКО с warning (тот же
# приём, что `go list` empty → skip в `test`/`vet`). Это НЕ молчаливое прощение: skip
# печатается, а резолвящийся offline плагин (community.redis) под него не попадает —
# его регресс гейт ловит. Merge()-тесты НЕ здесь: они в shared/cel (workspace,
# покрыты `make test`), дублировать не нужно.
# `-count=1` — без кеша (плагин может зависеть от внешнего fake-состояния).
test-plugins:
	@for d in examples/module/*/go.mod; do \
		[ -e "$$d" ] || continue; \
		m=$$(dirname "$$d"); \
		if ! (cd "$$m" && GOWORK=off go list ./... >/dev/null 2>&1); then \
			echo "skip $$m (standalone-offline не резолвится — GOWORK=off go list упал; cloud/ssh-плагин или go.mod-drift)"; \
			continue; \
		fi; \
		echo "GOWORK=off go test -count=1 ./... in $$m"; \
		(cd "$$m" && GOWORK=off go test -count=1 ./...) || exit 1; \
	done
	@echo "test-plugins: community-плагины (resolvable offline) зелёные"

# Прогон тестов с race detector — отдельным таргетом, чтобы обычный `make test`
# оставался быстрым. CI должен гонять оба: `test` (быстро, на каждый push) и
# `test-race` (отдельным шагом перед merge).
test-race:
	@for m in $(MODULES); do \
		if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then \
			echo "skip $$m (no Go packages)"; \
			continue; \
		fi; \
		echo "go test -race ./... in $$m"; \
		(cd $$m && go test -race ./...) || exit 1; \
	done

# Интеграционные тесты под build-tag `integration` (testcontainers-go).
# Отдельный таргет — `make test` не требует docker, остаётся быстрым.
# Файлы с тегом `integration` НЕ собираются обычным `go test ./...`, поэтому
# тут передаём `-tags=integration` явно. `-count=1` отключает кеш Go-тестов
# (поднятый контейнер каждый раз новый — кешировать нечего).
test-integration:
	@for m in $(MODULES); do \
		if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then \
			echo "skip $$m (no Go packages)"; \
			continue; \
		fi; \
		echo "go test -tags=integration -race -count=1 ./... in $$m"; \
		(cd $$m && go test -tags=integration -race -count=1 ./...) || exit 1; \
	done

# L3a fast-loop E2E (ADR-039): рабочий harness — testcontainers (PG+Redis+Vault) +
# реальный Keeper-процесс + soul-stub с live gRPC-mTLS. Отдельный go-модуль
# tests/e2e/ под build-tag `e2e` (deps testcontainers не утекают в основные
# keeper/soul). НЕ входит в `check` (требует docker); деталь — tests/e2e/README.md.
e2e:
	@if [ -z "$$(cd tests/e2e && go list -tags=e2e ./... 2>/dev/null)" ]; then \
		echo "skip tests/e2e (no Go packages under build-tag e2e)"; \
	else \
		echo "go test -tags=e2e ./... in tests/e2e"; \
		(cd tests/e2e && go test -tags=e2e -timeout=10m ./...) || exit 1; \
	fi

# Cross-compile keeper+soul под Linux amd64 для real-soul-container (L3b, ADR-039).
# В отличие от `make pkg` (deb/rpm-build), здесь только два бинаря, в свои `bin/`
# с явным суффиксом `-linux-amd64`. CGO_ENABLED=0 — статическая сборка, без
# зависимости на libc внутри контейнера (Debian-12 совместим, но статика проще).
# Нативный `make build` не задеваем (macOS-разработчик собирает host-arch).
build-linux:
	@echo "GOOS=linux GOARCH=amd64 go build -o keeper/$(BIN_DIR)/keeper-linux-amd64 ./cmd/keeper in keeper (VERSION=$(VERSION))"
	@cd keeper && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags '$(KEEPER_LDFLAGS)' -o $(BIN_DIR)/keeper-linux-amd64 ./cmd/keeper
	@echo "GOOS=linux GOARCH=amd64 go build -o soul/$(BIN_DIR)/soul-linux-amd64 ./cmd/soul in soul (VERSION=$(VERSION))"
	@cd soul && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags '$(SOUL_LDFLAGS)' -o $(BIN_DIR)/soul-linux-amd64 ./cmd/soul

# L3b smoke-loop E2E (ADR-039): real-soul-binary в privileged Debian-12
# контейнере + Keeper-процесс на хосте. Требует docker и `make build-linux`
# (cross-compile linux-amd64 binary для mount в контейнер). НЕ входит в `check`
# (длительный — 5-15 мин на тест; runs nightly/on-demand).
#
# `-p 1` — serial (RAM-heavy: privileged-контейнеры с systemd + apt-install
# одновременно убьют ноутбук разработчика). Architect-рекомендация.
e2e-live: build-linux
	@if [ -z "$$(cd tests/e2e-live && go list -tags=e2e_live ./... 2>/dev/null)" ]; then \
		echo "skip tests/e2e-live (no Go packages under build-tag e2e_live)"; \
	else \
		echo "go test -tags=e2e_live ./... in tests/e2e-live"; \
		(cd tests/e2e-live && go test -tags=e2e_live -timeout=30m -p 1 ./...) || exit 1; \
	fi

# docker-build-keeper — собирает образ `keeper:e2e-k8s` для L3c kind-cluster.
# Переиспользует артефакт `make build-linux` (cross-compiled keeper-linux-amd64);
# single-stage Dockerfile поверх distroless-runtime. PM-decision: образ
# одноразовый, грузится в kind через `kind load docker-image`, в registry НЕ
# публикуется. Контекст сборки — корень репо (Dockerfile COPY-ит из
# `keeper/bin/keeper-linux-amd64`).
#
# Зависимость: `make build-linux` собирает linux-amd64 бинарь.
docker-build-keeper: build-linux
	@echo "docker build -t keeper:e2e-k8s -f tests/e2e-k8s/dockerfiles/keeper.Dockerfile ."
	@docker build -t keeper:e2e-k8s -f tests/e2e-k8s/dockerfiles/keeper.Dockerfile .

# docker-keeper — ПРОД-образ keeper для публикации в registry оператора. В
# отличие от docker-build-keeper (одноразовый kind-образ, single-stage от
# артефакта build-linux) — multi-stage самодостаточный билд из
# deploy/docker/keeper.Dockerfile: пинит golang-тулчейн, не зависит от состояния
# keeper/bin/, версия инжектится в бинарь (ldflags) и в OCI-метку
# (--build-arg VERSION). Тег — $(KEEPER_IMAGE):$(VERSION) (versioned, не latest:
# воспроизводимый rollback). Контекст сборки — корень репо.
#
# Дальше оператор сам: `docker tag $(KEEPER_IMAGE):$(VERSION) <registry>/keeper:$(VERSION)`
# → `docker push <registry>/keeper:$(VERSION)`. Bootstrap первого Архонта и
# прод-конфиг — deploy/README.md → «Keeper в проде».
#
# Требует docker в PATH (в отличие от build-linux/pkg). НЕ входит в `check`.
docker-keeper:
	@echo "docker build -t $(KEEPER_IMAGE):$(VERSION) --build-arg VERSION=$(VERSION) -f deploy/docker/keeper.Dockerfile ."
	@docker build -t $(KEEPER_IMAGE):$(VERSION) --build-arg VERSION='$(VERSION)' -f deploy/docker/keeper.Dockerfile .
	@echo "built $(KEEPER_IMAGE):$(VERSION) — перетегируйте под свой registry и push (см. deploy/README.md)"

# docker-build-soul — собирает образ `soul:e2e-k8s` для L3c kind-cluster
# (L3c-3+). Privileged systemd-PID-1 Debian-12 base (parity с L3b), bake-ит
# cross-compiled soul-linux-amd64 из артефакта `make build-linux`. Загружается
# в kind через `kind load docker-image soul:e2e-k8s` (harness DeploySoul).
#
# Контекст сборки — корень репо (Dockerfile COPY-ит из soul/bin/ и
# tests/e2e-k8s/manifests/soul/soul.service).
docker-build-soul: build-linux
	@echo "docker build -t soul:e2e-k8s -f tests/e2e-k8s/dockerfiles/soul.Dockerfile ."
	@docker build -t soul:e2e-k8s -f tests/e2e-k8s/dockerfiles/soul.Dockerfile .

# L3c k8s-loop E2E (ADR-039): kind-cluster + bitnami Helm (PG/Redis/Vault) +
# raw YAML Keeper/Soul. Требует docker и kind CLI в PATH; без них тесты
# скипаются (см. tests/e2e-k8s/harness/cluster.go::NewCluster pre-flight).
# НЕ входит в `check` (длительный: kind spin-up + helm-install + image-load,
# 5-15 мин на тест; runs weekly / pre-release).
#
# Зависимость: `make docker-build-keeper` + `make docker-build-soul` собирают
# образы keeper:e2e-k8s / soul:e2e-k8s, которые тесты L3c-2+ грузят в kind
# через `kind load docker-image`.
#
# `-p 1` — serial (RAM-heavy: каждый тест поднимает свой kind-cluster со
# своими PG/Redis/Vault через bitnami Helm; параллель убьёт ноутбук).
e2e-k8s: docker-build-keeper docker-build-soul
	@if [ -z "$$(cd tests/e2e-k8s && go list -tags=e2e_k8s ./... 2>/dev/null)" ]; then \
		echo "skip tests/e2e-k8s (no Go packages under build-tag e2e_k8s)"; \
	else \
		echo "go test -tags=e2e_k8s ./... in tests/e2e-k8s"; \
		(cd tests/e2e-k8s && go test -tags=e2e_k8s -timeout=30m -p 1 ./...) || exit 1; \
	fi

# `go mod tidy` на модуле без go-файлов выдаёт "no Go files" и фейлится,
# поэтому модули с пустым `go list ./...` так же пропускаются.
tidy:
	@for m in $(MODULES); do \
		if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then \
			echo "skip $$m (no Go packages)"; \
			continue; \
		fi; \
		echo "go mod tidy in $$m"; \
		(cd $$m && go mod tidy) || exit 1; \
	done

# Локальный dev-стек (docker-compose). См. `docs/dev/local-setup.md`.
# `dev/docker-compose.yml` поднимает весь обязательный контур: Postgres, Redis,
# Vault (dev-mode), OTel-collector и Jaeger.
#
# `dev-down` НЕ удаляет volume — данные `postgres_data` сохраняются между
# циклами `up/down`. Для полного сброса (миграция изменилась, БД в
# inconsistent state) — `make dev-reset`.
dev-up:
	@cd dev && docker compose up -d

# `dev-down` сначала гасит локальные keeper/soul-демоны dev-воркфлоу
# (см. `dev-stop`), потом инфру docker-compose. Иначе orphan-`keeper run`
# от прошлой сессии висит и держит порты (8080/8081/9090/9442/9443) —
# свежий запуск падает на `bind: address already in use`.
dev-down: dev-stop
	@cd dev && docker compose down

# Останавливает ЛОКАЛЬНЫЕ keeper/soul-демоны dev-воркфлоу (foreground-процессы
# из `docs/dev/local-setup.md` → `keeper run`/`soul run`). Pidfile dev-скрипты
# не пишут, поэтому матчим по СПЕЦИФИЧНОМУ паттерну с dev-конфигом
# (`keeper.dev.yml`/`soul.dev.yml`) — чужие keeper/soul с прод-конфигом
# не задеваются. `|| true` — не падать, если процессов нет.
dev-stop:
	@pkill -f 'keeper run.*keeper\.dev\.yml' || true
	@pkill -f 'soul run.*soul\.dev\.yml' || true
	@echo "dev-stop: локальные keeper/soul-демоны остановлены (если были)"

dev-reset:
	@cd dev && docker compose down -v && docker compose up -d

# Idempotent bootstrap-провижининг секретов и TLS-материала для local-dev.
# Скрипт безопасен к повторному запуску: каждый шаг проверяет своё состояние.
# Подробности — `docs/dev/local-setup.md`.
dev-provision:
	@bash dev/provision.sh

# Полный smoke-цикл: поднять стек → провижининг Vault/TLS → `keeper init` →
# seed service-реестра. Собирает keeper-бинарь перед запуском (зависим от
# Go-кода, поэтому не делаем shortcut через `keeper/bin/keeper`-as-is). JWT-файл
# оператора пишется в /tmp/keeper-dev/archon-alice.jwt — следующий запуск smoke
# упадёт на `keeper init` (operators registry уже не пуст) — для повторного
# прогона делать `make dev-reset && make dev-smoke`.
#
# Второй `dev-provision` — ПОСЛЕ `keeper init`: на свежей БД (dev-reset) схемы
# (service_registry / keeper_settings) ещё нет, первый provision-проход их seed
# пропускает (см. dev/provision.sh::seed_service_registry, шаг 10). Схему
# создаёт `keeper init` (migrate.Apply), поэтому реестр сервисов сеется только
# повторным provision-проходом. provision идемпотентен — двойной вызов безопасен;
# без этого шага single-pass `make dev-smoke` оставил бы пустой service-реестр
# (config-S4 убрал services[] из keeper.dev.yml — резолв читает только БД).
dev-smoke:
	@$(MAKE) dev-up
	@$(MAKE) dev-provision
	@cd keeper && go build -o $(BIN_DIR)/keeper ./cmd/keeper
	@./keeper/bin/keeper init \
		--archon=archon-alice \
		--config=dev/keeper.dev.yml \
		--credential-out=/tmp/keeper-dev/archon-alice.jwt
	@$(MAKE) dev-provision

# Рестарт keeper с ПОЛНЫМ dev-env (SOUL_STACK_ALLOW_FILE_REPOS=1 + writable
# cache-dirs): без него file://-резолв сервисов падает (502). Гасит старый
# keeper, чистит leader-leases, ждёт healthz 200. Если нет TLS-материала —
# подсказывает `make dev-provision`. Скрипт — dev/keeper-run.sh.
dev-keeper:
	@bash dev/keeper-run.sh

# Выпустить Archon-JWT для ad-hoc dev-API-вызовов (без `keeper init`). Ключ
# берётся из того же Vault KV, что и у keeper (НЕ хардкодится). Печатает ТОЛЬКО
# токен в stdout → `TOKEN=$$(make dev-jwt)`. Параметры — через переменные:
# `make dev-jwt AID=archon-keyset ROLES='["keyset-demo"]' TTL=3600`.
AID ?= archon-alice
ROLES ?= ["cluster-admin"]
TTL ?= 43200
dev-jwt:
	@AID='$(AID)' ROLES='$(ROLES)' TTL='$(TTL)' bash dev/mint-jwt.sh

# Переподнять локальный флот souls по реестру БД: на каждый sid пишет soul.yml
# (если нет), онбордит при отсутствии seed, (пере)запускает `soul run`. Covens
# в БД сохраняются (заново НЕ регистрируем). Скрипт — dev/souls-up.sh.
dev-souls:
	@bash dev/souls-up.sh

# Vite dev-сервер web-репо (companion ../soul-stack-web). `--host` обязателен,
# иначе vite слушает только [::1] и 127.0.0.1:5173 отказывает. Путь web —
# переменная WEB_DIR. Скрипт — dev/web-run.sh.
WEB_DIR ?= ../soul-stack-web
dev-web:
	@WEB_DIR='$(WEB_DIR)' bash dev/web-run.sh

# Полный подъём dev-стенда одной командой: provision → keeper → souls → web.
# Удобно после рестарта / смены суток (чистка /tmp). В конце — сводка + напоминание
# про `make dev-jwt` для токена.
dev-stand:
	@$(MAKE) dev-provision
	@$(MAKE) dev-keeper
	@$(MAKE) dev-souls
	@$(MAKE) dev-web
	@echo ""
	@echo "=== dev-стенд поднят ==="
	@echo "keeper:  healthz http://127.0.0.1:8080/healthz | openapi :8080 | mcp :8081 | metrics :9090"
	@echo "souls:   статусы — docker exec soul-stack-postgres psql -U keeper -d keeper -c 'SELECT status, count(*) FROM souls GROUP BY status'"
	@echo "web:     http://127.0.0.1:5173"
	@echo "токен:   TOKEN=\$$(make dev-jwt)   (параметры: AID=... ROLES='[...]' TTL=...)"

# OpenAPI committed-снимок: источник правды — huma-агрегатор в коде
# (HumaFullSpecYAML, served на GET /openapi.yaml). docs/keeper/openapi.yaml —
# ПРОИЗВОДНЫЙ дамп (для UI-vendor + git-ревью), а НЕ рукопись. Два таргета:
#
#   gen-openapi   — перезаписать committed-файл текущим huma-дампом (после правки
#                   huma-домена). Определён выше рядом с gen.
#   check-openapi — drift-guard (CI): committed-файл == huma-дамп байт-в-байт;
#                   ошибка означает «забыли make gen-openapi». Делегирует тому же
#                   generate-тесту (без GEN_OPENAPI он сверяет, не пишет).
OPENAPI_COMMITTED := docs/keeper/openapi.yaml

check-openapi:
	@echo "openapi drift-guard: $(OPENAPI_COMMITTED) == huma-dump"
	@go test ./keeper/internal/api/ -run TestCommittedOpenAPI_NoDrift -count=1 >/dev/null || { \
		echo ""; \
		echo "openapi.yaml drift: committed $(OPENAPI_COMMITTED) расходится с huma-дампом"; \
		echo "run 'make gen-openapi' to regenerate the committed snapshot"; \
		exit 1; \
	}
	@echo "openapi.yaml: committed-снимок совпадает с huma-дампом"

# Plugin-template self-serve: дерево шаблона plugin-авторов живёт source-of-truth
# в companion-repo ../soul-stack-plugins/soul-mod-template/, а core embed-ит копию
# через soul-lint/internal/plugininit/template/ (go:embed). Drift между деревьями
# отлавливается так же, как openapi:
#
#   sync-template.sh — обновить копию из companion (rsync --delete зеркалит всё дерево).
#   check-template   — CI-guard на расхождение; ошибка означает «забыли sync после
#                      правки template в companion».
#
# Companion — ОТДЕЛЬНЫЙ репозиторий: на чужой машине/CI его может не быть. Если SRC
# отсутствует — гейт не падает (иначе сломал бы `make check` без companion), а
# пропускается с warning. Drift ловится только когда companion доступен рядом.
TEMPLATE_SRC := ../soul-stack-plugins/soul-mod-template
TEMPLATE_DST := soul-lint/internal/plugininit/template

check-template:
	@if [ ! -d "$(TEMPLATE_SRC)" ]; then \
		echo "companion soul-stack-plugins not found, skipping template-drift check"; \
	elif ! diff -r -q $(TEMPLATE_SRC) $(TEMPLATE_DST) >/dev/null; then \
		echo "plugin-template drift detected:"; \
		diff -r $(TEMPLATE_SRC) $(TEMPLATE_DST) || true; \
		echo ""; \
		echo "run 'scripts/sync-template.sh' to update the embedded copy"; \
		exit 1; \
	else \
		echo "plugin-template: copy in sync"; \
	fi

# Embed-UI vendoring: собранный build-снапшот UI живёт source-of-truth в
# companion-repo ../soul-stack-web/dist/, а core embed-ит копию через
# keeper/internal/webui/assets/ (go:embed, раздаётся keeper-ом на /ui, ADR-055).
# Drift между ними отлавливается так же, как plugin-template:
#
#   sync-webui.sh — обновить копию из companion (rsync --delete зеркалит dist/,
#                   при отсутствии dist/ — собирает companion `npm run build`).
#   check-webui   — CI-guard на расхождение; ошибка означает «забыли sync после
#                   пересборки UI в companion».
#
# Companion — ОТДЕЛЬНЫЙ репозиторий: на чужой машине/CI его может не быть. Если SRC
# отсутствует — гейт не падает (иначе сломал бы `make check` без companion), а
# пропускается с warning. Drift ловится только когда companion доступен рядом.
WEBUI_SRC := ../soul-stack-web/dist
WEBUI_DST := keeper/internal/webui/assets

sync-webui:
	@bash scripts/sync-webui.sh

check-webui:
	@if [ ! -d "$(WEBUI_SRC)" ]; then \
		echo "companion soul-stack-web/dist not found, skipping webui-drift check"; \
	elif ! diff -r -q $(WEBUI_SRC) $(WEBUI_DST) >/dev/null; then \
		echo "embed-UI drift detected:"; \
		diff -r $(WEBUI_SRC) $(WEBUI_DST) || true; \
		echo ""; \
		echo "run 'make sync-webui' (or 'scripts/sync-webui.sh') to update the embedded copy"; \
		exit 1; \
	else \
		echo "embed-UI: copy in sync"; \
	fi

# --- Release/packaging ---
# Эти таргеты аддитивны: в `check` НЕ входят (требуют внешний tooling, который в
# dev-окружении может быть не установлен). Артефакты пишутся в dist/ (gitignored).

# CycloneDX SBOM по трём релизным бинарям через cyclonedx-gomod (go-tool), режим
# `app` — SBOM именно того, что слинковано в бинарь (точнее для prod-readiness,
# чем граф всего модуля). Файл на бинарь в dist/sbom/. Инструмент в PATH не входит
# автоматически; если не найден — печатаем go install-подсказку и выходим с ошибкой
# (не silently). `-licenses` подтягивает лицензии зависимостей, `-json` —
# машиночитаемый CycloneDX, `-main` указывает main-пакет внутри модуля.
#
# Почему `app`, а не `mod`: репо — go.work. Режим `mod` с активным workspace для
# ЛЮБОГО модуля строит SBOM корневого (component.name всегда первый модуль), а с
# GOWORK=off модули с локальными cross-module-зависимостями (keeper/soul/shared)
# не резолвятся (go тянет pseudo-version из сети). Режим `app` понимает workspace
# и резолвит локальные replace корректно. SBOM трёх бинарей покрывает граф всех
# library-модулей (proto/sdk/shared) транзитивно.
SBOM_APPS := keeper:./cmd/keeper soul:./cmd/soul soul-lint:./cmd/soul-lint

sbom:
	@if ! command -v cyclonedx-gomod >/dev/null 2>&1; then \
		echo "cyclonedx-gomod не найден в PATH."; \
		echo "установить: go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@latest"; \
		exit 1; \
	fi
	@mkdir -p $(SBOM_DIR)
	@for spec in $(SBOM_APPS); do \
		mod="$${spec%%:*}"; main="$${spec##*:}"; \
		out="$(SBOM_DIR)/$$mod.cdx.json"; \
		echo "cyclonedx-gomod app -main $$main ./$$mod -> $$out"; \
		cyclonedx-gomod app -licenses -json -main "$$main" -output "$$out" "./$$mod" || exit 1; \
	done
	@echo "sbom: CycloneDX SBOM записан в $(SBOM_DIR)/"

# Нативные пакеты deb + rpm через nfpm. Бинари пересобираются под Linux/$(PKG_ARCH)
# (deb/rpm всегда Linux, а dev-машина может быть darwin) с теми же ldflags, что в
# `build`. Конфиги nfpm — deploy/nfpm/*.yaml; ${VERSION}/${ARCH} подставляются из
# окружения. Инструмент в PATH не входит автоматически; если не найден — печатаем
# go install-подсказку и выходим с ошибкой (не silently).
pkg:
	@if ! command -v nfpm >/dev/null 2>&1; then \
		echo "nfpm не найден в PATH."; \
		echo "установить: go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest"; \
		exit 1; \
	fi
	@mkdir -p $(PKG_DIR)
	@echo "build linux/$(PKG_ARCH) бинарей под упаковку (VERSION=$(VERSION))"
	@cd keeper    && CGO_ENABLED=0 GOOS=linux GOARCH=$(PKG_ARCH) go build -trimpath -ldflags '-s -w $(KEEPER_LDFLAGS)' -o $(BIN_DIR)/keeper ./cmd/keeper
	@cd soul      && CGO_ENABLED=0 GOOS=linux GOARCH=$(PKG_ARCH) go build -trimpath -ldflags '-s -w $(SOUL_LDFLAGS)' -o $(BIN_DIR)/soul ./cmd/soul
	@cd soul-lint && CGO_ENABLED=0 GOOS=linux GOARCH=$(PKG_ARCH) go build -trimpath -ldflags '-s -w' -o $(BIN_DIR)/soul-lint   ./cmd/soul-lint
	@for cfg in keeper soul soul-lint; do \
		for fmt in deb rpm; do \
			echo "nfpm package $$cfg ($$fmt)"; \
			VERSION='$(VERSION)' ARCH='$(PKG_ARCH)' nfpm package \
				--config deploy/nfpm/$$cfg.yaml \
				--packager $$fmt \
				--target $(PKG_DIR)/ || exit 1; \
		done; \
	done
	@echo "pkg: deb+rpm записаны в $(PKG_DIR)/"

# Подпись образов (cosign) — DOCUMENTED STUB. Реальная подпись требует registry +
# OIDC/keyless-identity (или приватного ключа), которых в локальном репо без
# CI/публикации нет. См. docs/deploy README, раздел «Подпись образов».
sign:
	@echo "make sign: подпись образов отложена (post-publish)."
	@echo "Требует registry + cosign keyless-identity (OIDC) или приватный ключ."
	@echo "Подробности и план — раздел «Подпись образов (cosign)» в deploy/README.md."
	@exit 0

# Единый локальный CI-гейт. Порядок: дешёвые статические проверки → сборка →
# тесты (workspace + community-плагины) → drift-проверки → supply-chain-скан →
# lint корпуса examples/ → L0-испытания (soul-trial).
# `test-integration` сюда НЕ входит — он требует docker (см. комментарий к
# `test`); гонять отдельно. Release/packaging-таргеты (sbom/pkg/sign) сюда НЕ
# входят — внешний tooling. `check-vuln` требует доступ к vuln.go.dev — offline
# пропускается через SKIP_VULNCHECK=1 (см. таргет), в CI гонит реально.
# `test-plugins` — go.mod-плагины вне go.work (GOWORK=off). `trial` — L0-render
# по корпусу examples/service/ (ловит сломанные case.yml-ассерты).
check: check-fmt vet build test test-plugins check-gen check-openapi check-template check-webui check-doc-links check-vuln lint trial
	@echo "check: все проверки пройдены"

# gofmt-форматирование по всем модулям. `gofmt -l` печатает только файлы,
# отличающиеся от канонического формата; непустой список — это ошибка гейта.
# Скоупим по корням модулей (gofmt сам рекурсивно обходит каталоги), вывод
# одного `gofmt -l` агрегируем и фейлим, если хоть что-то нашлось.
check-fmt:
	@out=$$(gofmt -l $(MODULES) 2>/dev/null); \
	if [ -n "$$out" ]; then \
		echo "gofmt: следующие файлы не отформатированы:"; \
		echo "$$out"; \
		echo ""; \
		echo "run 'gofmt -w' on listed files"; \
		exit 1; \
	fi; \
	echo "gofmt: все файлы отформатированы"

# `go vet ./...` по каждому модулю. Тот же skip-empty-module паттерн, что в
# `test`/`build` (`go list ./...` пуст → модуль без go-пакетов, пропускаем),
# иначе `go vet ./...` падает с "matched no packages".
vet:
	@for m in $(MODULES); do \
		if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then \
			echo "skip $$m (no Go packages)"; \
			continue; \
		fi; \
		echo "go vet ./... in $$m"; \
		(cd $$m && go vet ./...) || exit 1; \
	done

# Проверка идемпотентности протогена (gen-drift): прогоняем `make gen` и
# смотрим, не изменился ли committed generated Go. Скоупим diff именно по двум
# каталогам сгенерённого кода — гейт не должен падать на несвязанных правках
# рабочего дерева. Непустой diff означает «забыли закоммитить `make gen`» либо
# «протоген не идемпотентен» (тогда вопрос к toolchain/версиям protoc-плагинов).
check-gen:
	@$(MAKE) gen
	@if ! git diff --exit-code -- $(KEEPER_PROTO_OUT) $(PLUGIN_PROTO_OUT); then \
		echo ""; \
		echo "gen-drift: сгенерённый Go отличается от committed"; \
		echo "закоммить результат 'make gen' (или протоген не идемпотентен)"; \
		exit 1; \
	fi
	@echo "check-gen: протоген идемпотентен"

# Проверка целостности внутренних doc-ссылок (markdown [..](file.md#anchor) во
# всех *.md, включая CLAUDE.md и examples/, + docs/...#anchor в Go-комментариях).
# Целевой файл должен существовать, якорь — генериться GitHub-slug-ом из заголовка.
# PRE-EXISTING битые ссылки (усечённые Go-якоря, устаревшие slug-и) занесены в
# scripts/doc-links-allowlist.txt и выгребаются батчами миграции ADR в docs/adr/.
check-doc-links:
	@python3 scripts/check-doc-links.py

# govulncheck — supply-chain CI-гейт по всем go.work-модулям (ИБ-аудит, до беты).
# Symbol-scan: падает (exit 3) ТОЛЬКО когда уязвимость реально достижима по графу
# вызовов кода/зависимостей — не просто «есть в go.sum». Тот же skip-empty-module
# паттерн, что у vet/test (модуль без go-пакетов пропускается).
#
# Бинарь — `go install` в $(GOPATH)/bin (паттерн protoc-плагинов). Если не
# найден — ставим pinned-версию (идемпотентно).
#
# Offline-graceful: govulncheck тянет vuln-DB (vuln.go.dev). Без сети прогон
# невозможен — `SKIP_VULNCHECK=1` пропускает гейт с warning (dev-машина без
# доступа не блокируется). В CI переменная НЕ ставится → гейт гонит реально и
# обязан быть зелёным. Не silently-skip-by-default: пропуск только по явному
# opt-out, иначе supply-chain-регресс прошёл бы незамеченным.
# Весь рецепт — ОДНА shell-инвокация (`if ...; then ...; fi` в одной логической
# строке): иначе `exit 0` в первой recipe-строке завершил бы только её sub-shell,
# а следующие строки таргета всё равно выполнились бы (make гонит каждую строку
# отдельным shell-ом). SKIP-ветка обязана пропустить весь скан целиком.
check-vuln:
	@if [ -n "$(SKIP_VULNCHECK)" ]; then \
		echo "check-vuln: SKIP_VULNCHECK задан — supply-chain-скан пропущен (offline opt-out)"; \
	else \
		if [ ! -x "$(GOVULNCHECK)" ]; then \
			echo "govulncheck не найден — go install @$(GOVULNCHECK_VERSION)"; \
			go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) || exit 1; \
		fi; \
		for m in $(MODULES); do \
			if [ -z "$$(cd $$m && go list ./... 2>/dev/null)" ]; then \
				echo "skip $$m (no Go packages)"; \
				continue; \
			fi; \
			echo "govulncheck ./... in $$m"; \
			(cd $$m && $(GOVULNCHECK) ./...) || exit 1; \
		done; \
		echo "check-vuln: govulncheck чист по всем модулям"; \
	fi

# Офлайн-валидация корпуса examples/ линтером soul-lint. Бинарь собирается в
# рамках `build` (зависимость). Категории: destiny / service / manifest /
# scenario. validate-scenario принимает путь к scenario/<name>/main.yml
# (точку входа сценария; вторичные файлы резолвятся через include: из main.yml).
# Пустая категория (нет файлов под glob) пропускается без ошибки. Любой
# не-нулевой выход soul-lint на committed-примере фейлит гейт.
lint: build
	@for f in examples/destiny/*/destiny.yml; do \
		[ -e "$$f" ] || continue; \
		echo "validate-destiny $$f"; \
		$(LINT_BIN) validate-destiny "$$f" || exit 1; \
	done
	@for f in examples/service/*/service.yml; do \
		[ -e "$$f" ] || continue; \
		echo "validate-service $$f"; \
		$(LINT_BIN) validate-service "$$f" || exit 1; \
	done
	@for f in examples/module/*/manifest.yaml; do \
		[ -e "$$f" ] || continue; \
		echo "validate-manifest $$f"; \
		$(LINT_BIN) validate-manifest "$$f" || exit 1; \
	done
	@for f in examples/service/*/scenario/*/main.yml; do \
		[ -e "$$f" ] || continue; \
		echo "validate-scenario $$f"; \
		$(LINT_BIN) validate-scenario "$$f" || exit 1; \
	done
	@echo "lint: корпус examples/ валиден"

# L0-испытания (soul-trial, ADR-023): render-only, герметичные. Гоняем рекурсивно
# по КАЖДОМУ корпус-каталогу examples/service/<svc> И examples/destiny/<svc> с хотя
# бы одним tests/<case>/case.yml (soul-trial сам ищет case.yml рекурсивно, в т.ч.
# под _trial/scenario/.../tests/). Бинарь soul-trial собирается в рамках `build`
# (зависимость). Раньше эти кейсы жили ВНЕ гейта (`make lint` = только soul-lint-
# схема, `make test` = go test) — поэтому сломанные ассерты (напр. off-by-5 индексы
# add_node после sentinel-слайса) оставались зелёными. Теперь гейт реально исполняет
# L0. До 2026-06-26 обходились ТОЛЬКО examples/service/ — destiny-корпус (node-exporter
# и др. с _trial-кейсами) выпадал из гейта; теперь покрыт.
#
# L2 (stand-based, требуют поднятый стенд) harness пропускает сам — в гейт не
# тащим; ценность гейта — L0 render-инварианты.
#
# Skip-list ($(TRIAL_SKIP)) печатается ГРОМКО per-каталог: исключение видно в логе,
# не маскирует регресс. Каталог без case.yml тихо пропускается (нечего гонять).
trial: build
	@for root in examples/service examples/destiny; do \
		for svc in "$$root"/*/; do \
			[ -d "$$svc" ] || continue; \
			name=$$(basename "$$svc"); \
			skip=""; \
			for s in $(TRIAL_SKIP); do [ "$$s" = "$$name" ] && skip=1; done; \
			if [ -n "$$skip" ]; then \
				echo "SKIP trial $$name (в TRIAL_SKIP — pre-existing L0-drift, см. Makefile-комментарий)"; \
				continue; \
			fi; \
			if ! find "$$svc" -name case.yml | grep -q .; then \
				continue; \
			fi; \
			echo "soul-trial run $$svc"; \
			$(TRIAL_BIN) run "$$svc" || exit 1; \
		done; \
	done
	@echo "trial: L0-испытания корпуса examples/service/ + examples/destiny/ пройдены"

# --- Нагрузочное тестирование (soul-legion) ---
# Однокнопочный прогон нагрузочного генератора soul-legion (tests/load/, ADR-004:
# test-only, НЕ поставочный бинарь; вне MODULES — `make check` его не трогает).
# Полный план/методика/измеренные числа — docs/testing/load-testing.md.
#
# Предусловие: поднятый dev-стенд (keeper event-stream :9443 / metrics :9090 /
# openapi :8080 + dev-PKI). healthz-guard ниже проверяет это до сборки/минта и
# при недоступности подсказывает `make dev-stand` (или `make dev-keeper`).
#
# Профиль нагрузки задаётся ENV-переменными (ниже дефолты). Примеры:
#   make stress                          # 1000 коннектов (ось A), cleanup
#   make stress COUNT=500 API=1 VOYAGE=1 # + ось B (API) + ось C (один Voyage)
#   make stress WRITE=1                  # + ось write: create→delete циклы (write+audit-путь)
#   make stress COUNT=2000 RAMP=500 DURATION=60s
#   make stress COUNT=10000 VOYAGE=1 VOYAGE_CONCURRENCY=100 VOYAGE_POLL=600s
#                                        # disambiguating Voyage-cliff: явный concurrency + длинный poll
#   make stress COUNT=25000 ISSUE_CONCURRENCY=128
#                                        # большой N: поднять параллелизм cert-минтинга в setup-фазе
#
# Ось A (стримы) гонится всегда. Оси B/C/write опциональны (API=1 / VOYAGE=1 /
# WRITE=1) и требуют admin-JWT — он минтится тем же механизмом, что `make dev-jwt`
# (dev/mint-jwt.sh, ключ из Vault), и прокидывается в --jwt. Без них токен не
# нужен (не минтим).
COUNT         ?= 1000
RAMP          ?= 250
RAMP_INTERVAL ?= 300ms
DURATION      ?= 30s
COVEN         ?= legion
API           ?= 0
VOYAGE        ?= 0
# Ось write (write+audit-путь): create→delete циклы безопасных самоочищающихся
# сущностей (synod/role/push-provider/herald). Требует admin-JWT (как оси B/C).
WRITE          ?= 0
WRITE_DURATION ?= 15s
API_DURATION  ?= 15s
# Ось C tuning (disambiguating-эксперимент Voyage-cliff): VOYAGE_CONCURRENCY пусто/0
# → поле concurrency НЕ слать (keeper-дефолт=1, последовательно); >0 → top-level
# voyage.concurrency в теле create. VOYAGE_POLL — бюджет ожидания терминала.
VOYAGE_CONCURRENCY ?=
VOYAGE_POLL        ?= 120s
# Параллелизм Vault-issue в setup-фазе (cert-минтинг). На больших N (25k/50k)
# поднимать до ~96-128, чтобы setup-фаза не тянулась. Совпадает с дефолтом флага
# --issue-concurrency (32); ENV только переопределяет, дефолт флага не трогаем.
ISSUE_CONCURRENCY  ?= 32

# Эндпоинты dev-стенда (сверены с dev/keeper.dev.yml: event_stream :9443 /
# openapi :8080 / metrics :9090) и dev-PKI/инфра (provision.sh / docker-compose).
KEEPER_ENDPOINT ?= 127.0.0.1:9443
OPENAPI         ?= http://127.0.0.1:8080
METRICS         ?= http://127.0.0.1:9090
PG              ?= postgres://keeper:keeper@localhost:5434/keeper?sslmode=disable
VAULT           ?= http://127.0.0.1:8200
# root CA Keeper-server-cert-а — тот же путь, что listen.event_stream.tls.ca в
# dev/keeper.dev.yml (Vault PKI root, CN=soul-stack).
STRESS_CA       ?= /tmp/keeper-dev/tls/vault-ca.crt
# Здоровье API-listener-а keeper-а (тот же /healthz, что ждёт dev/keeper-run.sh).
STRESS_HEALTHZ  ?= http://127.0.0.1:8080/healthz

# stress — собрать soul-legion → (при API/VOYAGE) минтить admin-JWT → прогнать →
# почистить (--cleanup). load-test — алиас.
stress:
	@code="$$(curl -s -o /dev/null -w '%{http_code}' '$(STRESS_HEALTHZ)' 2>/dev/null || true)"; \
	if [ "$$code" != "200" ]; then \
		echo "stress: dev-стенд недоступен ($(STRESS_HEALTHZ) → $$code, ожидался 200)."; \
		echo "  подними стенд: 'make dev-stand' (полный) или 'make dev-keeper' (только keeper)."; \
		exit 1; \
	fi
	@if [ ! -s "$(STRESS_CA)" ]; then \
		echo "stress: нет dev-CA ($(STRESS_CA)) — запусти 'make dev-provision' и повтори."; \
		exit 1; \
	fi
	@echo "go build -o tests/load/bin/soul-legion ./cmd/soul-legion in tests/load"
	@cd tests/load && go build -o bin/soul-legion ./cmd/soul-legion
	@JWT=""; \
	if [ "$(API)" = "1" ] || [ "$(VOYAGE)" = "1" ] || [ "$(WRITE)" = "1" ]; then \
		echo "stress: минчу admin-JWT (механизм make dev-jwt) для осей B/C/write"; \
		JWT="$$(AID='$(AID)' ROLES='$(ROLES)' TTL='$(TTL)' bash dev/mint-jwt.sh)" \
			|| { echo "stress: не удалось выпустить JWT (Vault поднят? 'make dev-up' + 'make dev-provision')"; exit 1; }; \
	fi; \
	echo "stress: гоню soul-legion (count=$(COUNT) ramp=$(RAMP)/$(RAMP_INTERVAL) duration=$(DURATION) coven=$(COVEN) api=$(API) voyage=$(VOYAGE) write=$(WRITE))"; \
	./tests/load/bin/soul-legion \
		--keeper-endpoint='$(KEEPER_ENDPOINT)' \
		--metrics='$(METRICS)' \
		--openapi='$(OPENAPI)' \
		--pg='$(PG)' \
		--vault='$(VAULT)' \
		--ca='$(STRESS_CA)' \
		--coven='$(COVEN)' \
		--count=$(COUNT) \
		--issue-concurrency=$(ISSUE_CONCURRENCY) \
		--ramp=$(RAMP) \
		--ramp-interval='$(RAMP_INTERVAL)' \
		--duration='$(DURATION)' \
		--api=$(if $(filter 1,$(API)),true,false) \
		--api-duration='$(API_DURATION)' \
		--voyage=$(if $(filter 1,$(VOYAGE)),true,false) \
		--voyage-concurrency=$(if $(VOYAGE_CONCURRENCY),$(VOYAGE_CONCURRENCY),0) \
		--voyage-poll-timeout='$(VOYAGE_POLL)' \
		--write=$(if $(filter 1,$(WRITE)),true,false) \
		--write-duration='$(WRITE_DURATION)' \
		--jwt="$$JWT" \
		--cleanup=true

load-test: stress

# Шпаргалка по таргетам. Подробности dev-стека — `docs/dev/local-setup.md`.
help:
	@echo "Сборка и тесты:"
	@echo "  gen               protoc keeper+plugin + gen-openapi → committed gen"
	@echo "  gen-openapi       huma-dump → committed docs/keeper/openapi.yaml (производный, для UI)"
	@echo "  build             собрать keeper / soul-trial / soul / soul-lint / soulctl"
	@echo "  build-soulctl     собрать только soulctl (клиентский CLI оператора)"
	@echo "  test              go test ./... по всем модулям (без docker)"
	@echo "  test-plugins      GOWORK=off go test по go.mod-плагинам examples/module/* (community.redis)"
	@echo "  test-race         go test -race ./..."
	@echo "  test-integration  go test -tags=integration (testcontainers, нужен docker)"
	@echo "  e2e               L3a E2E pilot (tests/e2e, -tags=e2e, нужен docker для имп-slice)"
	@echo "  build-linux       cross-compile keeper+soul под Linux amd64 (для L3b real-soul-container)"
	@echo "  e2e-live          L3b smoke-loop (tests/e2e-live, -tags=e2e_live, privileged docker, nightly)"
	@echo "  e2e-k8s           L3c k8s-loop (tests/e2e-k8s, -tags=e2e_k8s, kind + bitnami Helm, weekly)"
	@echo "  docker-build-keeper  собрать образ keeper:e2e-k8s (для L3c kind load docker-image)"
	@echo "  docker-build-soul    собрать образ soul:e2e-k8s (privileged systemd Debian-12 для L3c-3+)"
	@echo "  tidy              go mod tidy по всем модулям"
	@echo ""
	@echo "Проверки/гейт:"
	@echo "  check             единый локальный CI-гейт (fmt+vet+build+test+test-plugins+openapi+gen+lint+trial)"
	@echo "  check-fmt         gofmt -l по всем модулям (fail на неотформатированных)"
	@echo "  vet               go vet ./... по всем модулям"
	@echo "  check-gen         идемпотентность протогена (gen-drift в proto/gen/go)"
	@echo "  check-doc-links   целостность внутренних doc-ссылок (markdown + Go-комментарии)"
	@echo "  check-vuln        govulncheck supply-chain по всем модулям (offline: SKIP_VULNCHECK=1)"
	@echo "  lint              soul-lint по корпусу examples/ (destiny/service/manifest/scenario)"
	@echo "  trial             soul-trial L0-испытания по корпусу examples/service/ (render-инварианты)"
	@echo ""
	@echo "Локальный dev-стек:"
	@echo "  dev-up            docker compose up -d (PG / Vault / Redis)"
	@echo "  dev-stop          остановить локальные keeper/soul-демоны dev-воркфлоу"
	@echo "  dev-down          dev-stop + docker compose down (данные persist)"
	@echo "  dev-reset         docker compose down -v && up -d (полный сброс БД)"
	@echo "  dev-provision     idempotent bootstrap: Vault KV/PKI + TLS + git-репо артефактов"
	@echo "  dev-smoke         dev-up → dev-provision → keeper init → dev-provision (seed реестра)"
	@echo "  dev-keeper        рестарт keeper с полным dev-env (file://-резолв) + ждёт healthz"
	@echo "  dev-jwt           выпустить Archon-JWT из Vault-ключа (AID/ROLES/TTL); токен в stdout"
	@echo "  dev-souls         переподнять локальный флот souls по реестру БД"
	@echo "  dev-web           vite dev-сервер web-репо (--host; WEB_DIR=<путь>)"
	@echo "  dev-stand         полный подъём стенда: provision → keeper → souls → web"
	@echo ""
	@echo "Нагрузочное тестирование (soul-legion, нужен поднятый стенд):"
	@echo "  stress            one-button нагрузка: build+mint-JWT+gon+cleanup (ENV: COUNT/RAMP/API/VOYAGE/WRITE/WRITE_DURATION/VOYAGE_CONCURRENCY/VOYAGE_POLL/ISSUE_CONCURRENCY/...)"
	@echo "  load-test         алиас stress"
	@echo ""
	@echo "OpenAPI:"
	@echo "  gen-openapi       перегенерировать committed openapi.yaml из huma-агрегатора"
	@echo "  check-openapi     CI-guard на drift committed openapi.yaml vs huma-dump"
	@echo "  check-template    CI-guard на drift embedded plugin-template (skip без companion)"
	@echo "  sync-webui        вендоринг dist/ companion soul-stack-web → keeper/internal/webui/assets/"
	@echo "  check-webui       CI-guard на drift embedded UI (skip без companion)"
	@echo ""
	@echo "Release/packaging (аддитивно, НЕ входят в check):"
	@echo "  docker-keeper     ПРОД-образ keeper (multi-stage distroless) → \$$(KEEPER_IMAGE):\$$(VERSION); push в свой registry"
	@echo "  sbom              CycloneDX SBOM по go-модулям (cyclonedx-gomod) → dist/sbom/"
	@echo "  pkg               нативные пакеты deb+rpm (nfpm) → dist/pkg/"
	@echo "  sign              подпись образов (cosign) — отложено, documented-stub"
