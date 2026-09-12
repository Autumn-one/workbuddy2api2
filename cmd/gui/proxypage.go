// proxypage.go — 账号出口代理的装配与界面。
//
// 用途（用户实测场景）：上游开始按 IP 处置账号，同 IP 下多账号共用会被批量风控
// （换 IP 后即可正常注册）。本功能让每个账号走一个独立的本地 Clash 端口
// （= 独立出口 IP），降低"同 IP 多账号"的特征。
//
// 与 Clash 的协作方式：
//   - 网关只【读】Clash API 的节点列表，并生成 listeners 配置文本供用户粘贴；
//   - 不改 Clash 任何配置（用户手动贴到 Verge 的 Merge 覆写文件）；
//   - 每个 listener 固定绑一个节点，网关按"账号 → 端口"发请求。
//
// 为什么不用 Clash API 动态切节点：节点选择是全局状态，并发请求会互相踩。
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api/internal/appconfig"
	"workbuddy2api/internal/proxy"
	"workbuddy2api/internal/upstream"
)

// regionSummary 统计各地区节点数（日志用）。
func regionSummary(ls []proxy.Listener) string {
	n := map[proxy.Region]int{}
	for _, l := range ls {
		n[l.Region]++
	}
	parts := make([]string, 0, 4)
	for _, r := range []proxy.Region{proxy.RegionHK, proxy.RegionTW, proxy.RegionJP, proxy.RegionOther} {
		if n[r] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", r, n[r]))
		}
	}
	return strings.Join(parts, " · ")
}

// proxySelectorFor 返回注入给 upstream 的选择器：账号 → 代理 URL。
func proxySelectorFor(reg *proxy.Registry) upstream.ProxySelector {
	if reg == nil {
		return nil
	}
	return func(uid string) string {
		l, ok := reg.Assign(uid)
		if !ok {
			return "" // 无可用节点 → 直连
		}
		return l.ProxyURL()
	}
}

// autoEnableProxyAtStartup 启动阶段自动开启代理（默认行为，用户要求）。
//
// 为什么默认开启：代理是"防上游按 IP 批量风控"的基础设施，忘记开启会让
// 所有账号以同一真实 IP 暴露（用户明确说过"容易忘"）。
// 想关掉就把配置写成 proxy.auto=false（或点界面上的「关闭代理」）。
//
// 与「一键开关」走完全相同的代码路径（enableProxyNow），保证行为一致——
// 避免"启动启用"与"手动开关"两套逻辑各写一遍而漂移。
func (a *app) autoEnableProxyAtStartup() {
	if a.svc.Upstream() == nil {
		return // 服务未启动，等用户点开关
	}
	msg, err := a.enableProxyNow()
	if err != nil {
		// 代理不可用不影响网关服务（全部直连），只是如实记录原因。
		log.Printf("代理：启动自动开启失败（%v）；可在「代理」页点「开启代理」重试", err)
		a.mw.Synchronize(func() { a.refreshProxyToggle() })
		return
	}
	log.Printf("代理（启动自动启用）：%s", msg)
	a.mw.Synchronize(func() {
		a.refreshProxyBindings()
		a.refreshProxyToggle()
	})
}

// refreshProxyBindings 刷新账号页的代理绑定显示（哪个账号在用哪个节点）。
func (a *app) refreshProxyBindings() {
	// 只依赖 model（表格控件由 declarative 绑定，model 才是数据源）。
	// 这样在无 GUI 的测试环境里也能验证列表行为，不必构造控件。
	if a.proxyBindings == nil {
		return
	}
	// 代理已关闭：必须清空列表（否则旧行会残留在界面上，看起来"还开着"）。
	if a.proxyReg == nil {
		a.proxyBindings.Replace(nil)
		return
	}
	// 代理运行期新增的账号可能还没有绑定（分配是懒执行的）→ 先补齐，
	// 保证列表始终显示"全部账号各自走哪个节点"，不漏行。
	uids := make([]string, 0, len(a.svc.Accounts()))
	for _, st := range a.svc.Accounts() {
		uids = append(uids, st.UID)
	}
	a.proxyReg.EnsureAllAssigned(uids)

	snap := a.proxyReg.Snapshot()
	rows := make([]proxyBindingRow, 0, len(snap))
	for _, b := range snap {
		rows = append(rows, proxyBindingRow{
			UID:     b.UID,
			Name:    a.displayName(b.UID),
			Node:    b.Node, // 可读节点名（不是 URL）——用户明确要求
			Port:    b.Port,
			Region:  b.Region.String(),
			Healthy: a.proxyReg.Healthy(b.Port),
			Delay:   a.proxyReg.DelayOf(b.Node),
		})
	}
	a.proxyBindings.Replace(rows)
	a.syncNodePickOptions()
	if a.lblProxyHint != nil {
		ls := a.proxyReg.Listeners()
		a.lblProxyHint.SetText(fmt.Sprintf("共 %d 个节点可用（%s）· 已绑定 %d 个账号",
			len(ls), regionSummary(ls), len(rows)))
	}
}

// doSwitchAccountNode 把【代理列表里选中的那一行】换到另一个健康节点。
//
// 设计修正（用户反馈）：此前读的是【账号页】的选中行，却放在代理页——
// 点了会"凭空冒出一行"（给那个账号新分配节点）。现在只作用于代理列表自身的选中行。
func (a *app) doSwitchAccountNode() {
	if a.proxyReg == nil {
		a.lblProxyHint.SetText("代理未启用：请先点「开启代理」")
		return
	}
	if a.tvProxyBindings == nil {
		return
	}
	idx := a.tvProxyBindings.CurrentIndex()
	row, ok := a.proxyBindings.At(idx)
	if !ok {
		a.lblProxyHint.SetText("请先在上面的列表里选中一个账号")
		return
	}
	uid := row.UID
	if uid == "" {
		a.lblProxyHint.SetText("请先在上面的列表里选中一个账号")
		return
	}
	before := a.proxyReg.NodeFor(uid)
	l, ok := a.proxyReg.Rebind(uid)
	if !ok {
		a.lblProxyHint.SetText("没有其他健康节点可切换")
		return
	}
	a.proxyReg.SaveBindings(a.proxyBindingsPath)
	a.refreshProxyBindings()
	// 保持选中行（refreshProxyBindings 会重建表格）
	a.selectProxyBinding(uid)
	a.lblProxyHint.SetText(fmt.Sprintf("%s：%s → %s（端口 %d）",
		a.displayName(uid), orDash(before), l.Node, l.Port))
	log.Printf("代理：账号 %s 切换节点 %s → %s（端口 %d）",
		a.displayName(uid), orDash(before), l.Node, l.Port)
}

// selectProxyBinding 在代理绑定表里选中指定账号那一行（找不到则不动）。
func (a *app) selectProxyBinding(uid string) {
	if a.tvProxyBindings == nil {
		return
	}
	for i := 0; i < a.proxyBindings.RowCount(); i++ {
		if r, ok := a.proxyBindings.At(i); ok && r.UID == uid {
			_ = a.tvProxyBindings.SetCurrentIndex(i)
			return
		}
	}
}

// probeProxyHealth 检测全部节点的真实连通性（Clash 批量延迟接口）。
//
// 为什么用延迟接口而非 TCP 探测：TCP 只能证明"Clash 在监听端口"，
// 不能证明出口节点真能出网。延迟接口由 Clash 自己发探测（不经过本网关、
// 不消耗上游模型积分），一次请求即拿到全部节点的真实可用性与延迟。
func (a *app) probeProxyHealth() (healthy, total int, err error) {
	if a.proxyReg == nil {
		return 0, 0, fmt.Errorf("代理未启用")
	}
	ep := a.clashEndpoint()
	delays, derr := proxy.FetchDelays(ep.API, ep.Secret, "GLOBAL", proxy.DelayTimeout)
	if derr != nil {
		return 0, 0, fmt.Errorf("延迟检测失败: %w", derr)
	}
	a.proxyReg.ApplyDelays(delays)
	total = len(a.proxyReg.Listeners())
	for _, l := range a.proxyReg.Listeners() {
		if a.proxyReg.Healthy(l.Port) {
			healthy++
		}
	}
	return healthy, total, nil
}

// clashEndpoint 返回当前 Clash 端点（自动发现，缓存上次结果）。
func (a *app) clashEndpoint() proxy.ClashEndpoint {
	if a.clashEP != nil {
		return *a.clashEP
	}
	ep, _ := proxy.DiscoverClashEndpoint(vergeDataDir(), "")
	a.clashEP = &ep
	return ep
}

// doProbeProxiesNow 立即检测一轮（用户手动触发，低成本）。
func (a *app) doProbeProxiesNow() {
	if a.proxyReg == nil {
		a.lblProxyHint.SetText("代理未启用：请先点「开启代理」")
		return
	}
	a.lblProxyHint.SetText("正在检测节点连通性…")
	go func() {
		healthy, total, err := a.probeProxyHealth()
		a.mw.Synchronize(func() {
			if err != nil {
				a.lblProxyHint.SetText("检测失败：" + firstLine(err.Error()))
				return
			}
			a.refreshProxyBindings()
			a.lblProxyHint.SetText(fmt.Sprintf("检测完成：%d/%d 个节点真实可用（延迟探测，未消耗积分）", healthy, total))
		})
	}()
}

// doSelectNodeForAccount 把选中账号手动指定到你挑选的节点。
func (a *app) doSelectNodeForAccount() {
	if a.proxyReg == nil {
		a.lblProxyHint.SetText("代理未启用：请先点「开启代理」")
		return
	}
	if a.tvProxyBindings == nil || a.cbNodePick == nil {
		return
	}
	row, ok := a.proxyBindings.At(a.tvProxyBindings.CurrentIndex())
	if !ok || row.UID == "" {
		a.lblProxyHint.SetText("请先在上面的列表里选中一个账号")
		return
	}
	idx := a.cbNodePick.CurrentIndex()
	if idx < 0 || idx >= len(a.nodePickValues) {
		a.lblProxyHint.SetText("请先在下拉框里选择一个节点")
		return
	}
	node := a.nodePickValues[idx]
	before := a.proxyReg.NodeFor(row.UID)
	if !a.proxyReg.SetNodeForAccount(row.UID, node) {
		a.lblProxyHint.SetText("指定失败：节点不存在")
		return
	}
	a.proxyReg.SaveBindings(a.proxyBindingsPath)
	a.refreshProxyBindings()
	a.selectProxyBinding(row.UID)
	a.lblProxyHint.SetText(fmt.Sprintf("%s：%s → %s（手动指定）",
		a.displayName(row.UID), orDash(before), node))
	log.Printf("代理：手动指定 %s 的节点 %s → %s", a.displayName(row.UID), orDash(before), node)
}

// doAutoAssignSelected 把选中账号交还自动分配（按地区优先+可用挑节点）。
func (a *app) doAutoAssignSelected() {
	if a.proxyReg == nil {
		a.lblProxyHint.SetText("代理未启用：请先点「开启代理」")
		return
	}
	if a.tvProxyBindings == nil {
		return
	}
	row, ok := a.proxyBindings.At(a.tvProxyBindings.CurrentIndex())
	if !ok || row.UID == "" {
		a.lblProxyHint.SetText("请先在上面的列表里选中一个账号")
		return
	}
	before := a.proxyReg.NodeFor(row.UID)
	l, ok := a.proxyReg.ClearNodeForAccount(row.UID)
	if !ok {
		a.lblProxyHint.SetText("没有可用节点可分配")
		return
	}
	a.proxyReg.SaveBindings(a.proxyBindingsPath)
	a.refreshProxyBindings()
	a.selectProxyBinding(row.UID)
	a.lblProxyHint.SetText(fmt.Sprintf("%s：%s → %s（已交还自动分配）",
		a.displayName(row.UID), orDash(before), l.Node))
	log.Printf("代理：%s 交还自动分配 %s → %s", a.displayName(row.UID), orDash(before), l.Node)
}

// syncNodePickOptions 刷新"可选节点"下拉框（含健康状态与延迟）。
func (a *app) syncNodePickOptions() {
	if a.cbNodePick == nil || a.proxyReg == nil {
		return
	}
	opts := a.proxyReg.NodeOptions()
	names := make([]string, 0, len(opts))
	values := make([]string, 0, len(opts))
	for _, o := range opts {
		label := o.Node
		if o.Healthy {
			if o.Delay > 0 {
				label = fmt.Sprintf("%s（%s %dms）", o.Node, o.Region, o.Delay)
			} else {
				label = fmt.Sprintf("%s（%s 可用）", o.Node, o.Region)
			}
		} else {
			label = fmt.Sprintf("%s（%s 不可用）", o.Node, o.Region)
		}
		names = append(names, label)
		values = append(values, o.Node)
	}
	if equalStrs(values, a.nodePickValues) {
		return // 选项未变不重建，避免打断选择
	}
	a.nodePickValues = values
	if err := a.cbNodePick.SetModel(names); err != nil {
		logf("节点下拉框刷新失败: %v", err)
		return
	}
	if len(names) > 0 {
		a.cbNodePick.SetCurrentIndex(0)
	}
}

// vergeDataDir 返回 Clash Verge 的数据目录（定位渲染出的完整配置）。
// 支持多版本目录名（Verge 与 Verge Rev 的标识不同）。
func vergeDataDir() string {
	base := os.Getenv("APPDATA")
	if base == "" {
		return ""
	}
	cands := []string{
		filepath.Join(base, "io.github.clash-verge-rev.clash-verge-rev"),
		filepath.Join(base, "clash-verge"),
		filepath.Join(base, "io.github.zzzgydi.clash-verge"),
	}
	for _, d := range cands {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
	}
	return ""
}

// doApplyProxyAuto 一键把 listeners 应用到 Clash 并重载生效（无需手工粘贴）。
func (a *app) doApplyProxyAuto() {
	if a.proxyReg == nil {
		a.lblProxyHint.SetText("代理未启用：请先在 config.json 里设置 proxy.enabled=true 并重启")
		return
	}
	if a.cfg == nil || a.cfg.Proxy.ClashAPI == "" {
		a.lblProxyHint.SetText("未配置 clash_api")
		return
	}
	dir := vergeDataDir()
	if dir == "" {
		a.lblProxyHint.SetText("未找到 Clash Verge 数据目录（请确认 Verge 已安装并运行过）")
		return
	}
	ls := a.proxyReg.Listeners()
	a.lblProxyHint.SetText("正在应用配置到 Clash…")

	go func() {
		cfgPath, err := proxy.ApplyListeners(a.cfg.Proxy.ClashAPI, a.cfg.Proxy.ClashSecret, dir, ls)
		a.mw.Synchronize(func() {
			if err != nil {
				a.lblProxyHint.SetText("自动应用失败：" + firstLine(err.Error()) +
					"（可点「复制配置」手工粘到 Verge 的 Merge.yaml 后重启 Clash）")
				return
			}
			// 应用后立即探测，确认端口真的起来了
			ok := 0
			for _, l := range ls {
				if proxy.ProbePort(l.Port) {
					a.proxyReg.MarkHealthy(l.Port)
					ok++
				}
			}
			a.refreshProxyBindings()
			a.lblProxyHint.SetText(fmt.Sprintf("已自动应用到 Clash（%s）：%d/%d 个端口可用，无需重启",
				filepath.Base(cfgPath), ok, len(ls)))
		})
	}()
}

// enableProxyNow 在【运行期】启用代理：自动发现 Clash → 拉节点 → 建分配表
// → （仅在必要时）应用 listeners 到 Clash → 注入选择器。全程无需改配置或重启。
//
// 这是用户要求的"简单"入口：界面上一个开关，点了就用；启动时也会自动走这条路。
//
// 两条关键行为（用户要求，2026-09-13）：
//  1. **尽量不改用户的 Clash 配置**：先实测既有端口能不能用；能用就完全不动配置。
//     用户可能已经自己把 listeners 配好了（例如写进 Verge 的 Merge 覆写文件，
//     订阅更新也不会丢），那就不该再让工具去覆盖一次。
//  2. **默认沿用上次的账号→节点对应关系**：只有节点真的不在了，才重分配那一部分。
func (a *app) enableProxyNow() (string, error) {
	dir := vergeDataDir()
	if dir == "" {
		return "", fmt.Errorf("未找到 Clash Verge 数据目录（请确认 Verge 已安装并运行）")
	}
	// 自动发现 Clash 端点（用户不需要填 clash_api / secret）
	ep, err := proxy.DiscoverClashEndpoint(dir, "")
	if err != nil {
		return "", err
	}
	if ok, msg := proxy.VerifyEndpoint(ep.API, ep.Secret); !ok {
		return "", fmt.Errorf("连接 Clash 失败（%s）：%s", ep.API, msg)
	}
	nodes, err := proxy.FetchNodes(ep.API, ep.Secret)
	if err != nil {
		return "", fmt.Errorf("读取 Clash 节点失败: %w", err)
	}
	base := proxy.DefaultPortBase
	if a.cfg != nil && a.cfg.Proxy.PortBase > 0 {
		base = a.cfg.Proxy.PortBase
	}
	genLS := proxy.BuildListeners(nodes, base)
	if len(genLS) == 0 {
		return "", fmt.Errorf("未取到任何节点")
	}

	bindPath := a.proxyBindingsPath
	if bindPath == "" {
		bindPath = filepath.Join(filepath.Dir(a.cfg.StateFile), "proxy-bindings.json")
	}
	statePath := a.proxyStatePath
	if statePath == "" {
		statePath = proxyStatePathFor(a.cfg)
	}

	// ─── 步骤 1：读现行 Clash 配置，找出【用户自己配的】listeners ───
	//
	// 用户的端口未必是 34567 起（自己粘进 Merge 覆写文件时常用别的号），
	// 所以先按配置文本解析出"端口 → 节点"，再实测它们能不能用。
	userLS, cfgReadNote := a.readUserListeners(dir)

	prevLevel := a.proxyLevel
	if prevLevel == proxy.LevelDisabled {
		prevLevel = proxy.LoadLevel(statePath) // 上次确认过的等级（重启后仍记得）
	}
	// 只对"用户自己配的端口"做实测：它们是否真的在监听、以及能否转发出网。
	// 没配的话就没得测（直接走注入），连多余的连接都省掉。
	probes := a.probeExistingPorts(ep, userLS)
	listening := len(userLS) > 0 && anyPortListening(userLS)

	// ─── 步骤 2：决定等级，并按等级决定是否改写 Clash 配置 ───
	dec := proxy.Decide(userLS, probes, listening, prevLevel)
	level, applyNote := dec.Level, dec.Reason
	ls := genLS
	if dec.UseUserPorts {
		ls = userLS // 用他自己的端口表（连端口号都不换，出口 IP 才稳定）
		if missing := proxy.CoverAllNodes(ls, nodes); len(missing) > 0 {
			applyNote += fmt.Sprintf("；%d 个节点没有对应端口，暂不参与分配", len(missing))
		}
	} else {
		if _, err := proxy.ApplyListeners(ep.API, ep.Secret, dir, genLS); err != nil {
			return "", fmt.Errorf("应用到 Clash 失败: %w", err)
		}
		time.Sleep(1200 * time.Millisecond) // 等 Clash 重载
	}
	applyNote += cfgReadNote

	// ─── 步骤 3：建分配表并继承上次的账号→节点对应关系 ───
	reg := proxy.NewRegistry(ls)
	restored, lost := reg.LoadBindingsReport(bindPath)

	// 【关键】为全部账号立刻建立绑定：让界面一开启就显示"每个账号走哪个节点"，
	// 而不是等请求发生才懒分配（那样列表默认空白，用户以为没生效）。
	// 已绑定的不会被改动 —— 这就是"默认沿用上次的对应关系"。
	uids := make([]string, 0, len(a.svc.Accounts()))
	for _, st := range a.svc.Accounts() {
		uids = append(uids, st.UID)
	}
	reg.EnsureAllAssigned(uids)

	// 检测真实连通性（延迟接口）。失败时回落 TCP 探测（至少确认端口在监听）。
	delayNote := ""
	if delays, derr := proxy.FetchDelays(ep.API, ep.Secret, "GLOBAL", proxy.DelayTimeout); derr == nil {
		reg.ApplyDelays(delays)
	} else {
		for _, l := range ls {
			if proxy.ProbePort(l.Port) {
				reg.MarkHealthy(l.Port)
			}
		}
		delayNote = "（延迟检测不可用，已回落端口探测）"
	}
	ok := 0
	for _, l := range ls {
		if reg.Healthy(l.Port) {
			ok++
		}
	}

	// 注入到 upstream（运行期生效，旧请求不受影响）
	up := a.svc.Upstream()
	if up == nil {
		return "", fmt.Errorf("服务未运行（请先启动服务）")
	}
	up.SetProxySelector(proxySelectorFor(reg))

	// 保存状态供界面显示与退出时落盘
	a.proxyReg = reg
	a.proxyBindingsPath = bindPath
	a.proxyStatePath = statePath
	a.proxyLevel = level
	if a.proxyCancel != nil {
		a.proxyCancel() // 停掉旧的健康探测循环
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.proxyCancel = cancel
	go reg.HealthLoop(ctx, proxy.DefaultHealthInterval)

	// 落盘：既要绑定，也要"这些端口是谁配的"——否则下次无法判断能不能动它们。
	reg.SaveBindingsWithOwner(bindPath, ownerFor(ls, level))
	proxy.SaveLevel(statePath, level, applyNote)

	// 如实汇报：沿用了多少、有多少因失效被重分配
	inherit := ""
	if restored > 0 {
		inherit = fmt.Sprintf("，沿用上次对应关系 %d 个", restored)
	}
	if lost > 0 {
		inherit += fmt.Sprintf("；%d 个账号的节点已不存在，已重新分配", lost)
	}
	return fmt.Sprintf("已启用：%d 个节点（%s），%d/%d 个真实可用；%s%s%s",
		len(ls), regionSummary(ls), ok, len(ls), applyNote, inherit, delayNote), nil
}

// readUserListeners 读现行 Clash 配置，解析出用户自己配置的 listeners。
//
// 读不到配置不算失败（返回空 + 说明），此时按"没有用户配置"处理，
// 走注入路径——保证功能不会因为读不到文件而整体不可用。
func (a *app) readUserListeners(dataDir string) ([]proxy.Listener, string) {
	cfgPath, err := proxy.FindRenderedConfig(dataDir)
	if err != nil {
		return nil, ""
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, ""
	}
	us := proxy.ParseUserListeners(string(raw))
	if len(us) == 0 {
		return nil, ""
	}
	return proxy.UserListenersToListeners(us), ""
}

// probeExistingPorts 实测"用户自己配置的端口"是否真能用。
//
// 这是整个"不改用户配置"判断的基础：只有真的拿这些端口发出去请求、
// 拿到了响应，才敢说"用户的配置是好的，不需要动它"。
//
// 目标是本机 Clash 的只读接口 /version —— 不涉及上游、不消耗额度、不碰账号。
// 没有用户端口（或还没有探测目标）时返回 nil。
func (a *app) probeExistingPorts(ep proxy.ClashEndpoint, userLS []proxy.Listener) []proxy.ProbeResult {
	if len(userLS) == 0 {
		return nil
	}
	host := proxy.APIHostPort(ep.API)
	if host == "" {
		return nil
	}
	cands := make([]int, 0, len(userLS))
	for _, l := range userLS {
		cands = append(cands, l.Port)
	}
	return proxy.ProbePorters(cands, host, 3*time.Second)
}

// anyPortListening 是否有任一候选端口在监听（TCP 层）。
// 与"能转发出网"分开：用来区分"配置被删了"与"配了但节点暂时不通"——
// 只有前者才回退到改写配置，后者应当尊重用户配置。
func anyPortListening(ls []proxy.Listener) bool {
	for _, l := range ls {
		if proxy.ProbePort(l.Port) {
			return true
		}
	}
	return false
}

// ownerFor 生成端口归属表：沿用状态下这些端口是用户配的，注入状态下是网关配的。
func ownerFor(ls []proxy.Listener, level proxy.TakeoverLevel) map[int]string {
	out := make(map[int]string, len(ls))
	who := proxy.OwnerGateway
	if level == proxy.LevelRespect {
		who = "user"
	}
	for _, l := range ls {
		out[l.Port] = who
	}
	return out
}

// proxyStatePathFor 接管等级状态文件路径（默认与绑定文件同目录）。
func proxyStatePathFor(cfg *appconfig.Config) string {
	if cfg == nil {
		return ""
	}
	if cfg.Proxy.StateFile != "" {
		return cfg.Proxy.StateFile
	}
	if cfg.Proxy.BindingsFile != "" {
		return filepath.Join(filepath.Dir(cfg.Proxy.BindingsFile), "proxy-state.json")
	}
	if cfg.StateFile != "" {
		return filepath.Join(filepath.Dir(cfg.StateFile), "proxy-state.json")
	}
	return ""
}

// disableProxyNow 在【运行期】关闭代理：立刻回落直连，不重启。
//
// 关于 Clash 配置：只清理【本程序自己写入的】自动注入块。
// 如果当前是「沿用用户既有 listeners」状态（LevelRespect），就一个字都不动——
// 那是用户自己的配置，我们没有资格删。
//
// 注意：关闭只在【本次运行】生效，重启后仍会按默认行为自动开启
// （用户要求"不用每次记得开"；要长期关闭请设 proxy.auto=false）。
func (a *app) disableProxyNow() (string, error) {
	up := a.svc.Upstream()
	if up == nil {
		return "", fmt.Errorf("服务未运行")
	}
	up.SetProxySelector(nil) // 立刻回落直连

	if a.proxyCancel != nil {
		a.proxyCancel()
		a.proxyCancel = nil
	}
	cleaned := "（Clash 里的自动配置已清理）"
	if a.proxyLevel == proxy.LevelRespect {
		cleaned = "（你的 listeners 配置保持不动）"
	} else if dir := vergeDataDir(); dir != "" {
		// 清理 Clash 里的自动注入块（传空列表 = 只清理）
		ep, err := proxy.DiscoverClashEndpoint(dir, "")
		if err == nil {
			if _, aerr := proxy.ApplyListeners(ep.API, ep.Secret, dir, nil); aerr != nil {
				log.Printf("代理：清理 Clash 配置失败（%v），已关闭代理但配置里可能留有自动块", aerr)
				cleaned = "（清理 Clash 配置失败，配置里可能留有自动块）"
			}
		}
	}
	a.proxyReg = nil
	a.proxyLevel = proxy.LevelDisabled
	a.refreshProxyBindings() // 清空列表（否则旧行残留，看起来还开着）
	return "已关闭：立即回落直连" + cleaned, nil
}

// doToggleProxy 一键开关（GUI 按钮入口）。
func (a *app) doToggleProxy() {
	if a.proxyBusy {
		a.lblProxyHint.SetText("操作进行中，请稍候…")
		return
	}
	a.proxyBusy = true
	wasOn := a.svc.Upstream() != nil && a.svc.Upstream().ProxySelectorEnabled()
	a.lblProxyHint.SetText("正在处理…")

	go func() {
		var msg string
		var err error
		if wasOn {
			msg, err = a.disableProxyNow()
		} else {
			msg, err = a.enableProxyNow()
		}
		a.mw.Synchronize(func() {
			a.proxyBusy = false
			if err != nil {
				a.lblProxyHint.SetText("操作失败：" + firstLine(err.Error()))
				return
			}
			a.lblProxyHint.SetText(msg)
			a.refreshProxyBindings()
			a.refreshProxyToggle()
			log.Printf("代理开关：%s", msg)
		})
	}()
}

// refreshProxyToggle 同步开关按钮文案与状态标签。
func (a *app) refreshProxyToggle() {
	on := a.svc.Upstream() != nil && a.svc.Upstream().ProxySelectorEnabled()
	if a.btnProxyToggle != nil {
		if on {
			a.btnProxyToggle.SetText("关闭代理")
		} else {
			a.btnProxyToggle.SetText("开启代理")
		}
	}
	if a.lblProxyState != nil {
		switch {
		case on && a.proxyLevel == proxy.LevelRespect:
			// 如实告知：这次没有改用户的 Clash 配置
			a.lblProxyState.SetText("● 已开启（每个账号走独立出口 IP · 沿用你已配置的 listeners）")
		case on:
			a.lblProxyState.SetText("● 已开启（每个账号走独立出口 IP）")
		default:
			a.lblProxyState.SetText("○ 未开启（全部直连）")
		}
	}
}
