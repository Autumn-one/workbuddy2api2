package appconfig

import (
	"os"
	"path/filepath"
	"testing"
)

// ─────────────── 启动自动开启代理（默认行为） ───────────────
//
// 用户要求（2026-09-13）：软件启动时自动开启代理，不然容易忘。
// 因此"配置文件里什么都不写"必须是自动开启——否则该要求不成立。

// TestProxyAutoDefaultsTrue 缺键 → 自动开启（这是本功能成立的底线）。
func TestProxyAutoDefaultsTrue(t *testing.T) {
	cfg := Default()
	if !cfg.ProxyAuto() {
		t.Fatal("未配置 proxy.auto 时应默认自动开启")
	}
}

// TestProxyAutoExplicitFalse 显式 false → 不自动开启（用户主动关掉的能力仍在）。
func TestProxyAutoExplicitFalse(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	os.WriteFile(fp, []byte(`{"proxy":{"auto":false}}`), 0o600)

	cfg, err := Load(fp)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.ProxyAuto() {
		t.Error("显式 auto=false 时不应自动开启")
	}
}

// TestProxyAutoExplicitTrue 显式 true → 自动开启。
func TestProxyAutoExplicitTrue(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	os.WriteFile(fp, []byte(`{"proxy":{"auto":true}}`), 0o600)

	cfg, err := Load(fp)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if !cfg.ProxyAuto() {
		t.Error("显式 auto=true 时应自动开启")
	}
}

// TestProxyLegacyEnabledKey 旧键 enabled=true 仍然等价于自动开启（兼容既有配置）。
func TestProxyLegacyEnabledKey(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	os.WriteFile(fp, []byte(`{"proxy":{"enabled":true}}`), 0o600)

	cfg, err := Load(fp)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if !cfg.ProxyAuto() {
		t.Error("旧键 enabled=true 应等价于自动开启")
	}
}

// TestProxyDefaultsFilledWithoutConfigKey 不写任何 proxy 键时，
// 路径与健康周期也要填好——启动自动开启需要它们。
func TestProxyDefaultsFilledWithoutConfigKey(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.Proxy.BindingsFile == "" || cfg.Proxy.StateFile == "" {
		t.Errorf("绑定/状态文件路径应有默认值, got %q / %q", cfg.Proxy.BindingsFile, cfg.Proxy.StateFile)
	}
	if cfg.Proxy.PortBase != 34567 {
		t.Errorf("端口基数默认应为 34567, got %d", cfg.Proxy.PortBase)
	}
	if cfg.HealthIntervalDur <= 0 {
		t.Errorf("健康探测周期应有默认值, got %v", cfg.HealthIntervalDur)
	}
}
