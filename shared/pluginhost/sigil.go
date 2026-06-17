package pluginhost

import (
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// Канонизация и сборка подписываемого блока Sigil (ADR-026, slice S3).
//
// Этот файл — общий helper, который импортируют ОБЕ стороны печати доверия:
//   - keeper/internal/sigil (S3) — при подписи бинаря плагина;
//   - soul/internal/pluginhost (S6, будущее) — при верификации перед seal/exec.
//
// Размещён в shared/pluginhost, потому что этот пакет уже импортируется и
// keeper, и soul (см. who-imports), и сам зависит только от shared/plugin +
// grpc — import-цикла нет.
//
// S3↔S6-инвариант (нормативный): байты manifest.yaml, которые Keeper хеширует
// при Sign, ОБЯЗАНЫ совпадать с байтами, которые Soul re-хеширует при verify.
// Гарантия держится на двух вещах: (1) manifest и бинарь доставляются одним
// artifact-потоком (один и тот же файл едет на host), (2) обе стороны прогоняют
// сырые байты через [NormalizeManifestBytes] перед хешированием — это снимает
// расхождения BOM / CRLF / trailing-newline между записью на разных ОС. Никакой
// re-parse/re-emit YAML: канонизация ТОЛЬКО байтовая, чтобы хеш не зависел от
// версии/настроек yaml-эмиттера.

// sigilDomainSeparator — domain-separation-тег подписываемого блока Sigil.
//
// Версия `/v1` обязательна: при смене формата блока (например, добавлении поля)
// тег станет `soul-stack/sigil/v2`, и старые подписи перестанут проходить против
// нового кода — это явный, а не молчаливый, разрыв совместимости. Тег включён в
// блок первым, чтобы подпись над Sigil нельзя было переиспользовать в другом
// протоколе (cross-protocol signature reuse).
const sigilDomainSeparator = "soul-stack/sigil/v1"

// NormalizeManifestBytes приводит сырые байты manifest.yaml к каноничной форме
// перед хешированием. ЕДИНСТВЕННАЯ канонизация manifest-а — байтовая:
//
//   - strip UTF-8 BOM (редакторы Windows иногда дописывают);
//   - CRLF → LF (Windows-переводы строк);
//   - ровно один trailing LF (несколько схлопываются в один; если нет — добавляется).
//
// НЕ re-parse и НЕ re-emit YAML: хеш не должен зависеть от версии/настроек
// yaml-эмиттера. Обе стороны (Sign на Keeper, verify на Soul) обязаны вызывать
// именно эту функцию — иначе S3↔S6-инвариант байтов не выполняется.
func NormalizeManifestBytes(raw []byte) []byte {
	b := sharedplugin.StripBOM(raw)

	// CRLF → LF. Делаем за один проход с copy-on-shrink: результат не длиннее
	// входа, выделяем буфер ровно под исходный размер и усекаем.
	out := make([]byte, 0, len(b)+1)
	for i := 0; i < len(b); i++ {
		if b[i] == '\r' {
			// \r\n → \n: пропускаем \r, \n добавится на следующей итерации.
			// Одинокий \r (старый Mac-CR) → \n.
			if i+1 < len(b) && b[i+1] == '\n' {
				continue
			}
			out = append(out, '\n')
			continue
		}
		out = append(out, b[i])
	}

	// Нормализуем trailing newline: схлопываем хвост из \n в ровно один \n.
	// Пустой вход → один \n (каноничный непустой результат, чтобы пустой и
	// «только пробелы»-manifest не давали идентичный хеш с реальным).
	end := len(out)
	for end > 0 && out[end-1] == '\n' {
		end--
	}
	out = out[:end]
	out = append(out, '\n')
	return out
}

// BuildSigilBlock собирает детерминированный подписываемый блок Sigil из полей
// allow-list-записи (ADR-026(b)/(c)). Чистая функция: один вход → один выход,
// без proto-marshal (message SigilSignedBlock сознательно НЕ вводится — это
// вернуло бы недетерминизм proto-сериализации, R-det).
//
// Форма блока (порядок полей фиксирован, менять нельзя без bump-а DST до v2):
//
//	DST || LP(namespace) || LP(name) || LP(ref) || LP(binarySHA256Raw) || LP(manifestSHA256Raw)
//
// где:
//   - DST = ASCII-константа [sigilDomainSeparator] ("soul-stack/sigil/v1"),
//     добавляется БЕЗ length-prefix (фиксированный известный префикс);
//   - LP(x) = 4 байта big-endian uint32 длины x, затем сами байты x. LP
//     применяется к КАЖДОМУ переменному полю — это защита границ полей: без
//     length-prefix конкатенация ("ab","c") и ("a","bc") дала бы один и тот же
//     блок, и подпись над одним набором полей подошла бы к другому;
//   - хеши кладутся СЫРЫМИ байтами (для SHA-256 — 32 байта), НЕ hex-строкой.
//
// Порядок полей ровно: namespace, name, ref, binary_sha256, manifest_sha256.
func BuildSigilBlock(namespace, name, ref string, binarySHA256Raw, manifestSHA256Raw []byte) []byte {
	const lpSize = 4
	dst := []byte(sigilDomainSeparator)

	total := len(dst) +
		lpSize + len(namespace) +
		lpSize + len(name) +
		lpSize + len(ref) +
		lpSize + len(binarySHA256Raw) +
		lpSize + len(manifestSHA256Raw)

	block := make([]byte, 0, total)
	block = append(block, dst...)
	block = appendLP(block, []byte(namespace))
	block = appendLP(block, []byte(name))
	block = appendLP(block, []byte(ref))
	block = appendLP(block, binarySHA256Raw)
	block = appendLP(block, manifestSHA256Raw)
	return block
}

// appendLP дописывает length-prefixed поле: uint32 big-endian длины + байты.
func appendLP(dst, field []byte) []byte {
	n := uint32(len(field))
	dst = append(dst, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	return append(dst, field...)
}
