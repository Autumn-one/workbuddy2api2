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
	if a.tvProxyBindings == nil || a.proxyReg == nil {
		return
	}
	snap := a.proxyReg.Snapshot()
	rows := make([]proxyBindingRow, 0, len(snap))
	for _, b := range snap {
		rows = append(rows, proxyBindingRow{
			Name:    a.displayName(b.UID),
			Node:    b.Node, // 可读节点名（不是 URL）——用户明确要求
			Port:    b.Port,
			Region:  b.Region.String(),
			Healthy: a.proxyReg.Healthy(b.Port),
		})
	}
	a.proxyBindings.Replace(rows)
	if a.lblProxyHint != nil {
		ls := a.proxyReg.Listeners()
		a.lblProxyHint.SetText(fmt.Sprintf("共 %d 个节点可用（%s）· 已绑定 %d 个账号",
			len(ls), regionSummary(ls), len(rows)))
	}
}

// doSwitchAccountNode 把选中账号换到下一个健康节点（用户要求可手动切换）。
func (a *app) doSwitchAccountNode() {
	if a.proxyReg == nil {
		a.lblAccts2.SetText("代理未启用（在「配置」页打开 proxy.enabled 后重启）")
		return
	}
	items := a.svc.Accounts()
	a.takeSelection(func(idx int) {
		if idx < 0 || idx >= len(items) {
			a.lblAccts2.SetText("请先在表格里选中一个账号")
			return
		}
		uid := items[idx].UID
		before := a.proxyReg.NodeFor(uid)
		l, ok := a.proxyReg.Rebind(uid)
		if !ok {
			a.lblAccts2.SetText("没有其他健康节点可切换")
			return
		}
		a.proxyReg.SaveBindings(a.proxyBindingsPath)
		a.refreshProxyBindings()
		a.lblAccts2.SetText(fmt.Sprintf("%s 的出口节点：%s → %s",
			a.displayName(uid), orDash(before), l.Node))
		log.Printf("代理：账号 %s 切换节点 %s → %s（端口 %d）",
			a.displayName(uid), orDash(before), l.Node, l.Port)
	})
}

// doProbeProxiesNow 立即探测一轮代理健康（用户手动触发，低成本：只连本地端口）。
func (a *app) doProbeProxiesNow() {
	if a.proxyReg == nil {
		a.lblProxyHint.SetText("代理未启用")
		return
	}
	go func() {
		results := proxy.ProbeAll(a.proxyReg.Listeners())
		bad := 0
		for port, ok := range results {
			if ok {
				a.proxyReg.MarkHealthy(port)
			} else {
				a.proxyReg.MarkUnhealthy(port)
				bad++
			}
		}
		a.mw.Synchronize(func() {
			a.refreshProxyBindings()
			a.lblProxyHint.SetText(fmt.Sprintf("探测完成：%d 个可用 · %d 个不可用",
				len(results)-bad, bad))
		})
	}()
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

	// 自动应用 listeners 到 Clash（运行期重载，无需重启 Clash）
	if _, err := proxy.ApplyListeners(ep.API, ep.Secret, dir, ls); err != nil {
		return "", fmt.Errorf("应用到 Clash 失败: %w", err)
	}
	time.Sleep(1200 * time.Millisecond) // 等 Clash 重载

	// 探测端口，统计可用数
	ok := 0
	for _, l := range ls {
		if proxy.ProbePort(l.Port) {
			reg.MarkHealthy(l.Port)
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
	return fmt.Sprintf("已启用：%d 个节点（%s），%d/%d 个端口就绪",
		len(ls), regionSummary(ls), ok, len(ls)), nil
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
