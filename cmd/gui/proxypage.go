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

// autoEnableProxyAtStartup 启动阶段自动开启代理（仅当 config.proxy.enabled=true）。
//
// 与「一键开关」走完全相同的代码路径（enableProxyNow），保证行为一致——
// 避免"配置启用"与"手动开关"两套逻辑各写一遍而漂移。
func (a *app) autoEnableProxyAtStartup() {
	if a.svc.Upstream() == nil {
		return // 服务未启动，等用户点开关
	}
	msg, err := a.enableProxyNow()
	if err != nil {
		log.Printf("代理：启动自动启用失败（%v）；可在「代理」页点「一键开启代理」重试", err)
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
		a.lblProxyHint.SetText("代理未启用：请先点「一键开启代理」")
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
		a.lblProxyHint.SetText("代理未启用：请先点「一键开启代理」")
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
		a.lblProxyHint.SetText("代理未启用：请先点「一键开启代理」")
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
		a.lblProxyHint.SetText("代理未启用：请先点「一键开启代理」")
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
// → 自动应用 listeners 到 Clash → 注入选择器。全程无需改配置或重启。
//
// 这是用户要求的"简单"入口：GPU 上一个开关，点了就用。
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
	ls := proxy.BuildListeners(nodes, proxy.DefaultPortBase)
	if len(ls) == 0 {
		return "", fmt.Errorf("未取到任何节点")
	}

	// 建分配表并恢复历史绑定（IP 稳定）
	reg := proxy.NewRegistry(ls)
	bindPath := a.proxyBindingsPath
	if bindPath == "" {
		bindPath = filepath.Join(filepath.Dir(a.cfg.StateFile), "proxy-bindings.json")
	}
	reg.LoadBindings(bindPath)

	// 【关键】为全部账号立刻建立绑定：让界面一开启就显示"每个账号走哪个节点"，
	// 而不是等请求发生才懒分配（那样列表默认空白，用户以为没生效）。
	uids := make([]string, 0, len(a.svc.Accounts()))
	for _, st := range a.svc.Accounts() {
		uids = append(uids, st.UID)
	}
	reg.EnsureAllAssigned(uids)

	// 自动应用 listeners 到 Clash（运行期重载，无需重启 Clash）
	if _, err := proxy.ApplyListeners(ep.API, ep.Secret, dir, ls); err != nil {
		return "", fmt.Errorf("应用到 Clash 失败: %w", err)
	}
	time.Sleep(1200 * time.Millisecond) // 等 Clash 重载

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
	if a.proxyCancel != nil {
		a.proxyCancel() // 停掉旧的健康探测循环
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.proxyCancel = cancel
	go reg.HealthLoop(ctx, proxy.DefaultHealthInterval)

	reg.SaveBindings(bindPath)
	return fmt.Sprintf("已启用：%d 个节点（%s），%d/%d 个真实可用%s",
		len(ls), regionSummary(ls), ok, len(ls), delayNote), nil
}

// disableProxyNow 在【运行期】关闭代理：立刻回落直连，不重启。
// 同时把 Clash 里的自动注入块清掉（不留垃圾配置）。
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
	// 清理 Clash 里的自动注入块（传空列表 = 只清理）
	if dir := vergeDataDir(); dir != "" {
		ep, err := proxy.DiscoverClashEndpoint(dir, "")
		if err == nil {
			if _, aerr := proxy.ApplyListeners(ep.API, ep.Secret, dir, nil); aerr != nil {
				log.Printf("代理：清理 Clash 配置失败（%v），已关闭代理但配置里可能留有自动块", aerr)
			}
		}
	}
	a.proxyReg = nil
	a.refreshProxyBindings() // 清空列表（否则旧行残留，看起来还开着）
	return "已关闭：立即回落直连（Clash 里的自动配置已清理）", nil
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
			a.btnProxyToggle.SetText("一键开启代理")
		}
	}
	if a.lblProxyState != nil {
		if on {
			a.lblProxyState.SetText("● 已开启（每个账号走独立出口 IP）")
		} else {
			a.lblProxyState.SetText("○ 未开启（全部直连）")
		}
	}
}
