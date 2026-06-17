package jwt

import (
	"errors"
	"fmt"
	"testing"
)

// TestClassifyVerifyErr — покрытие каждой ветки switch в ClassifyVerifyErr.
// Источник правды — verifier.go: классификатор различает только expired и
// invalid-issuer; всё остальное (malformed, bad-signature, not-yet-valid,
// произвольная ошибка) by design сводится к одному generic-detail
// «invalid token» — это защита от oracle-attacks через различение причин 401.
//
// Ошибки-sentinel проверяются и напрямую, и в обёрнутом виде
// (fmt.Errorf("%w: …")), потому что Verify возвращает их именно обёрнутыми.
func TestClassifyVerifyErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil — пустая строка",
			err:  nil,
			want: "",
		},
		{
			name: "истёкший токен",
			err:  ErrExpiredToken,
			want: publicDetailExpiredToken,
		},
		{
			name: "истёкший токен в обёртке",
			err:  fmt.Errorf("%w: extra context", ErrExpiredToken),
			want: publicDetailExpiredToken,
		},
		{
			name: "неверный issuer",
			err:  ErrInvalidIssuer,
			want: publicDetailInvalidIssuer,
		},
		{
			name: "неверный issuer в обёртке (как возвращает Verify)",
			err:  fmt.Errorf("%w: got %q, want %q", ErrInvalidIssuer, "evil", "keeper.test"),
			want: publicDetailInvalidIssuer,
		},
		{
			name: "ErrInvalidToken → generic-detail",
			err:  ErrInvalidToken,
			want: publicDetailInvalidToken,
		},
		{
			name: "ErrInvalidToken в обёртке (malformed/bad-sig путь Verify)",
			err:  fmt.Errorf("%w: token is malformed", ErrInvalidToken),
			want: publicDetailInvalidToken,
		},
		{
			name: "произвольная необёрнутая ошибка → дефолтная ветка",
			err:  errors.New("что-то совсем другое"),
			want: publicDetailInvalidToken,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyVerifyErr(tc.err)
			if got != tc.want {
				t.Errorf("ClassifyVerifyErr(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyVerifyErr_NeverLeaksRawError — гарантия из docstring
// ClassifyVerifyErr: detail НИКОГДА не содержит raw err.Error()
// (внутреннее сообщение golang-jwt — oracle-поверхность). Проверяем на
// ошибке с распознаваемым маркером в обёртке.
func TestClassifyVerifyErr_NeverLeaksRawError(t *testing.T) {
	marker := "INTERNAL-PARSER-PATH-MUST-NOT-LEAK"
	err := fmt.Errorf("%w: %s", ErrInvalidToken, marker)
	got := ClassifyVerifyErr(err)
	if got == "" {
		t.Fatalf("ClassifyVerifyErr на не-nil ошибке вернул пустую строку")
	}
	if got != publicDetailInvalidToken {
		t.Fatalf("detail = %q, want %q", got, publicDetailInvalidToken)
	}
	// Дополнительная проверка инварианта: маркер не должен всплыть в detail.
	if got == err.Error() {
		t.Errorf("detail совпал с raw err.Error() — утечка внутреннего сообщения")
	}
}
