// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenStoreCreatesDataDir 数据目录不存在时必须自己建（v0.31.64）。
//
// 现场是怎么发现的：`scripts/smoke.py` 在空临时目录里启动**构建产物**，
// 立刻报 `migrate: unable to open database file (14)` —— SQLite 不会替你创建父目录，
// 而"先 mkdir 再启动"只写在部署文档里。新用户第一次运行就撞上，
// 且报错文本完全指不到"目录不存在"这个真因。
func TestOpenStoreCreatesDataDir(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fresh", "nested", "webapp.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("数据目录不存在时应自动创建，得 %v", err)
	}
	defer store.Close()
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("应已创建数据库文件：%v", err)
	}
	// 迁移真的跑过了：kv 是最早建的一批表，能读说明建表没被目录问题带偏。
	if _, err := store.GetKV("update_check_enabled"); err != nil {
		t.Fatalf("迁移应已建表，读 kv 却失败：%v", err)
	}
}
