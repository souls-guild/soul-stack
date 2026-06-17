package beacon

import (
	"context"
	"errors"
	"testing"
)

// fakeUsage возвращает фиксированный процент использования — детерминированно,
// без зависимости от реального свободного места хоста.
func fakeUsage(percent float64) func(string) (diskUsage, error) {
	return func(string) (diskUsage, error) { return diskUsage{usedPercent: percent}, nil }
}

func TestDiskFullOK(t *testing.T) {
	b := &DiskFull{Usage: fakeUsage(50)}
	state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{"path": "/"}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state != stateDiskOK {
		t.Fatalf("state = %q, want ok", state)
	}
	if data.GetFields()["used_percent"].GetNumberValue() != 50 {
		t.Error("data.used_percent должно нести фактический процент")
	}
	if data.GetFields()["threshold"].GetNumberValue() != diskFullDefaultThreshold {
		t.Error("data.threshold должно нести дефолтный порог 90")
	}
}

func TestDiskFullOverThreshold(t *testing.T) {
	b := &DiskFull{Usage: fakeUsage(92)}
	state, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{"path": "/var"}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state != stateDiskFull {
		t.Fatalf("state = %q, want full (92%% ≥ 90%%)", state)
	}
}

func TestDiskFullAtThresholdIsFull(t *testing.T) {
	// Ровно порог → "full" (граница включающая: использование ≥ threshold).
	b := &DiskFull{Usage: fakeUsage(90)}
	state, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{"path": "/"}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state != stateDiskFull {
		t.Fatalf("state = %q, want full (90%% == порог)", state)
	}
}

func TestDiskFullCustomThreshold(t *testing.T) {
	// 50% при пороге 40 → full; тот же процент при дефолтном пороге был бы ok.
	b := &DiskFull{Usage: fakeUsage(50)}
	state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{
		"path":              "/",
		"threshold_percent": 40,
	}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state != stateDiskFull {
		t.Fatalf("state = %q, want full (50%% ≥ 40%%)", state)
	}
	if data.GetFields()["threshold"].GetNumberValue() != 40 {
		t.Error("data.threshold должно нести кастомный порог 40")
	}
}

func TestDiskFullRealStatfs(t *testing.T) {
	// Сквозной прогон production-сэмплера на t.TempDir (реальная ФС): процент
	// в диапазоне [0,100], проверка не флейчит на самом значении.
	b := NewDiskFull()
	state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{
		"path":              t.TempDir(),
		"threshold_percent": 100, // 100 → full только при полностью забитой ФС
	}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	up := data.GetFields()["used_percent"].GetNumberValue()
	if up < 0 || up > 100 {
		t.Fatalf("used_percent вне [0,100]: %v", up)
	}
	// При пороге 100 ФС обычно "ok" (не забита под завязку); проверяем лишь, что
	// state — один из валидных, без жёсткой привязки к занятости тестового хоста.
	if state != stateDiskOK && state != stateDiskFull {
		t.Fatalf("неожиданный state %q", state)
	}
}

func TestDiskFullStatfsError(t *testing.T) {
	b := &DiskFull{Usage: func(string) (diskUsage, error) { return diskUsage{}, errors.New("ENOENT") }}
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{"path": "/nope"})); err == nil {
		t.Fatal("ожидали ошибку при сбое statfs")
	}
}

func TestDiskFullMissingPath(t *testing.T) {
	b := &DiskFull{Usage: fakeUsage(10)}
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{})); err == nil {
		t.Fatal("ожидали ошибку при отсутствии param path")
	}
}

func TestDiskFullInvalidThreshold(t *testing.T) {
	b := &DiskFull{Usage: fakeUsage(10)}
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{
		"path":              "/",
		"threshold_percent": 0,
	})); err == nil {
		t.Fatal("ожидали ошибку при threshold_percent вне 1..100")
	}
}
