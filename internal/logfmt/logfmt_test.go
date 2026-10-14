// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package logfmt

import "testing"

func TestUID8(t *testing.T) {
	tests := []struct {
		name string
		uid  string
		want string
	}{
		{"full uuid truncated to 8", "5895a56c-449f-49e7-b723-8ddf10a97cdb", "5895a56c"},
		{"exactly 8 chars unchanged", "00e26541", "00e26541"},
		{"short uid unchanged", "abc", "abc"},
		{"empty uid returns dash", "", "-"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange — tc.uid
			// Act
			got := UID8(tc.uid)
			// Assert
			if got != tc.want {
				t.Errorf("UID8(%q) = %q, want %q", tc.uid, got, tc.want)
			}
		})
	}
}
