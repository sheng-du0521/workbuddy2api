// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// growth_store.go 成长任务账号凭证与执行时间戳的存取（v0.17.0）。
//
// 设计取舍：accounts.growth_done_at 只记录「最近一次执行时间」（0 = 从未跑过），
// 不参与任何准入判断 —— 管理员选谁就跑谁，不需要先「重置」才能重跑。
// 之所以还留着这个时间戳：账号卡上要能一眼看出「这个号上一次跑成长任务是什么时候」，
// 这是运营判断「该不该再选它跑一次」的依据之一。
//
// 为什么不做每日去重：成长任务的行为事件在服务端**按天幂等**、claim 已领返
// already_claimed，所以「重复跑」本身无害（脚本会跳过已完成的），本地状态无需
// 承担正确性责任。这样即便标记丢失（换库/回滚），最坏结果也只是多跑一轮幂等请求。
package main

import (
	"strings"
	"time"
)

// MarkGrowthDone 记录某账号的成长任务最近执行时间（跑完一轮后调用）。
// 失败只返回错误，由调用方决定是否重试；不因标记写失败而回滚已完成的真实收益。
func (s *Store) MarkGrowthDone(uid string) error {
	_, err := s.db.Exec(`UPDATE accounts SET growth_done_at = ? WHERE uid = ?`, time.Now().Unix(), uid)
	return err
}

// AccountsByUIDs 按 uid 批量取账号，**返回顺序与入参一致**（缺失的跳过）。
//
// 顺序有语义：手动执行时按管理员的选择顺序逐个跑，便于对照日志与选择结果。
// 单条 IN 查询而非逐个 GetAccount：一次选中几十个账号时省掉几十次往返。
func (s *Store) AccountsByUIDs(uids []string) ([]*Account, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(uids)), ",")
	args := make([]any, 0, len(uids))
	for _, u := range uids {
		args = append(args, u)
	}
	rows, err := s.db.Query(`SELECT `+accountCols+` FROM accounts WHERE uid IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byUID := make(map[string]*Account, len(uids))
	for rows.Next() {
		a, err := s.scanAccount(rows)
		if err != nil {
			return nil, err
		}
		byUID[a.UID] = a
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*Account, 0, len(uids))
	for _, u := range uids {
		if a := byUID[u]; a != nil {
			out = append(out, a)
		}
	}
	return out, nil
}
