package main

import (
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/appconfig"
	"workbuddy2api/internal/proxy"
)

// ─────────────── 启动时的代理选路决策 ───────────────
//
// 用户要求（2026-09-13）：
//  1. 启动自动开启代理；
//  2. 默认沿用上次的账号→节点对应关系；
//  3. 只有对应关系失效（本机代理/节点变了）才重新分配。

// TestOwnerForMarksUserPorts 沿用用户配置时，端口归属要记成 user ——
// 否则下次关闭代理会去删用户的 listeners。
func TestOwnerForMarksUserPorts(t *testing.T) {
	ls := []proxy.Listener{{Port: 40001, Node: "香港Y01"}, {Port: 40002, Node: "日本Y01"}}

	got := ownerFor(ls, proxy.LevelRespect)
	if len(got) != 2 || got[40001] != "user" || got[40002] != "user" {
		t.Errorf("沿用用户配置时归属应为 user, got %v", got)
	}

	got = ownerFor(ls, proxy.LevelInject)
	if got[40001] != proxy.OwnerGateway {
		t.Errorf("网关注入时归属应为 gateway, got %v", got)
	}
}

// TestProxyStatePathForDefault 状态文件默认与绑定文件同目录
// （配置里什么都不写也要能落盘，否则"记住上次等级"无从谈起）。
func TestProxyStatePathForDefault(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, []byte(`{"listen":"127.0.0.1:8787","auth_dir":"./auths","state_file":"./data/state.json"}`), 0o600)

	cfg, err := appconfig.Load(cfgPath)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	got := proxyStatePathFor(cfg)
	if got == "" {
		t.Fatal("状态文件路径不应为空")
	}
	if filepath.Dir(got) != filepath.Dir(cfg.Proxy.BindingsFile) {
		t.Errorf("状态文件应与绑定文件同目录: %q vs %q", got, cfg.Proxy.BindingsFile)
	}
	// 显式配置优先
	cfg.Proxy.StateFile = "X:/custom/state.json"
	if got := proxyStatePathFor(cfg); got != "X:/custom/state.json" {
		t.Errorf("显式配置应优先, got %q", got)
	}
}

// TestAnyPortListeningNoPorts 无候选端口时必须快速返回 false（不 panic）。
func TestAnyPortListeningNoPorts(t *testing.T) {
	if anyPortListening(nil) {
		t.Error("无端口时应返回 false")
	}
}
