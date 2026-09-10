package appconfig

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCreditRefreshDefault 默认配置应启用 30m 额度刷新。
func TestCreditRefreshDefault(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.CreditRefresh != "30m" {
		t.Errorf("默认 credit_refresh=%q want 30m", c.Schedule.CreditRefresh)
	}
	if c.CreditRefreshDur != 30*time.Minute {
		t.Errorf("CreditRefreshDur=%v want 30m", c.CreditRefreshDur)
	}
}

// TestCreditRefreshFromFile 配置文件可覆盖间隔。
func TestCreditRefreshFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"schedule":{"credit_refresh":"15m"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.CreditRefreshDur != 15*time.Minute {
		t.Errorf("CreditRefreshDur=%v want 15m", c.CreditRefreshDur)
	}
}

// TestCreditRefreshDisabled 显式 "0" 表示关闭定时刷新（保留手动刷新）。
func TestCreditRefreshDisabled(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"schedule":{"credit_refresh":"0"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.CreditRefreshDur != 0 {
		t.Errorf("显式 0 应关闭定时刷新，got %v", c.CreditRefreshDur)
	}
}

// TestCreditRefreshInvalidReportsError 非法格式应报错，而非静默用默认值
// （避免用户写了错值却以为已生效）。
func TestCreditRefreshInvalidReportsError(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"schedule":{"credit_refresh":"abc"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(fp); err == nil {
		t.Error("非法 credit_refresh 应报错")
	}
}

// TestCreditRefreshBackwardCompatible 旧配置文件（无 credit_refresh 键）仍可加载并取默认值。
func TestCreditRefreshBackwardCompatible(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	legacy := `{"listen":":9999","schedule":{"checkin_hours":[9,21],"keepalive_hours":[22]}}`
	if err := os.WriteFile(fp, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("旧配置应可加载: %v", err)
	}
	if c.CreditRefreshDur != 30*time.Minute {
		t.Errorf("旧配置应回落默认 30m，got %v", c.CreditRefreshDur)
	}
	if c.Listen != ":9999" {
		t.Errorf("listen 解析错误: %q", c.Listen)
	}
}
