// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// mounted.go 账号「已挂载时长」的**唯一计算口径**（v0.26.7）。
//
// 为什么单开一个文件：这个数字会同时出现在**管理后台账号卡**与**用户控制台的
// 代挂账号列表**两处，而"多久算一天"是最容易两处算不一致的一类逻辑
// （一个用 Truncate(24h)、一个用自然日，跨零点时就会差一天）。
// 更要紧的是边界：`created_at = 0`（历史数据/导入异常）与"未来时间"（时钟偏移）
// 都必须有确定行为 —— 否则界面会显示"已挂载 20500 天"这种荒唐数字。
//
// 口径：**从账号首次接入本系统（入库时刻 created_at）起算的自然日数**。
// 不按"入池成功时刻"算：pool_state 会随禁用/退池反复变化，而"这个号是什么时候
// 挂进来的"是稳定事实；号主关心的是"我挂了多久"，不是"它此刻在池里的第几天"。
package main

import (
	"strconv"
	"time"
)

// mountedInfo 已挂载时长的展示三元组。
type mountedInfo struct {
	// At 挂载时刻（unix 秒）。0 = 未知。
	At int64
	// Days 已挂载的**整天数**（自然日差，向下取整）。仅在 Known 为真时有意义。
	Days int
	// Text 现成文案："" 表示未知；"今天挂载"；"已挂载 N 天"。
	//
	// 后端给文案而不是只给数字：两个界面各自拼一遍必然出现
	// "0 天" / "1 天" / "今天挂载" 三种写法混用。
	Text string
	// Known 是否有可信的挂载时刻。false 时前端应显示"—"而不是 0 天 ——
	// "不知道挂了多久"与"刚挂上"是两件事。
	Known bool
}

// mountedOf 由挂载时刻算出展示三元组（纯函数，便于直接钉进单测）。
//
// 边界规则（都有用例）：
//   - createdAt <= 0 → Known=false（历史行/异常导入，不猜）
//   - createdAt 在未来（本机时钟回拨 / 跨机导入）→ 视为"今天挂载"，
//     天数钳到 0。**不显示负数**：那会让界面出现"已挂载 -3 天"。
//   - 天数按**自然日差**而不是 86400 秒整除：今天 23:59 挂上、明天 00:01 查看，
//     按秒算是 0 天、按自然日算也应是 0 天（跨了日但不足一天）；
//     而"昨天 09:00 挂上、今天 10:00 查看"按自然日算是 1 天，
//     比按秒算（1.04 天 → 1 天）更符合用户"挂了一天了"的直觉。
func mountedOf(createdAt int64, now time.Time) mountedInfo {
	if createdAt <= 0 {
		return mountedInfo{}
	}
	at := time.Unix(createdAt, 0).In(now.Location())
	if at.After(now) {
		// 时钟偏移：当作今天挂载，不产生负数天数。
		return mountedInfo{At: createdAt, Days: 0, Text: "今天挂载", Known: true}
	}
	// 按本地自然日取整（跨夏令时也稳：用日期而非秒数相减）。
	y1, m1, d1 := at.Date()
	y2, m2, d2 := now.Date()
	day1 := time.Date(y1, m1, d1, 0, 0, 0, 0, now.Location())
	day2 := time.Date(y2, m2, d2, 0, 0, 0, 0, now.Location())
	days := int(day2.Sub(day1).Hours() / 24)
	if days < 0 {
		days = 0
	}
	// 必须用 strconv.Itoa：本包的 itoa(n)（tasks.go）在 n<=0 时硬编码返回 "3"
	// —— 那是给「连续失败次数」用的默认值。拿它拼天数会让 days=0 显示
	// "已挂载 3 天"（本轮写第一版时确实这么踩了，测试抓到的）。
	text := "已挂载 " + strconv.Itoa(days) + " 天"
	if days == 0 {
		text = "今天挂载"
	}
	return mountedInfo{At: createdAt, Days: days, Text: text, Known: true}
}
