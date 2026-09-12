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
		log.Printf("代理：读取 Clash 节点失败（%v），本次回落直连", err)
		return nil, nil
	}
	ls := proxy.BuildListeners(nodes, cfg.Proxy.PortBase)
	if len(ls) == 0 {
		log.Printf("代理：未生成任何 listener，回落直连")
		return nil, nil
	}
	reg := proxy.NewRegistry(ls)
	reg.LoadBindings(cfg.Proxy.BindingsFile)

	ctx, cancel := context.WithCancel(context.Background())
	go reg.HealthLoop(ctx, cfg.HealthIntervalDur)
	log.Printf("代理：已启用，%d 个节点（%s），健康探测每 %s",
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

// autoApplyProxyAtStartup 启动时自动把 listeners 应用到 Clash（用户要求：不要手工粘贴）。
//
// 幂等且安全：注入前先移除上次的自动块，原有配置一字不改；
// 任何一步失败都只记日志并继续（不阻塞启动，也不留半成品——ApplyListeners 内部会回滚）。
func (a *app) autoApplyProxyAtStartup() {
	if a.proxyReg == nil || a.cfg == nil || a.cfg.Proxy.ClashAPI == "" {
		return
	}
	dir := vergeDataDir()
	if dir == "" {
		log.Printf("代理：未找到 Clash Verge 数据目录，跳过自动应用（可手工点「一键应用到 Clash」）")
		return
	}
	ls := a.proxyReg.Listeners()
	cfgPath, err := proxy.ApplyListeners(a.cfg.Proxy.ClashAPI, a.cfg.Proxy.ClashSecret, dir, ls)
	if err != nil {
		log.Printf("代理：自动应用失败（%v）；可点「一键应用到 Clash」重试或用「复制配置（兜底）」手工粘贴", err)
		return
	}
	// 探测确认端口是否真的起来（Clash 重载是异步的，给一点时间）
	time.Sleep(1200 * time.Millisecond)
	ok := 0
	for _, l := range ls {
		if proxy.ProbePort(l.Port) {
			a.proxyReg.MarkHealthy(l.Port)
			ok++
		}
	}
	log.Printf("代理：已自动应用到 Clash（%s），%d/%d 个端口可用", filepath.Base(cfgPath), ok, len(ls))
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

// doGenListeners 生成 listeners 配置文本（自动应用失败时的兜底：供手工粘贴）。
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
	a.lblProxyHint.SetText("已复制（兜底用）。粘到 Verge 的 profiles/Merge.yaml 顶层后需重启 Clash；正常情况下点「一键应用到 Clash」即可。")
}
