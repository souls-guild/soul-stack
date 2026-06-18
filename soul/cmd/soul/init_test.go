package main

import "testing"

// TestResolveInitToken проверяет precedence источников bootstrap-токена для
// `soul init`: явный --token побеждает env SOUL_BOOTSTRAP_TOKEN, при пустом
// флаге берётся env, при обоих пустых — ошибка.
func TestResolveInitToken(t *testing.T) {
	// Пустая env-строка эквивалентна «не задано»: os.Getenv возвращает "", и
	// resolveInitToken трактует это как отсутствие источника. t.Setenv всегда
	// выставляет детерминированное значение и снимает его после теста, изолируя
	// от env окружения CI.
	tests := []struct {
		name      string
		flagToken string
		env       string
		want      string
		wantErr   bool
	}{
		{
			name:      "флаг побеждает env",
			flagToken: "flag-tok",
			env:       "env-tok",
			want:      "flag-tok",
		},
		{
			name:      "флаг побеждает даже без env",
			flagToken: "flag-tok",
			env:       "",
			want:      "flag-tok",
		},
		{
			name:      "env подхватывается когда флаг пуст",
			flagToken: "",
			env:       "env-tok",
			want:      "env-tok",
		},
		{
			name:      "оба пусты → ошибка",
			flagToken: "",
			env:       "",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envBootstrapToken, tt.env)

			got, err := resolveInitToken(tt.flagToken)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveInitToken(%q) = %q, nil; want error", tt.flagToken, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveInitToken(%q) unexpected error: %v", tt.flagToken, err)
			}
			if got != tt.want {
				t.Errorf("resolveInitToken(%q) = %q, want %q", tt.flagToken, got, tt.want)
			}
		})
	}
}
