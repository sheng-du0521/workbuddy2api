// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// export_test.go 测试专用 API：生产代码不暴露，仅同包 _test 可见（Go 机制）。
package session

// Resolve 返回会话 key 应绑定的账号 uid，ok=false 表示当前无可用账号。
// 无模型维度（等价于 ResolveForModel(key, "")）。原为生产导出方法，但全库
// 仅本包测试引用（生产全走 ResolveForModel），收缩到 export_test.go。
func (r *Router) Resolve(key string) (string, bool) {
	return r.ResolveForModel(key, "")
}
