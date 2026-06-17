// Package seed — версионная раскладка SoulSeed-материала на диске Soul-хоста.
//
// SoulSeed состоит из трёх обязательных PEM-файлов:
//
//	cert.pem  — клиентский сертификат, выпущенный Vault PKI через Keeper Bootstrap.
//	key.pem   — приватный ключ к cert.pem (mode 0o400, owner = soul-service-user).
//	ca.pem    — CA-цепочка для верификации серверного сертификата Keeper-а.
//
// и одного опционального trust-anchor-файла (ADR-026, slice S2b):
//
//	sigil_pubkey.pem — PEM (SPKI) публичного ed25519-ключа подписи Sigil,
//	    полученного в BootstrapReply. Им Soul верифицирует подпись допусков
//	    плагинов (PluginSigil) в pull-режиме без нового bootstrap (slice S6).
//	    ОПЦИОНАЛЕН: пустой pubkey (Sigil на Keeper-е не настроен) — валидное
//	    состояние, файл не пишется и его отсутствие не делает версию неполной.
//
// Раскладка в `paths.seed` (директория 0o700):
//
//	paths.seed/
//	  current -> vN        # относительный симлинк на активную версию
//	  v1/  cert.pem key.pem ca.pem [sigil_pubkey.pem]
//	  v2/  cert.pem key.pem ca.pem [sigil_pubkey.pem]
//	  ...
//
// Запись новой версии и переключение активной — атомарны (см. [Write]):
// версия целиком пишется в `vN+1/`, после чего симлинк `current` атомарно
// переставляется на неё. До переключения `current` указывает на прежнюю
// версию, поэтому сбой на любом шаге до swap-а оставляет валидную старую
// активную версию (crash-safety). sigil_pubkey.pem пишется в той же версии
// до swap-а, поэтому trust-anchor переключается атомарно вместе с cert/key/ca
// и переживает рестарт. Чтение прозрачно идёт через `current/` (см. [Load]).
// Имена файлов фиксированные, не настраиваются.
//
// Хард-кат M1: старый плоский формат (cert/key/ca прямо в `paths.seed`) НЕ
// поддерживается. Отсутствие `current` → [ErrIncomplete] (оператор делает
// `soul init` заново); авто-миграции нет.
package seed

import (
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Имена файлов внутри версии. Меняются только синхронно с docs/soul/identity.md.
const (
	CertFile = "cert.pem"
	KeyFile  = "key.pem"
	CAFile   = "ca.pem"
	// SigilPubKeyFile — опциональный trust-anchor Sigil (ADR-026, S2b).
	// Отсутствие файла = Sigil выключен, это валидное состояние.
	SigilPubKeyFile = "sigil_pubkey.pem"

	// currentLink — относительный симлинк на активную версию внутри paths.seed.
	currentLink = "current"
	// versionPrefix — префикс каталогов версий (`v1`, `v2`, …).
	versionPrefix = "v"
)

// Material — содержимое SoulSeed в памяти.
type Material struct {
	// CertPEM — клиентский cert, выпущенный Keeper-ом.
	CertPEM []byte
	// KeyPEM — приватный ключ к CertPEM. Никогда не покидает хост,
	// при логировании не выводить.
	KeyPEM []byte
	// CAPEM — CA-цепочка Keeper-а (для верификации серверного cert-а).
	CAPEM []byte
	// SigilPubKeyPEM — опциональный trust-anchor Sigil (ADR-026, S2b): PEM
	// (SPKI) публичного ed25519-ключа подписи допусков плагинов. nil/пусто =
	// Sigil не настроен на Keeper-е; тогда файл sigil_pubkey.pem не пишется,
	// и его отсутствие не делает версию неполной (verify плагинов выключен).
	SigilPubKeyPEM []byte
}

// Paths — пути к файлам seed-а активной версии (внутри `dir/current/`).
type Paths struct {
	Cert string
	Key  string
	CA   string
	// SigilPubKey — путь к опциональному trust-anchor-у Sigil. Файл по этому
	// пути может отсутствовать (Sigil выключен) — caller обязан проверять
	// существование, а не предполагать наличие.
	SigilPubKey string
}

// PathsIn возвращает пути к файлам активной версии — внутри `dir/current/`.
// Симлинк `current` прозрачен для open(2), tls-конфиг читает материал через
// него, поэтому swap версии меняет источник без переинициализации путей.
func PathsIn(dir string) Paths {
	cur := filepath.Join(dir, currentLink)
	return Paths{
		Cert:        filepath.Join(cur, CertFile),
		Key:         filepath.Join(cur, KeyFile),
		CA:          filepath.Join(cur, CAFile),
		SigilPubKey: filepath.Join(cur, SigilPubKeyFile),
	}
}

// ErrIncomplete — Load на директории без активной версии (нет `current` или
// в активной версии не хватает одного из трёх файлов). Runtime-условие
// «ещё не делали soul init», а не «I/O сломан».
var ErrIncomplete = errors.New("seed: bootstrap not completed (no active version under paths.seed/current)")

// ErrMismatched — на диске лежит несогласованная пара cert↔key (например,
// частичная/повреждённая ротация мимо нашего атомарного swap-а). Отличается
// от ErrIncomplete: «материал есть, но cert и key не образуют валидную пару»,
// а не «материала нет».
var ErrMismatched = errors.New("seed: cert.pem and key.pem do not form a valid pair")

// Load читает активную версию через `dir/current/{cert,key,ca}.pem` плюс
// опциональный `sigil_pubkey.pem`.
//
// Возвращает [ErrIncomplete] обёрнутым, если `current` отсутствует или в
// активной версии нет одного из трёх ОБЯЗАТЕЛЬНЫХ файлов (cert/key/ca) — caller
// печатает подсказку «run soul init». Отсутствие sigil_pubkey.pem — НЕ ошибка
// (Sigil выключен): поле [Material.SigilPubKeyPEM] остаётся nil. После чтения
// cert+key проверяются на согласованность через [tls.X509KeyPair]; рассинхрон →
// [ErrMismatched] (без утечки key в текст).
func Load(dir string) (*Material, error) {
	if dir == "" {
		return nil, errors.New("seed: paths.seed is empty")
	}
	if _, err := os.Lstat(filepath.Join(dir, currentLink)); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrIncomplete, filepath.Join(dir, currentLink))
		}
		return nil, fmt.Errorf("seed: stat %s: %w", filepath.Join(dir, currentLink), err)
	}
	p := PathsIn(dir)
	certPEM, err := readMember(p.Cert)
	if err != nil {
		return nil, err
	}
	keyPEM, err := readMember(p.Key)
	if err != nil {
		return nil, err
	}
	caPEM, err := readMember(p.CA)
	if err != nil {
		return nil, err
	}
	// Опциональный trust-anchor Sigil: NotExist → nil (Sigil выключен), не
	// ErrIncomplete. Прочая I/O-ошибка — фейл (файл есть, но не читается).
	sigilPub, err := readOptionalMember(p.SigilPubKey)
	if err != nil {
		return nil, err
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		// Текст ошибки tls.X509KeyPair не содержит сам ключ — оборачиваем в
		// статичный ErrMismatched, без %w на err, чтобы наверняка не протащить
		// детали в логи.
		return nil, ErrMismatched
	}
	return &Material{CertPEM: certPEM, KeyPEM: keyPEM, CAPEM: caPEM, SigilPubKeyPEM: sigilPub}, nil
}

// readMember читает один обязательный файл версии. NotExist → ErrIncomplete
// (версия неполная), прочая I/O-ошибка оборачивается с именем файла.
func readMember(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrIncomplete, path)
		}
		return nil, fmt.Errorf("seed: read %s: %w", path, err)
	}
	return data, nil
}

// readOptionalMember читает опциональный файл версии. NotExist → (nil, nil):
// отсутствие — валидное состояние (для sigil_pubkey.pem = Sigil выключен), а не
// признак неполной версии. Прочая I/O-ошибка оборачивается с именем файла.
func readOptionalMember(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("seed: read %s: %w", path, err)
	}
	return data, nil
}

// Write раскладывает Material в новую версию `dir/vN+1/` и атомарно
// переставляет на неё симлинк `dir/current`.
//
// Шаги:
//  1. валидация пары cert↔key через [tls.X509KeyPair] — fail-fast ДО любой
//     записи; несогласованную пару на диск не пишем (текст ошибки не содержит
//     key-материал);
//  2. вычисление следующей версии vN+1 (нет версий → v1);
//  3. запись трёх обязательных файлов в `dir/vN+1/` (mode cert/ca = 0o644,
//     key = 0o400) плюс опционального sigil_pubkey.pem (0o644), если
//     SigilPubKeyPEM не пуст; fsync каждого файла и fsync самого каталога
//     версии (crash-safety);
//  4. атомарный swap: temp-симлинк → os.Rename поверх `current`, fsync `dir`;
//  5. best-effort очистка версий старше предыдущей (хранится current + 1).
//
// До шага 4 `current` указывает на прежнюю версию — сбой на 1–3 оставляет
// валидную старую активную версию. sigil_pubkey.pem попадает в ту же версию
// vN+1, поэтому trust-anchor переключается атомарно вместе с cert/key/ca и
// переживает рестарт (pull-режим verify в S6 без нового bootstrap).
func Write(dir string, m *Material) error {
	if dir == "" {
		return errors.New("seed: paths.seed is empty")
	}
	if m == nil {
		return errors.New("seed: material is nil")
	}
	// (a) Валидация пары до любой записи. Ошибку не оборачиваем содержимым key.
	if _, err := tls.X509KeyPair(m.CertPEM, m.KeyPEM); err != nil {
		return ErrMismatched
	}
	// (b) Каталог seed-а + вычисление следующей версии.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("seed: mkdir %s: %w", dir, err)
	}
	next, err := nextVersion(dir)
	if err != nil {
		return err
	}
	verName := versionPrefix + strconv.Itoa(next)
	verDir := filepath.Join(dir, verName)
	// (c) Запись всей версии в vN+1.
	if err := os.MkdirAll(verDir, 0o700); err != nil {
		return fmt.Errorf("seed: mkdir %s: %w", verDir, err)
	}
	if err := atomicWrite(filepath.Join(verDir, CertFile), m.CertPEM, 0o644); err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(verDir, KeyFile), m.KeyPEM, 0o400); err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(verDir, CAFile), m.CAPEM, 0o644); err != nil {
		return err
	}
	// Опциональный trust-anchor Sigil — только при наличии (Sigil настроен).
	// Пустой → файл не создаём; Load трактует отсутствие как «Sigil выключен».
	if len(m.SigilPubKeyPEM) > 0 {
		if err := atomicWrite(filepath.Join(verDir, SigilPubKeyFile), m.SigilPubKeyPEM, 0o644); err != nil {
			return err
		}
	}
	// (d) fsync каталога версии — без него rename-ы файлов внутри vN+1 могут не
	// дойти до диска до crash-а, и версия окажется неполной (R1).
	if err := fsyncDir(verDir); err != nil {
		return err
	}
	// (e) Атомарный swap симлинка current -> verName (относительный).
	if err := swapCurrent(dir, verName); err != nil {
		return err
	}
	// (f) Best-effort очистка: ошибка не фейлит Write — версия уже активна.
	pruneOldVersions(dir, next)
	return nil
}

// nextVersion возвращает max(существующие vN) + 1; нет версий → 1.
func nextVersion(dir string) (int, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("seed: read dir %s: %w", dir, err)
	}
	max := 0
	for _, e := range ents {
		if n, ok := parseVersion(e.Name()); ok && n > max {
			max = n
		}
	}
	return max + 1, nil
}

// parseVersion разбирает имя каталога версии `vN` в число N (N ≥ 1).
func parseVersion(name string) (int, bool) {
	if !strings.HasPrefix(name, versionPrefix) {
		return 0, false
	}
	n, err := strconv.Atoi(name[len(versionPrefix):])
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// swapCurrent атомарно переставляет симлинк `dir/current` на target
// (относительное имя версии внутри dir). Создаёт temp-симлинк рядом и
// os.Rename-ит его поверх current (атомарно на POSIX), затем fsync-ит dir.
func swapCurrent(dir, target string) error {
	tmp, err := os.CreateTemp(dir, "."+currentLink+".tmp-*")
	if err != nil {
		return fmt.Errorf("seed: create temp symlink in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// CreateTemp создал обычный файл — нам нужен симлинк на его месте.
	_ = tmp.Close()
	if err := os.Remove(tmpName); err != nil {
		return fmt.Errorf("seed: prepare temp symlink %s: %w", tmpName, err)
	}
	if err := os.Symlink(target, tmpName); err != nil {
		return fmt.Errorf("seed: create temp symlink %s -> %s: %w", tmpName, target, err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, currentLink)); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("seed: swap %s -> %s: %w", currentLink, target, err)
	}
	// fsync каталога — фиксируем сам факт переименования симлинка (R1).
	if err := fsyncDir(dir); err != nil {
		return err
	}
	return nil
}

// pruneOldVersions удаляет версии vK с K < current-1: храним активную и одну
// предыдущую. Best-effort — ошибки игнорируются (вызывается после успешного
// swap-а, активная версия уже на месте).
func pruneOldVersions(dir string, current int) {
	keepFrom := current - 1
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		n, ok := parseVersion(e.Name())
		if !ok || n >= keepFrom {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// fsyncDir открывает каталог и fsync-ит его, фиксируя метаданные (созданные/
// переименованные внутри записи) на диск. Критично для crash-safety
// version-dir + swap (R1).
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("seed: open dir %s for fsync: %w", dir, err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return fmt.Errorf("seed: fsync dir %s: %w", dir, err)
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("seed: close dir %s: %w", dir, err)
	}
	return nil
}

// atomicWrite пишет data в temp-файл рядом с path и переименовывает.
// На POSIX rename атомарен внутри одной FS; temp-файл за пределы каталога не
// уезжает (path фиксирован вызывающим).
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("seed: create temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	// На любой ошибке ниже временный файл должен исчезнуть.
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("seed: write %s: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("seed: chmod %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("seed: fsync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("seed: close %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("seed: rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}
