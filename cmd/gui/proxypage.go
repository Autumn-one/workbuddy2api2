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
	"strings"

	"github.com/lxn/walk"

	"workbuddy2api/internal/appconfig"
	"workbuddy2api/internal/proxy"
	"workbuddy2api/internal/upstream"
)

// setupProxy 按配置装配代理：拉节点 → 建分配表 → 恢复绑定 → 启动健康探测。
// 返回 (registry, cancel)；未启用或拉取失败时返回 (nil, nil)（回落直连）。
func (a *app) setupProxy(cfg *appconfig.Config) (*proxy.Registry, context.CancelFunc) {
	if cfg == nil || !cfg.Proxy.Enabled {
		return nil, nil
	}
	nodes, err := proxy.FetchNodes(cfg.Proxy.ClashAPI, cfg.Proxy.ClashSecret)
	if err != nil {
		logf("代理：读取 Clash 节点失败（%v），本次回落直连", err)
		return nil, nil
	}
	ls := proxy.BuildListeners(nodes, cfg.Proxy.PortBase)
	if len(ls) == 0 {
		logf("代理：未生成任何 listener，回落直连")
		return nil, nil
	}
	reg := proxy.NewRegistry(ls)
	reg.LoadBindings(cfg.Proxy.BindingsFile)

	ctx, cancel := context.WithCancel(context.Background())
	go reg.HealthLoop(ctx, cfg.HealthIntervalDur)
	logf("代理：已启用，%d 个节点（%s），健康探测每 %s",
		len(ls), regionSummary(ls), cfg.HealthIntervalDur)
	return reg, cancel
}

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
		logf("代理：账号 %s 切换节点 %s → %s（端口 %d）",
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

// doGenListeners 生成 listeners 配置文本（供用户粘贴到 Clash 的 Merge 覆写文件）。
func (a *app) doGenListeners() {
	if a.proxyReg == nil {
		a.lblProxyHint.SetText("代理未启用：请在 config.json 里设置 proxy.enabled=true 后重启")
		return
	}
	ls := a.proxyReg.Listeners()
	yaml := proxy.RenderListenersYAML(ls)
	a.lastListenersYAML = yaml
	if a.teProxyCfg != nil {
		// 只显示前几行（Label 不适合放长文本），完整内容用「复制配置」取。
		lines := strings.Split(strings.TrimRight(yaml, "\n"), "\n")
		head := lines
		if len(head) > 14 {
			head = head[:14]
		}
		a.teProxyCfg.SetText("已生成 " + itoa(int64(len(ls))) + " 条 listener 配置（点「复制配置」取完整内容）:\r\n" +
			strings.Join(head, "\r\n"))
	}
	a.lblProxyHint.SetText(fmt.Sprintf("已生成 %d 条 listener（端口 %d~%d）·%s",
		len(ls), ls[0].Port, ls[len(ls)-1].Port, regionSummary(ls)))
}

// doCopyListeners 复制 listeners 配置到剪贴板。
func (a *app) doCopyListeners() {
	if a.lastListenersYAML == "" {
		a.doGenListeners()
	}
	if a.lastListenersYAML == "" {
		return
	}
	if err := walk.Clipboard().SetText(a.lastListenersYAML); err != nil {
		a.lblProxyHint.SetText("复制失败：" + firstLine(err.Error()))
		return
	}
	a.lblProxyHint.SetText("配置已复制。粘到 Clash 的 Merge 覆写文件（profiles/Merge.yaml）的顶层，然后重启 Clash。")
}
