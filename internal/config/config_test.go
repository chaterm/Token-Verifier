package config

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCfg(t *testing.T, content string) *File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tv.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestLoad(t *testing.T) {
	f := writeCfg(t, `
version: 1
thresholds:
  onetoken: 0.15
  tokenizer: 0.01
`)
	if f.Version != 1 {
		t.Errorf("version = %d, want 1", f.Version)
	}
	if f.Thresholds["onetoken"] != 0.15 || f.Thresholds["tokenizer"] != 0.01 {
		t.Errorf("thresholds = %v", f.Thresholds)
	}
}

func TestLoadErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("不存在的文件应报错")
	}
	path := filepath.Join(t.TempDir(), "bad.yaml")
	os.WriteFile(path, []byte(":\nbad: [yaml"), 0o644)
	if _, err := Load(path); err == nil {
		t.Error("非法 YAML 应报错")
	}
}

func TestResolveThresholdsFlagOverConfig(t *testing.T) {
	cfg := writeCfg(t, "thresholds:\n  onetoken: 0.15\n")
	// flag 覆盖 config
	got, err := ResolveThresholds(
		[]string{"onetoken"},
		map[string]float64{"onetoken": 0.2},
		cfg, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got["onetoken"].Value != 0.2 || got["onetoken"].Source != "flag" {
		t.Errorf("got %+v, want 0.2 from flag", got["onetoken"])
	}
	// 只有 config
	got, err = ResolveThresholds([]string{"onetoken"}, nil, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got["onetoken"].Value != 0.15 || got["onetoken"].Source != "config" {
		t.Errorf("got %+v, want 0.15 from config", got["onetoken"])
	}
	// 两处都没有 → MissingThresholdError
	_, err = ResolveThresholds([]string{"needle"}, nil, cfg, nil)
	var mte MissingThresholdError
	if !errors.As(err, &mte) {
		t.Fatalf("err = %v, want MissingThresholdError", err)
	}
	if !strings.Contains(mte.Error(), "needle") {
		t.Errorf("错误信息应指名探针: %v", mte.Error())
	}
}

func TestResolveThresholdsValidation(t *testing.T) {
	cfg := writeCfg(t, "thresholds:\n  onetoken: -1\n  tokenizer: 1.5\n")
	// 非正数
	if _, err := ResolveThresholds([]string{"onetoken"}, nil, cfg, nil); err == nil {
		t.Error("负阈值应报错")
	}
	// alpha 类须在 (0,1)
	if _, err := ResolveThresholds([]string{"tokenizer"}, nil, cfg, map[string]bool{"tokenizer": true}); err == nil {
		t.Error("alpha >= 1 应报错")
	}
}

func TestResolveThresholdsNaNInf(t *testing.T) {
	// NaN 与任何数比较都为 false，能绕过 <=0 / >=1 检查，让判定恒 pass —— 必须拒绝
	nanCfg := writeCfg(t, "thresholds:\n  onetoken: .nan\n  tokenizer: .inf\n")
	if _, err := ResolveThresholds([]string{"onetoken"}, nil, nanCfg, nil); err == nil {
		t.Error("NaN 阈值应报错")
	}
	if _, err := ResolveThresholds([]string{"tokenizer"}, nil, nanCfg, map[string]bool{"tokenizer": true}); err == nil {
		t.Error("+Inf 阈值应报错")
	}
	// flag 路径同理：ParseFloat 接受 "NaN"/"Inf"
	flags := map[string]float64{"onetoken": math.NaN()}
	if _, err := ResolveThresholds([]string{"onetoken"}, flags, nil, nil); err == nil {
		t.Error("flag NaN 阈值应报错")
	}
	flags = map[string]float64{"onetoken": math.Inf(-1)}
	if _, err := ResolveThresholds([]string{"onetoken"}, flags, nil, nil); err == nil {
		t.Error("flag -Inf 阈值应报错")
	}
}

func TestParseThresholdFlag(t *testing.T) {
	id, val, err := ParseThresholdFlag("onetoken=0.15")
	if err != nil || id != "onetoken" || val != 0.15 {
		t.Errorf("got (%v,%v,%v)", id, val, err)
	}
	if _, _, err := ParseThresholdFlag("bad"); err == nil {
		t.Error("缺 = 应报错")
	}
	if _, _, err := ParseThresholdFlag("=0.5"); err == nil {
		t.Error("空探针名应报错")
	}
	if _, _, err := ParseThresholdFlag("x=abc"); err == nil {
		t.Error("非数字应报错")
	}
}

func TestBackoffDefaults(t *testing.T) {
	f := writeCfg(t, "version: 1\n")
	if f.Runtime.BackoffBaseMs != 500 || f.Runtime.BackoffMaxMs != 30000 {
		t.Errorf("backoff 缺省 = %d/%d, want 500/30000", f.Runtime.BackoffBaseMs, f.Runtime.BackoffMaxMs)
	}
	f = writeCfg(t, "version: 1\nruntime:\n  backoff_base_ms: 100\n  backoff_max_ms: 2000\n")
	if f.Runtime.BackoffBaseMs != 100 || f.Runtime.BackoffMaxMs != 2000 {
		t.Errorf("backoff = %d/%d, want 100/2000", f.Runtime.BackoffBaseMs, f.Runtime.BackoffMaxMs)
	}
}

func TestReservedFieldsExported(t *testing.T) {
	// 保留字段集合应有单一导出来源，供 adapter 复用（不再各存一份）
	for _, k := range []string{"model", "messages", "input", "system"} {
		if !ReservedFields[k] {
			t.Errorf("ReservedFields 缺 %q", k)
		}
	}
}
