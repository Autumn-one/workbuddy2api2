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

// ─────────────── 启动时继承/失效重分配 ───────────────
//
// 用户要求：默认沿用上次记录的账号→代理对应关系；只有发现它失效
// （本机代理/节点变了）才对【受影响的部分】重新分配。

// TestLoadBindingsReport 如实统计"继承了几个、失效几个"。
func TestLoadBindingsReport(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "b.json")
	os.WriteFile(fp, []byte(`{"bindings":{"u1":"香港Y01","u2":"已下线","u3":"日本Y01"}}`), 0o600)

	r := NewRegistry([]Listener{
		{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "b", Port: 34568, Node: "日本Y01", Region: RegionJP},
	})
	restored, lost := r.LoadBindingsReport(fp)
	if restored != 2 || lost != 1 {
		t.Errorf("restored=%d lost=%d, want 2/1", restored, lost)
	}
	if r.NodeFor("u1") != "香港Y01" || r.NodeFor("u3") != "日本Y01" {
		t.Error("存在的节点应被继承")
	}
	if r.NodeFor("u2") != "" {
		t.Error("已消失节点的绑定不应被继承")
	}
}

// TestOnlyAffectedAccountsRebound 核心：节点变化后，只有受影响的账号换出口，
// 其余账号的对应关系必须原样保留（IP 稳定）。
func TestOnlyAffectedAccountsRebound(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "b.json")
	// 上次：u2 用"香港Y02"（该节点本次仍在），u1 用"香港Y01"（本次仍在）
	os.WriteFile(fp, []byte(`{"bindings":{"u1":"香港Y01","u2":"香港Y02"}}`), 0o600)

	// 本次节点表：新增了一个（订阅更新加节点），原有两个都在
	r := NewRegistry([]Listener{
		{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "b", Port: 34568, Node: "香港Y02", Region: RegionHK},
		{Name: "c", Port: 34569, Node: "香港Y09", Region: RegionHK},
	})
	if restored, lost := r.LoadBindingsReport(fp); restored != 2 || lost != 0 {
		t.Fatalf("应全部继承, got restored=%d lost=%d", restored, lost)
	}
	r.EnsureAllAssigned([]string{"u1", "u2", "u3"})
	if got := r.NodeFor("u1"); got != "香港Y01" {
		t.Errorf("u1 的对应关系不该变: %q", got)
	}
	if got := r.NodeFor("u2"); got != "香港Y02" {
		t.Errorf("u2 的对应关系不该变: %q", got)
	}
	if got := r.NodeFor("u3"); got == "" {
		t.Error("新账号应被分配（哪怕是新增节点）")
	}
}

// TestBindingToAbsentNodeIsRebound 绑定的节点在本次监听表里不存在时，
// 该账号要重新分配（这正是"失效才重分配"的语义）。
//
// 为什么不保留旧绑定：一个账号的出口 IP 只由当前生效的端口决定；
// 保留一个没有端口的绑定不会让它继续走原出口，反而会让列表/日志显示
// 一个不存在的对应关系。
func TestBindingToAbsentNodeIsRebound(t *testing.T) {
	r := NewRegistry([]Listener{{Name: "b", Port: 34568, Node: "日本Y01", Region: RegionJP}})
	if r.Adopt("u1", "香港Y01") {
		t.Fatal("节点不存在时 Adopt 应失败（调用方据此重新分配）")
	}
	l, ok := r.Assign("u1")
	if !ok || l.Node != "日本Y01" {
		t.Errorf("应重新分配到现有节点, got node=%q ok=%v", l.Node, ok)
	}
	if got := r.NodeFor("u1"); got != "日本Y01" {
		t.Errorf("重新分配后对应关系应更新, got %q", got)
	}
}

// TestSaveBindingsPreservesListenOwner 保存绑定时不得丢掉端口归属信息
// （丢了就无法判断"端口是谁配的"，可能误改用户配置）。
func TestSaveBindingsPreservesListenOwner(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "b.json")
	r := NewRegistry([]Listener{{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK}})
	r.Assign("u1")
	r.SaveBindingsWithOwner(fp, map[int]string{34567: OwnerGateway})

	r2 := NewRegistry([]Listener{{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK}})
	r2.Assign("u2")
	r2.SaveBindings(fp) // 普通保存（不传 owner）

	if got := LoadListenOwner(fp); got[34567] != OwnerGateway {
		t.Errorf("listen_owner 应被保留, got %v", got)
	}
}

// TestLoadBindingsOldFormatNoOwner 旧格式文件（无 listen_owner）可读，
// 且端口归属为空（调用方据此按"网关配置的"处理，保证既有行为不变）。
func TestLoadBindingsOldFormatNoOwner(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "old.json")
	os.WriteFile(fp, []byte(`{"bindings":{"u1":"香港Y01"}}`), 0o600)

	r := NewRegistry([]Listener{{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK}})
	if restored, lost := r.LoadBindingsReport(fp); restored != 1 || lost != 0 {
		t.Errorf("旧格式应可读, got %d/%d", restored, lost)
	}
	if got := LoadListenOwner(fp); len(got) != 0 {
		t.Errorf("旧格式的端口归属应为空, got %v", got)
	}
}

// TestSaveBindingsKeepsAbsentAccounts 保存时不得丢掉"当前不在账号池里的账号"
// 的对应关系。
//
// 实测缺陷（2026-09-13）：保存只写当前账号池 → 切配置目录/账号文件临时移走时，
// 那两个账号的对应关系被抹掉；它们回来后会拿到新节点，出口 IP 就变了——
// 而 IP 稳定正是这套机制存在的理由。
func TestSaveBindingsKeepsAbsentAccounts(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "b.json")

	// 上次：三个账号都有对应关系
	os.WriteFile(fp, []byte(`{"bindings":{"u1":"香港Y01","u2":"香港Y02","u3":"香港Y03"}}`), 0o600)

	// 本次只有 u1 出现在账号池里（u2/u3 暂不在）
	r := NewRegistry([]Listener{
		{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "b", Port: 34568, Node: "香港Y02", Region: RegionHK},
		{Name: "c", Port: 34569, Node: "香港Y03", Region: RegionHK},
	})
	r.LoadBindingsReport(fp)
	r.EnsureAllAssigned([]string{"u1"})
	r.SaveBindings(fp)

	again, _ := readBindingFile(fp)
	for _, uid := range []string{"u1", "u2", "u3"} {
		if again.Bindings[uid] == "" {
			t.Errorf("%s 的对应关系不应被抹掉（got %v）", uid, again.Bindings)
		}
	}
}

// TestSaveBindingsDropsRemovedAccount 显式解绑（账号删除）的账号不再写回，
// 否则"删掉的账号"会从磁盘历史里复活。
func TestSaveBindingsDropsRemovedAccount(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "b.json")
	os.WriteFile(fp, []byte(`{"bindings":{"u1":"香港Y01","u2":"香港Y02"}}`), 0o600)

	r := NewRegistry([]Listener{
		{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "b", Port: 34568, Node: "香港Y02", Region: RegionHK},
	})
	r.LoadBindingsReport(fp)
	r.Unassign("u2") // 账号被删除
	r.SaveBindings(fp)

	again, _ := readBindingFile(fp)
	if again.Bindings["u2"] != "" {
		t.Errorf("已解绑的账号不应写回, got %v", again.Bindings)
	}
	if again.Bindings["u1"] != "香港Y01" {
		t.Errorf("其它账号应保留, got %v", again.Bindings)
	}
}
