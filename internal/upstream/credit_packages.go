// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package upstream

import (
	"time"

	"workbuddy2api/internal/auth"
)

// CreditPackage 一个积分套餐（get-user-resource 响应里的一行）。
//
// 为什么要逐包透出：上游把可用积分按「套餐/奖励包」分批下发，每包有各自的
// 剩余/总量与到期时刻（CycleEndTime）。运维面板若只显示一个聚合总额，
// 就看不出「哪一包 / 哪个包快过期」——而这恰恰是白扔积分的主因。
// 逐包口径与 ResourceSummary 的聚合口径同源（都用 packageRemainUsed），
// 保证「总额 == 各包之和」，不会出现两块数字互相打架。
type CreditPackage struct {
	Name     string `json:"name"`
	Remain   int64  `json:"remain"`
	Used     int64  `json:"used"`
	Size     int64  `json:"size"`
	ExpireAt int64  `json:"expire_at"` // Unix 秒；0 = 无到期（长期有效）
}

// expiringSoonWindow 判定「近期到期」的窗口，与卡片上的橙色告警一致。
const expiringSoonWindow = 7 * 24 * time.Hour

// ResourcePackages 查询账号的逐套餐积分明细。
//
// 返回：packs=逐包明细（按到期升序，无到期的排最后）、remain/used/size=聚合总额、
// expiringSoon=7 天内到期且仍有剩余的积分总量（供卡片高亮）。
// 聚合口径与 ResourceSummary 逐字一致，只是额外保留每包一行。
func (c *Client) ResourcePackages(a *auth.Auth) (packs []CreditPackage, remain, used, size, expiringSoon int64, err error) {
	resp, err := c.getUserResourceBody(a)
	if err != nil {
		return nil, 0, 0, 0, 0, err
	}
	now := time.Now()
	for _, acct := range resp.Response.Data.Accounts {
		r, u, s := packageRemainUsed(respAccount{
			CapacityRemain:      acct.CapacityRemain,
			CapacityUsed:        acct.CapacityUsed,
			CapacitySize:        acct.CapacitySize,
			CycleCapacityRemain: acct.CycleCapacityRemain,
			CycleCapacityUsed:   acct.CycleCapacityUsed,
			CycleCapacitySize:   acct.CycleCapacitySize,
		})
		if r < 0 {
			r = 0
		}
		p := CreditPackage{Name: acct.PackageName, Remain: r, Used: u, Size: s}
		if acct.CycleEndTime != "" {
			// 上游时间为 UTC+8 墙钟（与 UserResourceDetailed 同口径）。
			if end, perr := time.ParseInLocation(packageEndLayout, acct.CycleEndTime, softRateResetLoc); perr == nil {
				p.ExpireAt = end.Unix()
				if !end.After(now.Add(expiringSoonWindow)) && r > 0 {
					expiringSoon += r
				}
			}
		}
		packs = append(packs, p)
		remain += r
		used += u
		size += s
	}
	// TotalDosage 作 size 下限（与 ResourceSummary 同口径：已消耗的不该比总剂量小）。
	if size > 0 {
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	if dosage := resp.Response.Data.TotalDosage; dosage > size {
		size = dosage
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	return packs, remain, used, size, expiringSoon, nil
}
