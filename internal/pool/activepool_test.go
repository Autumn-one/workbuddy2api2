package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// ─────────────── 活跃号池（GUI 复选框：临时指定只用某几个账号）───────────────
//
// 语义：未勾选的账号不参与 chat 选号（pick/兜底/半开探测/粘性/计数一律跳过），
// 但签到/额度刷新等维护路径不受影响（它们按 Disabled 过滤，不看 active）。
// 默认（无任何排除）= 全部活跃，与引入该功能前行为完全一致。

func TestPickSkipsInactive(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Add(&auth.Auth{UID: "u3"})
	p.SetCredits("u1", 99999) // u1 积分最高，正常必被选中
	p.SetCredits("u2", 50)
	p.SetCredits("u3", 30)

	p.SetActive("u1", false)
	p.SetActive("u3", false)

	// 只剩 u2 活跃：100 次抽签必须全部命中 u2。
	for i := 0; i < 100; i++ {
		got := p.Pick()
		if got == nil || got.UID != "u2" {
			t.Fatalf("iter %d: pick=%+v want u2（u1/u3 已被复选框排除）", i, got)
		}
	}
}

func TestPickNilWhenAllInactive(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetActiveSet(nil) // 全不勾 = 无号可用
	if got := p.Pick(); got != nil {
		t.Fatalf("want nil（活跃号池为空）, got %+v", got)
	}
	if p.ServableNow() {
		t.Fatal("ServableNow 应为 false（活跃号池为空）")
	}
}

func TestFallbackSkipsInactive(t *testing.T) {
	// 活跃账号全部冷却时，兜底不得捞出"未勾选"的账号（哪怕它冷却最早到期）。
	p := New("")
	p.Add(&auth.Auth{UID: "hot"})
	p.Add(&auth.Auth{UID: "cold"})
	p.SetCredits("hot", 10)
	p.SetCredits("cold", 10)
	p.Cooldown("hot", CoolSoft, time.Hour, "429")
	p.Cooldown("cold", CoolSoft, 30*time.Minute, "429") // cold 更早到期
	p.SetActive("cold", false)

	got := p.Pick()
	if got == nil || got.UID != "hot" {
		t.Fatalf("兜底应选 hot（cold 未勾选，哪怕它更早到期）, got %+v", got)
	}
}

func TestPickByUIDSkipsInactive(t *testing.T) {
	// 会话粘性直取也必须尊重复选框：粘住一个被排除的账号等于绕过用户指定。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)
	p.SetActive("u1", false)
	if got := p.PickByUID("u1", ""); got != nil {
		t.Fatalf("PickByUID 应为 nil（u1 未勾选）, got %+v", got)
	}
}

func TestAvailableUIDsSkipsInactive(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetActive("u2", false)
	uids := p.AvailableUIDs()
	if len(uids) != 1 || uids[0] != "u1" {
		t.Fatalf("AvailableUIDs=%v want [u1]", uids)
	}
}

func TestCountsDetailedSkipsInactive(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Add(&auth.Auth{UID: "u3"})
	p.SetActive("u3", false)
	total, healthy, cooling, disabled, _ := p.CountsDetailed()
	// total 只计活跃账号（对外口径与选号一致）：u3 不应出现在任何计数里。
	if total != 2 || healthy != 2 || cooling != 0 || disabled != 0 {
		t.Fatalf("counts=(%d,%d,%d,%d) want (2,2,0,0)（u3 未勾选不计）", total, healthy, cooling, disabled)
	}
}

func TestSetActiveSetExact(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Add(&auth.Auth{UID: "u3"})

	p.SetActiveSet([]string{"u1", "u3"})
	if !p.IsActive("u1") || p.IsActive("u2") || !p.IsActive("u3") {
		t.Fatalf("SetActiveSet 后 active 状态错误: u1=%v u2=%v u3=%v",
			p.IsActive("u1"), p.IsActive("u2"), p.IsActive("u3"))
	}

	// 状态列可见：Status.Active 必须与勾选一致。
	st1, _ := p.Status("u1")
	st2, _ := p.Status("u2")
	if !st1.Active || st2.Active {
		t.Fatalf("Status.Active: u1=%v want true, u2=%v want false", st1.Active, st2.Active)
	}

	// 全选 = 传全部 UID → 全部活跃。
	p.SetActiveSet([]string{"u1", "u2", "u3"})
	if !p.IsActive("u2") {
		t.Fatal("全选后 u2 应活跃")
	}
}

func TestSetActiveUnknownUIDNoop(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetActive("ghost", false) // 不存在的账号：静默忽略，不 panic、不污染排除集
	if len(p.inactive) != 0 {
		t.Fatalf("unknown uid 不应写入排除集, inactive=%v", p.inactive)
	}
}

func TestActivePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetActive("u2", false)
	p.Flush()

	// 落盘文件里应含 inactive_accounts。
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(sf.InactiveAccounts) != 1 || sf.InactiveAccounts[0] != "u2" {
		t.Fatalf("InactiveAccounts=%v want [u2]", sf.InactiveAccounts)
	}

	// 重启（新 Pool 加载同一文件）后排除集恢复。
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.Add(&auth.Auth{UID: "u2"})
	if p2.IsActive("u2") {
		t.Fatal("重启后 u2 应仍未勾选")
	}
	if !p2.IsActive("u1") {
		t.Fatal("重启后 u1 应仍勾选")
	}
	// 且选号确实不命中 u2。
	for i := 0; i < 50; i++ {
		if got := p2.Pick(); got == nil || got.UID != "u1" {
			t.Fatalf("重启后 iter %d: pick=%+v want u1", i, got)
		}
	}
}

func TestActiveOldStateFileDefaultsAllActive(t *testing.T) {
	// 旧版 state.json（无 inactive_accounts 字段）→ 全部活跃（向后兼容）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":100}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	if !p.IsActive("u1") {
		t.Fatal("旧 state 文件加载后应默认全部活跃")
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("旧 state 文件加载后应能正常选中 u1, got %+v", got)
	}
}

func TestSyncToDirCleansInactive(t *testing.T) {
	// 账号文件被删除后，其排除集残留必须一并清掉（防 UID 幽灵）。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetActive("u2", false)
	// 模拟 u2 凭证文件被删：目录只剩 u1。
	p.SyncToDir([]*auth.Auth{{UID: "u1"}})
	if len(p.inactive) != 0 {
		t.Fatalf("删除账号后排除集应为空, inactive=%v", p.inactive)
	}
}

func TestInactiveAccountStillMaintained(t *testing.T) {
	// 关键语义核对：未勾选的账号【维护路径不受影响】——
	// Disable/ReenableIfCredits/SetCredits 照常工作（签到续命不被复选框打断）。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetActive("u1", false)
	p.SetCredits("u1", 42)
	st, _ := p.Status("u1")
	if st.Credits != 42 {
		t.Fatalf("未勾选账号的积分更新应照常, credits=%d", st.Credits)
	}
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u1", 100) // 签到解冻照常生效
	st, _ = p.Status("u1")
	if st.Cooling {
		t.Fatal("未勾选账号的签到解冻应照常生效")
	}
}
