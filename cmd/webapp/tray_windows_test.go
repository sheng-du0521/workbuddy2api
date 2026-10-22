//go:build windows

// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import "testing"

// TestTrayIconBitsShape 校验托盘图标的像素。
//
// 为什么值得测：图标是**画出来的**，不是资源文件 —— 改一个参数（描边半宽、
// 圆角半径、外边距）就可能在 32px 这个尺寸上糊成一团，而托盘图标在真机上
// 只有指甲盖大，"看起来不太对"这种反馈几乎不会有人报上来。这里把三个
// 最容易坏的性质钉住。
func TestTrayIconBitsShape(t *testing.T) {
	xor, mask := trayIconBits()
	s := trayIconSize
	if len(xor) != s*s*4 {
		t.Fatalf("颜色位图长度 = %d, want %d（32bpp BGRA）", len(xor), s*s*4)
	}
	if len(mask) != s*s/8 {
		t.Fatalf("掩码长度 = %d, want %d（1bpp，32px 每行 4B）", len(mask), s*s/8)
	}
	for _, b := range mask {
		if b != 0 {
			t.Fatalf("掩码应全 0：32bpp 图标的透明由颜色位图的 alpha 决定，"+
				"非零掩码会把图标画成镂空。首个非零字节 = %#x", b)
		}
	}

	at := func(x, y int) (b, g, r, a byte) {
		i := ((s-1-y)*s + x) * 4 // DIB 自下而上
		return xor[i], xor[i+1], xor[i+2], xor[i+3]
	}

	// ① 圆角矩形之外必须透明：否则托盘里是一张**方块贴纸**，
	//    在深色任务栏上尤其明显（四角出现四个亮块）。
	for _, c := range [][2]int{{0, 0}, {1, 1}, {s - 1, 0}, {0, s - 1}, {s - 1, s - 1}} {
		if _, _, _, a := at(c[0], c[1]); a != 0 {
			t.Errorf("角点 (%d,%d) 应透明，alpha=%d", c[0], c[1], a)
		}
	}
	// ② 字之外的区域必须是底色。
	//
	// 采样点取「上边正中」与「左边正中」而不是图标正中心：正中心恰好是 "W"
	// 中间那个尖，本来就是白的 —— 拿它当底色样本会在字形正常时误报。
	for _, p := range [][2]int{{s / 2, 2}, {2, s / 2}, {s - 3, s / 2}} {
		b, g, r, a := at(p[0], p[1])
		if a != 0xFF || !(r < 0x80 && b > 0xC0) {
			t.Errorf("(%d,%d) 应是品牌蓝底（#2F6FED 的 BGRA），实得 b=%#x g=%#x r=%#x a=%#x",
				p[0], p[1], b, g, r, a)
		}
	}
	// ③ 白色字形的量要落在合理区间。
	//
	// 上限是**关键**：描边半宽调大后四条笔画会在中段连成一片，
	// "W" 直接变成一条实心横杠（实测半宽 2.6 时白像素约 1.7 倍）。
	// 下限则保证"字形还在"—— 参数改错时最容易得到的是一个纯色方块。
	var white int
	for y := 0; y < s; y++ {
		for x := 0; x < s; x++ {
			b, g, r, a := at(x, y)
			if a == 0xFF && r > 0xF0 && g > 0xF0 && b > 0xF0 {
				white++
			}
		}
	}
	t.Logf("白色字形像素数 = %d", white)
	if white < 90 || white > 280 {
		t.Errorf("白色字形像素数 = %d，超出 [90,280]：偏少说明字形没了，偏多说明笔画糊在一起了", white)
	}
}

// TestClipTextUTF16 定长 WCHAR 数组的截断必须按码元而不是按字节。
func TestClipTextUTF16(t *testing.T) {
	// 中文一个字 3 字节但只占 1 个 UTF-16 码元；按字节截会切出半个字。
	got := clipText("workbuddy2api 已启动", 12)
	if len([]rune(got)) != 12 {
		t.Fatalf("clipText 结果 = %q（%d 码元），want 12", got, len([]rune(got)))
	}
	if clipText("短", 127) != "短" {
		t.Fatalf("未超长时不应改动原串")
	}
}

// TestInRoundedRect 圆角判定的四个象限都要覆盖。
func TestInRoundedRect(t *testing.T) {
	// 半径 7 时，角落圆心在 (7,7)：到圆心距离 <=7 的点仍在图形内，
	// 所以 (3,3) 是**实心**的（距圆心 4.95），(1,1) 才被切掉（距圆心 7.78）。
	cases := []struct {
		x, y int
		want bool
	}{
		{16, 16, true},  // 正中
		{0, 0, false},   // 左上角：切掉
		{1, 1, false},   // 左上角：切掉（距圆心 7.78 > 7）
		{3, 3, true},    // 左上角圆内：仍在图形内
		{8, 1, true},    // 上边中段
		{31, 16, true},  // 右边中段
		{31, 31, false}, // 右下角：切掉
		{30, 30, false}, // 右下角：切掉
		{28, 28, true},  // 右下角圆内：仍在图形内
		{-1, 16, false}, // 左侧外
		{16, 32, false}, // 下方外
	}
	for _, c := range cases {
		if got := inRoundedRect(float64(c.x)+0.5, float64(c.y)+0.5, 0, 0, 32, 32, 7); got != c.want {
			t.Errorf("inRoundedRect(%d,%d) = %v, want %v", c.x, c.y, got, c.want)
		}
	}
}
