package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBindingsPersistAcrossRestart 核心：重启后账号必须走同一节点（IP 稳定）。
func TestBindingsPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "proxy-bindings.json")

	ls := []Listener{
		{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "b", Port: 34568, Node: "日本Y01", Region: RegionJP},
		{Name: "c", Port: 34569, Node: "美国Y01", Region: RegionOther},
	}
	r := NewRegistry(ls)
	// 给三个账号分配（会按负载分散）
	orig := map[string]string{}
	for _, uid := range []string{"u1", "u2", "u3"} {
		l, ok := r.Assign(uid)
		if !ok {
			t.Fatalf("%s 分配失败", uid)
		}
		orig[uid] = l.Node
	}
	r.SaveBindings(fp)

	// 模拟重启：新注册表 + 恢复绑定
	r2 := NewRegistry(ls)
	r2.LoadBindings(fp)
	for uid, wantNode := range orig {
		n := r2.NodeFor(uid)
		if n != wantNode {
			t.Errorf("%s 重启后节点变了: %q → %q（IP 必须稳定）", uid, wantNode, n)
		}
	}
}

// TestLoadBindingsSkipsMissingNodes 节点已不存在时丢弃该绑定（不 panic、不误绑）。
func TestLoadBindingsSkipsMissingNodes(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "b.json")
	os.WriteFile(fp, []byte(`{"bindings":{"u1":"已下线的节点","u2":"香港Y01"}}`), 0o600)

	r := NewRegistry([]Listener{{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK}})
	r.LoadBindings(fp)

	if n := r.NodeFor("u1"); n != "" {
		t.Errorf("已消失节点的绑定应被丢弃, got %q", n)
	}
	if n := r.NodeFor("u2"); n != "香港Y01" {
		t.Errorf("存在的节点应恢复绑定, got %q", n)
	}
}

// TestLoadBindingsMissingFile 文件缺失静默降级（不影响启动）。
func TestLoadBindingsMissingFile(t *testing.T) {
	r := NewRegistry([]Listener{{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK}})
	dir := t.TempDir()
	r.LoadBindings(filepath.Join(dir, "nope.json")) // 不应 panic
	if _, ok := r.Assign("u1"); !ok {
		t.Error("缺失文件后仍应能正常分配")
	}
}

// TestLoadBindingsCorruptFile 损坏文件静默降级。
func TestLoadBindingsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "bad.json")
	os.WriteFile(fp, []byte("{not json"), 0o600)
	r := NewRegistry([]Listener{{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK}})
	r.LoadBindings(fp)
	if _, ok := r.Assign("u1"); !ok {
		t.Error("损坏文件后仍应能正常分配")
	}
}

// TestSaveBindingsEmptyPath 空路径 = 不落盘（降级模式）。
func TestSaveBindingsEmptyPath(t *testing.T) {
	r := NewRegistry([]Listener{{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK}})
	r.Assign("u1")
	r.SaveBindings("") // 不应 panic
}
