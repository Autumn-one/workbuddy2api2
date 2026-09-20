package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseNested(t *testing.T) {
	raw := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1753600000,"domain":""},"account":{"uid":"u1","enterpriseId":"e1","nickname":"n1"}}`)
	sa, err := Parse(raw)
	if err != nil {
		t.Fatalf("nested parse err: %v", err)
	}
	if sa.AccessToken != "at" || sa.RefreshToken != "rt" || sa.ExpiresAt != 1753600000 {
		t.Errorf("tokens: %+v", sa)
	}
	if sa.UID != "u1" || sa.EnterpriseID != "e1" || sa.Nickname != "n1" {
		t.Errorf("account: %+v", sa)
	}
}

func TestParseFlat(t *testing.T) {
	raw := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1753600000,"uid":"u2","nickname":"n2"}`)
	sa, err := Parse(raw)
	if err != nil || sa.UID != "u2" || sa.AccessToken != "at" {
		t.Fatalf("flat: %+v %v", sa, err)
	}
}

func TestParseMissingToken(t *testing.T) {
	if _, err := Parse([]byte(`{"uid":"u3"}`)); err == nil {
		t.Fatal("want error for missing accessToken")
	}
}

func TestSaveAtomicRoundtrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1753600000,
		UID: "u1", EnterpriseID: "e1", Nickname: "n1", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(fp + ".tmp"); !os.IsNotExist(err) {
		t.Error("tmp file should not remain")
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if b.AccessToken != "at" || b.UID != "u1" || b.EnterpriseID != "e1" {
		t.Errorf("roundtrip: %+v", b)
	}
}

// TestLoadDirLoadsAllValid 所有可解析的 auth 文件都被加载；
// 解析失败的文件不再静默跳过，而是进入 bad 清单供上层告警/保号。
func TestLoadDirLoadsAllValid(t *testing.T) {
	dir := t.TempDir()
	cn := `{"auth":{"accessToken":"at1","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"cn1"}}`
	other := `{"auth":{"accessToken":"at2","refreshToken":"r","expiresAt":1,"domain":"example.com"},"account":{"uid":"u2"}}`
	bad := `not json`
	os.WriteFile(filepath.Join(dir, "workbuddy-cn1.json"), []byte(cn), 0o600)
	os.WriteFile(filepath.Join(dir, "workbuddy-u2.json"), []byte(other), 0o600)
	os.WriteFile(filepath.Join(dir, "workbuddy-bad.json"), []byte(bad), 0o600)

	list, badFiles, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 valid accounts, got %+v", list)
	}
	if len(badFiles) != 1 || filepath.Base(badFiles[0].Path) != "workbuddy-bad.json" {
		t.Fatalf("want 1 bad file workbuddy-bad.json, got %+v", badFiles)
	}
	if UIDFromFileName(badFiles[0].Path) != "bad" {
		t.Fatalf("UIDFromFileName=%q", UIDFromFileName(badFiles[0].Path))
	}
	for _, a := range list {
		if a.FilePath == "" {
			t.Error("FilePath not set")
		}
	}
}

func TestUIDFromFileName(t *testing.T) {
	cases := map[string]string{
		"workbuddy-04b087ef-98b2-4540-8ce3-febe846db34e.json": "04b087ef-98b2-4540-8ce3-febe846db34e",
		"workbuddy-u1.json": "u1",
		"workbuddy-.json":   "",
		"workbuddyX.json":   "", // 无 dash 前缀，非标准命名
		"other.json":        "",
		"workbuddy-u2.bak":  "",
	}
	for name, want := range cases {
		if got := UIDFromFileName(filepath.Join("d", name)); got != want {
			t.Errorf("%s: got %q want %q", name, got, want)
		}
	}
}

// TestSnapshotDirDedup 快照按内容指纹去重：无变化不新增目录，有变化才落新副本。
func TestSnapshotDirDedup(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	os.MkdirAll(authDir, 0o755)
	os.WriteFile(filepath.Join(authDir, "workbuddy-u1.json"),
		[]byte(`{"auth":{"accessToken":"at","refreshToken":"r","expiresAt":1},"account":{"uid":"u1"}}`), 0o600)

	p1, changed, err := SnapshotDir(authDir, 10)
	if err != nil || !changed {
		t.Fatalf("first snapshot: changed=%v err=%v", changed, err)
	}
	if _, err := os.Stat(filepath.Join(p1, "workbuddy-u1.json")); err != nil {
		t.Fatalf("snapshot file missing: %v", err)
	}

	// 内容未变 → 去重命中，不新增快照。
	p2, changed, err := SnapshotDir(authDir, 10)
	if err != nil || changed || p2 != p1 {
		t.Fatalf("dedup: changed=%v p2=%q p1=%q err=%v", changed, p2, p1, err)
	}

	// 内容变化 → 新快照。
	os.WriteFile(filepath.Join(authDir, "workbuddy-u2.json"),
		[]byte(`{"auth":{"accessToken":"bt","refreshToken":"r","expiresAt":1},"account":{"uid":"u2"}}`), 0o600)
	p3, changed, err := SnapshotDir(authDir, 10)
	if err != nil || !changed || p3 == p1 {
		t.Fatalf("second snapshot: changed=%v p3=%q err=%v", changed, p3, err)
	}
	if _, err := os.Stat(filepath.Join(p3, "workbuddy-u2.json")); err != nil {
		t.Fatalf("new snapshot missing u2: %v", err)
	}
}

// TestSnapshotDirPrune 超过 keep 份时最旧快照被修剪。
func TestSnapshotDirPrune(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	os.MkdirAll(authDir, 0o755)
	for i := 0; i < 4; i++ {
		os.WriteFile(filepath.Join(authDir, "workbuddy-u1.json"),
			[]byte(`{"auth":{"accessToken":"v`+string(rune('a'+i))+`","refreshToken":"r","expiresAt":1},"account":{"uid":"u1"}}`), 0o600)
		if _, _, err := SnapshotDir(authDir, 2); err != nil {
			t.Fatalf("snapshot %d: %v", i, err)
		}
	}
	snaps, err := snapshotList(filepath.Join(dir, "auths-backup"))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("want 2 snapshots kept, got %v", snaps)
	}
}

func TestNeedsRefresh(t *testing.T) {
	a := &Auth{ExpiresAt: 0}
	if !a.NeedsRefresh(0) {
		t.Error("zero expiry should need refresh")
	}
	a.ExpiresAt = 9999999999
	if a.NeedsRefresh(0) {
		t.Error("far future should not need refresh")
	}
}
