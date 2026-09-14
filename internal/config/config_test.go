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
	probes := map[string][]int{"onetoken": nil}
	// flag 覆盖 config
	got, err := ResolveThresholds(
		probes,
		map[string]float64{"onetoken": 0.2},
		cfg, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	th, ok := got["onetoken"].For(0)
	if !ok || th.Value != 0.2 || th.Source != "flag" {
		t.Errorf("got %+v, want 0.2 from flag", th)
	}
	// 只有 config
	got, err = ResolveThresholds(probes, nil, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	th, _ = got["onetoken"].For(0)
	if th.Value != 0.15 || th.Source != "config" {
		t.Errorf("got %+v, want 0.15 from config", th)
	}
	// 两处都没有 → MissingThresholdError
	_, err = ResolveThresholds(map[string][]int{"needle": nil}, nil, cfg, nil)
	var mte MissingThresholdError
	if !errors.As(err, &mte) {
		t.Fatalf("err = %v, want MissingThresholdError", err)
	}
	if !strings.Contains(mte.Error(), "needle") {
		t.Errorf("错误信息应指名探针: %v", mte.Error())
	}
}

func TestResolveThresholdsBucketKeys(t *testing.T) {
	cfg := writeCfg(t, "thresholds:\n  onetoken: 0.15\n  onetoken.8000: 0.12\n")
	probes := map[string][]int{"onetoken": {0, 8000}}

	// 档位专用 > 探针级兜底
	got, err := ResolveThresholds(probes, nil, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if th, _ := got["onetoken"].For(0); th.Value != 0.15 || th.Source != "config" {
		t.Errorf("bucket 0 应兜底 0.15, got %+v", th)
	}
	if th, _ := got["onetoken"].For(8000); th.Value != 0.12 || th.Source != "config" {
		t.Errorf("bucket 8000 应专用 0.12, got %+v", th)
	}

	// 四级优先级：flag <probe>.<bucket> > config <probe>.<bucket> > flag <probe> > config <probe>
	got, err = ResolveThresholds(probes,
		map[string]float64{"onetoken": 0.3, "onetoken.8000": 0.2}, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if th, _ := got["onetoken"].For(0); th.Value != 0.3 || th.Source != "flag" {
		t.Errorf("bucket 0 应取 flag 探针级 0.3, got %+v", th)
	}
	if th, _ := got["onetoken"].For(8000); th.Value != 0.2 || th.Source != "flag" {
		t.Errorf("bucket 8000 应取 flag 档位级 0.2, got %+v", th)
	}

	// 只写满所有档位、不写兜底 → 通过
	cfg2 := writeCfg(t, "thresholds:\n  onetoken.0: 0.1\n  onetoken.8000: 0.12\n")
	got, err = ResolveThresholds(probes, nil, cfg2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if th, _ := got["onetoken"].For(0); th.Value != 0.1 {
		t.Errorf("bucket 0 got %+v", th)
	}
	if th, _ := got["onetoken"].For(8000); th.Value != 0.12 {
		t.Errorf("bucket 8000 got %+v", th)
	}

	// 缺一个档位且无兜底 → MissingThresholdError 指名档位
	cfg3 := writeCfg(t, "thresholds:\n  onetoken.0: 0.1\n")
	_, err = ResolveThresholds(probes, nil, cfg3, nil)
	var mte MissingThresholdError
	if !errors.As(err, &mte) {
		t.Fatalf("err = %v, want MissingThresholdError", err)
	}
	if mte.Bucket == nil || *mte.Bucket != 8000 {
		t.Errorf("错误应指名缺档位 8000, got %+v", mte)
	}
	if !strings.Contains(mte.Error(), "8000") {
		t.Errorf("错误信息应含档位: %v", mte.Error())
	}
}

func TestResolveThresholdsBadBucketKeys(t *testing.T) {
	probes := map[string][]int{"onetoken": {0, 8000}}
	// 档位不在 context_buckets 内 → 报错（笔误防呆）
	cfg := writeCfg(t, "thresholds:\n  onetoken: 0.15\n  onetoken.8001: 0.12\n")
	if _, err := ResolveThresholds(probes, nil, cfg, nil); err == nil {
		t.Error("档位不在 context_buckets 内应报错")
	}
	// 非法档位后缀
	cfg = writeCfg(t, "thresholds:\n  onetoken: 0.15\n  onetoken.abc: 0.12\n")
	if _, err := ResolveThresholds(probes, nil, cfg, nil); err == nil {
		t.Error("非法档位后缀应报错")
	}
	// flag 里的坏档位同样拒绝
	if _, err := ResolveThresholds(probes,
		map[string]float64{"onetoken": 0.15, "onetoken.8001": 0.12}, nil, nil); err == nil {
		t.Error("flag 档位笔误应报错")
	}
	// 未知探针的档位键宽容忽略（与旧版对未知探针的行为一致）
	if _, err := ResolveThresholds(probes, nil,
		writeCfg(t, "thresholds:\n  onetoken: 0.15\n  nosuch.4000: 0.1\n"), nil); err != nil {
		t.Errorf("未知探针档位键应忽略: %v", err)
	}
}

func TestResolveThresholdsBucketValidation(t *testing.T) {
	// 档位键同样走数值校验
	cfg := writeCfg(t, "thresholds:\n  onetoken.0: -1\n")
	if _, err := ResolveThresholds(map[string][]int{"onetoken": {0}}, nil, cfg, nil); err == nil {
		t.Error("档位负阈值应报错")
	}
	// p 值类探针的档位键 alpha 须在 (0,1)
	cfg = writeCfg(t, "thresholds:\n  tokenizer.0: 1.5\n")
	if _, err := ResolveThresholds(map[string][]int{"tokenizer": {0}}, nil, cfg,
		map[string]bool{"tokenizer": true}); err == nil {
		t.Error("档位 alpha >= 1 应报错")
	}
}

func TestResolveThresholdsValidation(t *testing.T) {
	cfg := writeCfg(t, "thresholds:\n  onetoken: -1\n  tokenizer: 1.5\n")
	// 非正数
	if _, err := ResolveThresholds(map[string][]int{"onetoken": nil}, nil, cfg, nil); err == nil {
		t.Error("负阈值应报错")
	}
	// alpha 类须在 (0,1)
	if _, err := ResolveThresholds(map[string][]int{"tokenizer": nil}, nil, cfg, map[string]bool{"tokenizer": true}); err == nil {
		t.Error("alpha >= 1 应报错")
	}
}

func TestResolveThresholdsNaNInf(t *testing.T) {
	// NaN 与任何数比较都为 false，能绕过 <=0 / >=1 检查，让判定恒 pass —— 必须拒绝
	nanCfg := writeCfg(t, "thresholds:\n  onetoken: .nan\n  tokenizer: .inf\n")
	if _, err := ResolveThresholds(map[string][]int{"onetoken": nil}, nil, nanCfg, nil); err == nil {
		t.Error("NaN 阈值应报错")
	}
	if _, err := ResolveThresholds(map[string][]int{"tokenizer": nil}, nil, nanCfg, map[string]bool{"tokenizer": true}); err == nil {
		t.Error("+Inf 阈值应报错")
	}
	// flag 路径同理：ParseFloat 接受 "NaN"/"Inf"
	flags := map[string]float64{"onetoken": math.NaN()}
	if _, err := ResolveThresholds(map[string][]int{"onetoken": nil}, flags, nil, nil); err == nil {
		t.Error("flag NaN 阈值应报错")
	}
	flags = map[string]float64{"onetoken": math.Inf(-1)}
	if _, err := ResolveThresholds(map[string][]int{"onetoken": nil}, flags, nil, nil); err == nil {
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
