// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// cost_explore_config_test.go pool.cost_explore_interval 配置测试（issue #136）：
// 默认 30m / 文件覆盖 / "0" 关停 / 空值回落默认 / 非法值报错。
package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCostExploreIntervalDefault 键缺席 → 默认 30m（探索缺省开启）。
func TestCostExploreIntervalDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.CostExploreIntervalDur != 30*time.Minute {
		t.Errorf("cost_explore_interval=%v want 30m (default)", c.CostExploreIntervalDur)
	}
}

// TestCostExploreIntervalParsedFromFile 显式配置覆盖默认。
func TestCostExploreIntervalParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"cost_explore_interval":"45m"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.CostExploreIntervalDur != 45*time.Minute {
		t.Errorf("cost_explore_interval=%v want 45m", c.CostExploreIntervalDur)
	}
}

// TestCostExploreIntervalZeroDisables "0" = 关停（完全回到现状行为）。
func TestCostExploreIntervalZeroDisables(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"cost_explore_interval":"0"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.CostExploreIntervalDur != 0 {
		t.Errorf("cost_explore_interval=%v want 0 (关停)", c.CostExploreIntervalDur)
	}
}

// TestCostExploreIntervalEmptyFallsBackToDefault 显式空串回落默认 30m。
func TestCostExploreIntervalEmptyFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"cost_explore_interval":""}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.CostExploreIntervalDur != 30*time.Minute {
		t.Errorf("cost_explore_interval=%v want 30m fallback", c.CostExploreIntervalDur)
	}
}

// TestBadCostExploreInterval 非法值 fail fast（不静默回落，风格同 soft_rate_max）。
func TestBadCostExploreInterval(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"cost_explore_interval":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad cost_explore_interval")
	}
}
