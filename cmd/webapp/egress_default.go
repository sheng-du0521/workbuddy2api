// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// egress_default.go 全局默认出口的护栏与修复。
//
// 为什么"全局默认出口指向谁"需要护栏：它是**唯一一条不经逐个指定就生效**的出口。
// 一旦它指向某个用户在控制台拉起的线路，后果是三件事同时发生：
//  1. 全平台所有「未单独指定出口」的账号都从这个用户的家庭宽带走
//     —— 越界使用别人的带宽（用户并不知情，页面上只写着"我的出口"）；
//  2. 平台号与该用户自己的号落在**同一个公网 IP** 上 —— 这是最强的关联信号，
//     等于把他和自己一起送进同一个风控桶；
//  3. 他一关电脑（或换宽带），平台号全线失效。
//
// 2026-10-17 的生产现场就是这个状态：`egress_proxy_default = http://127.0.0.1:21160`
// （owner_user_id=9916 的用户线路），38 个号全部从 `39.91.80.16` 出去，
// 而平台上那条 11080 的线路一个号都没在用 —— 管理员以为"配了两条线、分散了"。
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"workbuddy2api/internal/egress"
)

// egressProxyByURL 按 URL 找池内线路（找不到返回 nil）。
func (a *app) egressProxyByURL(url string) *EgressProxy {
	url = strings.TrimSpace(url)
	if url == "" {
		return nil
	}
	list, err := a.store.ListEgressProxies()
	if err != nil {
		return nil
	}
	for _, p := range list {
		if strings.TrimSpace(p.URL) == url {
			return p
		}
	}
	return nil
}

// egressProxyOwnerByURL 这条线路属于谁。found=false 表示不在池里（手填的历史出口）。
//
// 不在池里时 owner 未知，**按平台处理**（放行）：把"未知"当成"用户的"会让管理员
// 无法设置一条他自己刚手填的商业代理为默认 —— 而手填线路在池里登录后归属自然是 0。
func (a *app) egressProxyOwnerByURL(url string) (owner int64, name string, found bool) {
	p := a.egressProxyByURL(url)
	if p == nil {
		return 0, "", false
	}
	return p.OwnerUserID, p.Name, true
}

// defaultEgressGuard 校验一个 URL 能否成为全局默认出口。
//
// 返回非空字符串 = 拒绝原因（直接回给调用方，不做静默修正：静默改成别的线路
// 会让人以为"保存成功、配置就是这个"）。
func (a *app) defaultEgressGuard(url string) string {
	url = strings.TrimSpace(url)
	if url == "" {
		return "" // 空 = 直连，合法
	}
	owner, name, found := a.egressProxyOwnerByURL(url)
	if !found || owner <= 0 {
		return ""
	}
	if name == "" {
		name = url
	}
	return "全局默认出口不能指向用户线路「" + name + "」（属于用户 #" + strconv.FormatInt(owner, 10) + "）：" +
		"它会让**所有未指定出口的账号**从这位用户的家庭宽带出去，并把这些号与他的号绑在同一个 IP 上。" +
		"如果确实要让某些号走这条线路，请在线路卡上点「分配」显式指定。"
}

// handleEgressDefaultRepair POST /api/admin/egress/default/repair {"mode":"platform"|"none"}
//
// 为什么需要这个接口：护栏只能拦住**新的**写入，管不了历史配置；而"默认出口指到
// 用户线路"这种状态恰恰是历史配置造成的（2026-10-17 现场）。没有一键修复，
// 运维唯一的办法是手工去设置页翻一个可能已经不在池里的 URL —— 而设置页
// 保存时会重跑一遍全部校验，很容易连带改动别的参数。
//
// mode:
//   - platform（默认）：改回"平台自己的一条可用线路"（优先探活通过的）；
//   - none：清空 → 未指定出口的账号改为直连（从服务器机房 IP 出去）。
//
// 两种都**不会动任何账号的显式 egress_proxy**，只改全局默认。
func (a *app) handleEgressDefaultRepair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	mode := strings.TrimSpace(req.Mode)
	if mode == "" {
		mode = "platform"
	}
	if mode != "platform" && mode != "none" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "mode 仅支持 platform 或 none"})
		return
	}

	before := a.proxy.DefaultEgress()

	if mode == "none" {
		if err := a.store.SetKV(kvEgressDefault, ""); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if err := a.proxy.ApplyEgressChange(); err != nil {
			log.Printf("WARN: [egress] 清空默认出口后重建失败：%v", err)
		}
		log.Printf("WARN: [egress] 全局默认出口已清空（%q → 直连），未指定出口的账号将走服务器本机 IP", before)
		writeJSON(w, http.StatusOK, map[string]any{
			"default_egress": "", "before": before, "mode": mode,
			"note": "未指定出口的账号将直接使用服务器本机 IP（机房 IP）出站",
		})
		return
	}

	list, err := a.store.ListEgressProxies()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 候选：启用的平台线路。排序规则（可解释）：
	//   探活通过 > 有出口 IP > 名字/URL 稳定顺序。
	// 不看承载数：修默认出口是"止血"，均衡是分配那一步的事。
	cands := make([]*EgressProxy, 0, len(list))
	for _, p := range list {
		if p.Enabled && p.OwnerUserID == 0 && strings.TrimSpace(p.URL) != "" {
			cands = append(cands, p)
		}
	}
	if len(cands) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "池里没有可用的平台线路（只有用户线路或被停用）——请先接入一条平台线路，或改用 mode=none 直连",
		})
		return
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.LastOK != b.LastOK {
			return a.LastOK
		}
		ai, bi := exitIPOfProxy(a) != "", exitIPOfProxy(b) != ""
		if ai != bi {
			return ai
		}
		return a.URL < b.URL
	})
	pick := cands[0]
	if _, err := egress.Validate(pick.URL); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "选中线路地址不合法：" + err.Error()})
		return
	}
	if err := a.store.SetKV(kvEgressDefault, strings.TrimSpace(pick.URL)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := a.proxy.ApplyEgressChange(); err != nil {
		log.Printf("WARN: [egress] 修复默认出口后重建失败：%v", err)
	}
	log.Printf("INFO: [egress] 全局默认出口已修复：%q → %q（%s）", before, pick.URL, pick.Name)
	writeJSON(w, http.StatusOK, map[string]any{
		"default_egress": strings.TrimSpace(pick.URL),
		"before":         before,
		"mode":           mode,
		"proxy_id":       pick.ID,
		"name":           pick.Name,
		"exit_ip":        exitIPOfProxy(pick),
		"note":           "只改了全局默认出口，账号上显式指定的出口未被改动",
	})
}
