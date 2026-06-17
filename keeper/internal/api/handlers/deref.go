package handlers

// derefStrings разыменовывает optional []string из request-тела (oapi/native даёт
// *[]string для omitempty-поля); nil → nil-срез («без значений»). Пакетный helper
// request-декодинга. Извлечён из operator.go при handler-native-развороте operator
// (T5d): остаётся общим для остальных доменов (oracle и др.).
func derefStrings(in *[]string) []string {
	if in == nil {
		return nil
	}
	return *in
}

// derefString разыменовывает optional string из request-тела; nil → "".
func derefString(in *string) string {
	if in == nil {
		return ""
	}
	return *in
}

// slicePtrIfNotEmpty возвращает nil для пустого/nil-среза (паритет json omitempty
// над массивом), иначе указатель на срез. Пакетный helper reply-проекции; извлечён
// из mypermissions.go при handler-native-развороте catalog (T5d): остаётся общим
// для остальных доменов (oracle и др.).
func slicePtrIfNotEmpty(s []string) *[]string {
	if len(s) == 0 {
		return nil
	}
	return &s
}
