// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 上游通道（第三方模型 API / 中转站）的**配置面**（v0.31.68，需求 P2）。
//
// 一个"通道"= 一个 base_url + 一把 key + 一组模型。它和号池是两条独立的供给：
//   - 号池：workbuddy 账号，走指纹/出口/轮转那一整套；
//   - 通道：标准 OpenAI 兼容端点，**不做任何改写**（改成我们的指纹反而会被对方拒）。
//
// 本文件只管配置（表、校验、探活、启停、对外可见的模型清单）；
// "请求怎么转发、钱怎么算"在 relay.go —— 分开是因为改配置不该碰计费路径。
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// 通道类型。首版只有 OpenAI 兼容：中转站与绝大多数第三方都走这个协议。
// 留字段而不是直接写死，是为了 anthropic 原生的 /v1/messages 能平滑加进来。
const (
	upstreamKindOpenAI = "openai"
)

// Upstream 一条第三方/中转站通道。
type Upstream struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key,omitempty"` // 仅在写入时上行；读取时给 KeyMasked
	Enabled  bool   `json:"enabled"`
	Priority int    `json:"priority"` // 越小越先选（与"号池优先"的分层无关，只在通道之间排序）
	Note     string `json:"note"`
	// KeyMasked 读接口返回的掩码（永不回传明文）。
	//
	// 为什么不回传明文：后台是一把"能看到全部钱"的钥匙，一旦它的会话被拿走，
	// 明文 key 就是第三方账号的直接损失；而前端根本不需要明文（只有测试需要，
	// 测试走服务端探活）。
	KeyMasked string `json:"key_masked,omitempty"`

	LastProbeAt  int64  `json:"last_probe_at"`
	LastProbeOK  bool   `json:"last_probe_ok"`
	LastProbeMsg string `json:"last_probe_msg,omitempty"`

	Models []UpstreamModel `json:"models,omitempty"`

	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// UpstreamModel 一个通道下的模型映射。
type UpstreamModel struct {
	ID          int64   `json:"id"`
	UpstreamID  int64   `json:"upstream_id"`
	Model       string  `json:"model"`         // 用户可见的模型名（客户端请求里写的）
	RemoteModel string  `json:"remote_model"`  // 转发给上游时替换成的名字；空 = 同名
	CreditPer1k float64 `json:"credit_per_1k"` // 每 1000 token 折算多少**积分**（平台的计价单位）
	Enabled     bool    `json:"enabled"`
	// VerifiedAt 上次探活确认该模型存在（0 = 未确认）。仅提示，不作路由依据：
	// 探活是"某个时刻对方的目录"，而对方随时可能下线一个模型。
	VerifiedAt int64 `json:"verified_at"`
	CreatedAt  int64 `json:"created_at"`
	UpdatedAt  int64 `json:"updated_at"`
}

// UpstreamModelView 只读视图（给 /v1/models 合并与路由用，不含 key）。
type UpstreamModelView struct {
	UpstreamID   int64
	UpstreamName string
	BaseURL      string
	APIKey       string
	Model        string
	RemoteModel  string
	CreditPer1k  float64
	Priority     int
}

const migrationUpstreams = `CREATE TABLE IF NOT EXISTS upstreams (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	name        TEXT NOT NULL DEFAULT '',
	kind        TEXT NOT NULL DEFAULT 'openai',
	base_url    TEXT NOT NULL DEFAULT '',
	api_key     TEXT NOT NULL DEFAULT '',
	enabled     INTEGER NOT NULL DEFAULT 1,
	priority    INTEGER NOT NULL DEFAULT 100,
	note        TEXT NOT NULL DEFAULT '',
	last_probe_at  INTEGER NOT NULL DEFAULT 0,
	last_probe_ok  INTEGER NOT NULL DEFAULT 0,
	last_probe_msg TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL DEFAULT 0,
	updated_at  INTEGER NOT NULL DEFAULT 0
)`

const migrationUpstreamModels = `CREATE TABLE IF NOT EXISTS upstream_models (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	upstream_id   INTEGER NOT NULL DEFAULT 0,
	model         TEXT NOT NULL DEFAULT '',
	remote_model  TEXT NOT NULL DEFAULT '',
	credit_per_1k REAL NOT NULL DEFAULT 0,
	enabled       INTEGER NOT NULL DEFAULT 1,
	verified_at   INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL DEFAULT 0,
	updated_at    INTEGER NOT NULL DEFAULT 0
)`

func (s *Store) migrateUpstreams() error {
	for _, st := range []string{
		migrationUpstreams,
		migrationUpstreamModels,
		// 一个通道下同一个用户可见模型只能有一条：重复会让"选哪条"变成随机。
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_upstream_models_uniq ON upstream_models(upstream_id, model)`,
		`CREATE INDEX IF NOT EXISTS idx_upstream_models_model ON upstream_models(model)`,
		// 通道名唯一：否则后台会出现两条同名通道，排障时"我改的是哪条"无从回答。
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_upstreams_name ON upstreams(name)`,
	} {
		if _, err := s.db.Exec(st); err != nil {
			return fmt.Errorf("migrate upstreams: %w", err)
		}
	}
	return nil
}

// ------------------------------------------------------------------ 校验

// errUpstreamURL 通道地址不合法（含 SSRF 拦截）。错误文本要能直接给运维看。
var errUpstreamURL = errors.New("通道地址不合法")

// validateUpstreamURL 校验 base_url 并归一化（去尾斜杠）。
//
// **为什么必须拦私网**：这个地址是**服务端**去请求的。允许 http://127.0.0.1:8780
// 或者 http://169.254.169.254/ 就等于把"内网探测 / 云元数据"做成了一个后台功能 ——
// 而后台账号被拿走时，这是最常见的第一跳。这不是理论风险：任何"用户可填 URL、
// 服务器去请求"的功能都必须先过这一关。
func validateUpstreamURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("%w：地址不能为空", errUpstreamURL)
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("%w：%v", errUpstreamURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%w：只支持 http/https", errUpstreamURL)
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("%w：缺少主机名", errUpstreamURL)
	}
	// 主机名直判（IP 字面量之外的形态，如 localhost）。
	switch strings.ToLower(host) {
	case "localhost", "localhost.localdomain", "ip6-localhost":
		return "", fmt.Errorf("%w：不允许指向本机", errUpstreamURL)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return "", fmt.Errorf("%w：不允许指向内网/回环地址（%s）", errUpstreamURL, host)
		}
		// 云元数据服务（169.254.169.254 已被 LinkLocal 覆盖，这里补常见别名）。
		if host == "metadata.google.internal" {
			return "", fmt.Errorf("%w：不允许指向云元数据服务", errUpstreamURL)
		}
	}
	// 归一化：去尾部斜杠，保留路径前缀（有的中转站把 base 放在 /v1/openai）。
	out := strings.TrimRight(s, "/")
	return out, nil
}

// maskUpstreamKey 只露头尾，够人确认"是不是这把"，不够拿走去用。
// （不叫 maskKey：本包已有同名的 Key 列表掩码，语义不同，撞名会让两边都改错。）
func maskUpstreamKey(k string) string {
	k = strings.TrimSpace(k)
	if k == "" {
		return ""
	}
	if len(k) <= 10 {
		return strings.Repeat("*", len(k))
	}
	return k[:4] + strings.Repeat("*", 6) + k[len(k)-4:]
}

// ------------------------------------------------------------------ 读写

// ListUpstreams 全部通道（含模型），供后台与路由共用。
//
// 路由侧**不要**直接用它：那会把 api_key 明文带进热路径的每次调用里。
// 路由用 relayCandidates（只挑要用的字段）。
func (s *Store) ListUpstreams() ([]Upstream, error) {
	rows, err := s.db.Query(`SELECT id, name, kind, base_url, api_key, enabled, priority, note,
		last_probe_at, last_probe_ok, last_probe_msg, created_at, updated_at
		FROM upstreams ORDER BY priority, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Upstream
	for rows.Next() {
		var u Upstream
		var en, ok int
		var key string
		if err := rows.Scan(&u.ID, &u.Name, &u.Kind, &u.BaseURL, &key, &en, &u.Priority, &u.Note,
			&u.LastProbeAt, &ok, &u.LastProbeMsg, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		u.APIKey, u.KeyMasked, u.Enabled, u.LastProbeOK = key, maskUpstreamKey(key), en != 0, ok != 0
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 模型一次性全取，避免 N+1（通道数量本来就不多，但 N+1 是习惯问题）。
	all, err := s.listUpstreamModels(0)
	if err != nil {
		return nil, err
	}
	byID := map[int64][]UpstreamModel{}
	for _, m := range all {
		byID[m.UpstreamID] = append(byID[m.UpstreamID], m)
	}
	for i := range out {
		out[i].Models = byID[out[i].ID]
	}
	return out, nil
}

func (s *Store) listUpstreamModels(upstreamID int64) ([]UpstreamModel, error) {
	q := `SELECT id, upstream_id, model, remote_model, credit_per_1k, enabled, verified_at,
		created_at, updated_at FROM upstream_models`
	args := []any{}
	if upstreamID > 0 {
		q += ` WHERE upstream_id = ?`
		args = append(args, upstreamID)
	}
	q += ` ORDER BY model`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UpstreamModel
	for rows.Next() {
		var m UpstreamModel
		var en int
		if err := rows.Scan(&m.ID, &m.UpstreamID, &m.Model, &m.RemoteModel, &m.CreditPer1k,
			&en, &m.VerifiedAt, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		m.Enabled = en != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// SaveUpstream 新增或更新一条通道。id=0 表示新增。
//
// name / base_url 都唯一：重名会让"我改的是哪条"无法回答；同地址两条通道
// 也让"是不是重复配置"无从判断（真要两条就改个名字，但至少人得看见）。
func (s *Store) SaveUpstream(u *Upstream) (int64, error) {
	if strings.TrimSpace(u.Name) == "" {
		return 0, errors.New("通道名不能为空")
	}
	base, err := validateUpstreamURL(u.BaseURL)
	if err != nil {
		return 0, err
	}
	u.BaseURL = base
	if u.Kind == "" {
		u.Kind = upstreamKindOpenAI
	}
	if u.Kind != upstreamKindOpenAI {
		return 0, fmt.Errorf("暂不支持 kind=%q（当前只有 openai 兼容协议）", u.Kind)
	}
	now := time.Now().Unix()
	en := 0
	if u.Enabled {
		en = 1
	}
	if u.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO upstreams
			(name, kind, base_url, api_key, enabled, priority, note, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			u.Name, u.Kind, u.BaseURL, u.APIKey, en, u.Priority, u.Note, now, now)
		if err != nil {
			if isUniqueViolation(err) {
				return 0, errors.New("通道名已存在，请换一个名字")
			}
			return 0, err
		}
		id, _ := res.LastInsertId()
		return id, nil
	}
	// 更新时 api_key 为空 = **不改**（前端拿不到明文，也不可能回传它）。
	if strings.TrimSpace(u.APIKey) == "" {
		_, err = s.db.Exec(`UPDATE upstreams SET name=?, base_url=?, enabled=?, priority=?, note=?, updated_at=?
			WHERE id=?`, u.Name, u.BaseURL, en, u.Priority, u.Note, now, u.ID)
	} else {
		_, err = s.db.Exec(`UPDATE upstreams SET name=?, base_url=?, api_key=?, enabled=?, priority=?, note=?, updated_at=?
			WHERE id=?`, u.Name, u.BaseURL, u.APIKey, en, u.Priority, u.Note, now, u.ID)
	}
	if err != nil {
		if isUniqueViolation(err) {
			return 0, errors.New("通道名已存在，请换一个名字")
		}
		return 0, err
	}
	return u.ID, nil
}

// DeleteUpstream 删通道，同时删它的模型映射。
//
// 必须一起删：留下孤儿映射会让"这个模型从哪来的"永远查不清，而
// relayCandidates 会因为 JOIN 不到通道而静默跳过 —— 看起来正常，实则脏数据。
func (s *Store) DeleteUpstream(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM upstream_models WHERE upstream_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM upstreams WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// SaveUpstreamModel 新增/更新一条模型映射。
func (s *Store) SaveUpstreamModel(m *UpstreamModel) (int64, error) {
	m.Model = strings.TrimSpace(m.Model)
	if m.Model == "" {
		return 0, errors.New("模型名不能为空")
	}
	if m.UpstreamID <= 0 {
		return 0, errors.New("必须先选一条通道")
	}
	if m.CreditPer1k < 0 || m.CreditPer1k > 100000 {
		return 0, errors.New("每 1k token 的积分必须在 0–100000 之间")
	}
	// 上游模型名为空 = 与用户可见名相同（绝大多数中转站就是这样）。
	m.RemoteModel = strings.TrimSpace(m.RemoteModel)
	now := time.Now().Unix()
	en := 0
	if m.Enabled {
		en = 1
	}
	if m.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO upstream_models
			(upstream_id, model, remote_model, credit_per_1k, enabled, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?)`,
			m.UpstreamID, m.Model, m.RemoteModel, m.CreditPer1k, en, now, now)
		if err != nil {
			if isUniqueViolation(err) {
				return 0, fmt.Errorf("该通道下已经有模型 %q", m.Model)
			}
			return 0, err
		}
		return res.LastInsertId()
	}
	_, err := s.db.Exec(`UPDATE upstream_models SET model=?, remote_model=?, credit_per_1k=?, enabled=?, updated_at=?
		WHERE id=?`, m.Model, m.RemoteModel, m.CreditPer1k, en, now, m.ID)
	return m.ID, err
}

func (s *Store) DeleteUpstreamModel(id int64) error {
	_, err := s.db.Exec(`DELETE FROM upstream_models WHERE id = ?`, id)
	return err
}

// SetUpstreamProbe 记录一次探活结果。
func (s *Store) SetUpstreamProbe(id int64, ok bool, msg string) error {
	o := 0
	if ok {
		o = 1
	}
	_, err := s.db.Exec(`UPDATE upstreams SET last_probe_at=?, last_probe_ok=?, last_probe_msg=? WHERE id=?`,
		time.Now().Unix(), o, msg, id)
	return err
}

// isUniqueViolation SQLite 唯一约束冲突（modernc 驱动的错误文本）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "unique constraint") || strings.Contains(s, "constraint failed")
}

// ------------------------------------------------------------------ 路由视图

// relayCandidates 某模型可用的通道（已启用的通道 + 已启用的映射），按通道 priority 排序。
//
// 这是 relay 的**唯一选路入口**：分发、可见性、计费三处都调它，
// 免得出现"面板说这个模型可用、请求却说没有"这类两套口径。
func (s *Store) relayCandidates(model string) ([]UpstreamModelView, error) {
	rows, err := s.db.Query(`SELECT u.id, u.name, u.base_url, u.api_key,
		m.model, COALESCE(NULLIF(m.remote_model,''), m.model), m.credit_per_1k, u.priority
		FROM upstream_models m JOIN upstreams u ON u.id = m.upstream_id
		WHERE u.enabled = 1 AND m.enabled = 1 AND m.model = ?
		ORDER BY u.priority, u.id`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UpstreamModelView
	for rows.Next() {
		var v UpstreamModelView
		if err := rows.Scan(&v.UpstreamID, &v.UpstreamName, &v.BaseURL, &v.APIKey,
			&v.Model, &v.RemoteModel, &v.CreditPer1k, &v.Priority); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// relayModelIDs 所有"可通过通道提供"的模型名（去重、升序）。
func (s *Store) relayModelIDs() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT m.model
		FROM upstream_models m JOIN upstreams u ON u.id = m.upstream_id
		WHERE u.enabled = 1 AND m.enabled = 1 ORDER BY m.model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// relayModelSet 同上，但返回 set（合并 /v1/models 时要按名字判重）。
func (s *Store) relayModelSet() map[string]bool {
	ids, err := s.relayModelIDs()
	if err != nil {
		log.Printf("WARN: [relay] 读取通道模型失败：%v", err)
		return nil
	}
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// ------------------------------------------------------------------ 探活

// probeUpstream 用一次 GET {base}/models 验证：地址通、key 有效、拿到模型清单。
//
// 为什么探 /models 而不是发一次 chat：/models 免费、快、不消耗对方额度，
// 且能顺带拿到对方真实的模型名 —— 这正是"填映射"时最需要的信息。
// 而 chat 探活会真花钱，且被限流时会误判成"配置错"。
func probeUpstream(ctx context.Context, u Upstream) ([]string, error) {
	if strings.TrimSpace(u.APIKey) == "" {
		return nil, errors.New("通道没有配置 API Key")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(u.BaseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+u.APIKey)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if resp.StatusCode >= 400 {
		// 401/403 是确定结论（key 错），不要去重试；文案要能直接指向问题。
		return nil, fmt.Errorf("上游返回 %d：%s", resp.StatusCode, strings.TrimSpace(truncateStr(string(body), 200)))
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("响应不是 OpenAI 兼容的 /models 结构：%v", err)
	}
	var ids []string
	for _, d := range parsed.Data {
		if s := strings.TrimSpace(d.ID); s != "" {
			ids = append(ids, s)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return nil, errors.New("/models 返回了空清单（检查 key 权限或该中转站是否屏蔽了该端点）")
	}
	return ids, nil
}

// truncateStr 截断用于错误文案（不截断会把整段 HTML 错误页塞进日志）。
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ------------------------------------------------------------------ 管理接口

func (a *app) handleAdminUpstreams(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListUpstreams()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 顺手把"号池同名的模型"标出来：分发规则是号池优先，运营需要看见
	// "我配的这个模型其实永远走不到通道"（否则会以为通道坏了）。
	//
	// **必须与 poolKnowsModel 同源**（cachedModelIDs → ObservedModels）：
	// 用 knownModelIDs 会把内置兜底清单也算成"号池认识"，于是面板标"号池优先"
	// 而请求实际走了通道 —— 两个口径不一致比缺这个提示更糟。
	poolSet := map[string]bool{}
	ids := cachedModelIDs()
	if len(ids) == 0 {
		ids = a.proxy.pool.ObservedModels()
	}
	for _, id := range ids {
		poolSet[id] = true
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"upstreams":   list,
		"pool_models": poolSet, // 前端据此提示"该模型号池也有，将优先走号池"
		"count":       len(list),
	})
}

func (a *app) handleAdminUpstreamSave(w http.ResponseWriter, r *http.Request) {
	var u Upstream
	if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	id, err := a.store.SaveUpstream(&u)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errUpstreamURL) {
			code = http.StatusBadRequest
		}
		writeJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("INFO: [relay] 保存通道 id=%d name=%q base=%s enabled=%v", id, u.Name, u.BaseURL, u.Enabled)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "ok": true})
}

func (a *app) handleAdminUpstreamDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 不能为空"})
		return
	}
	if err := a.store.DeleteUpstream(req.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "通道不存在"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("WARN: [relay] 删除通道 id=%d（其模型映射一并删除）", req.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *app) handleAdminUpstreamToggle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID      int64 `json:"id"`
		Enabled bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 不能为空"})
		return
	}
	en := 0
	if req.Enabled {
		en = 1
	}
	if _, err := a.store.db.Exec(`UPDATE upstreams SET enabled=?, updated_at=? WHERE id=?`,
		en, time.Now().Unix(), req.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 停用即刻从 /v1/models 消失（缓存最多 5 分钟）→ 主动作废缓存，避免"关了还能看见"。
	invalidateModelsCache()
	log.Printf("INFO: [relay] 通道 id=%d enabled=%v", req.ID, req.Enabled)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *app) handleAdminUpstreamProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 不能为空"})
		return
	}
	list, err := a.store.ListUpstreams()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	var target *Upstream
	for i := range list {
		if list[i].ID == req.ID {
			target = &list[i]
			break
		}
	}
	if target == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "通道不存在"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	ids, perr := probeUpstream(ctx, *target)
	msg := ""
	if perr != nil {
		msg = perr.Error()
	} else {
		msg = fmt.Sprintf("可用模型 %d 个", len(ids))
	}
	// 探活结果落库：后台列表要能显示"上次探活什么时候、结果如何"，
	// 否则运维每次都要手动点一遍才知道通道还活着。
	if err := a.store.SetUpstreamProbe(req.ID, perr == nil, msg); err != nil {
		log.Printf("WARN: [relay] 保存探活结果失败：%v", err)
	}
	if perr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": perr.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": ids, "count": len(ids)})
}

func (a *app) handleAdminUpstreamModelSave(w http.ResponseWriter, r *http.Request) {
	var m UpstreamModel
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	id, err := a.store.SaveUpstreamModel(&m)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	invalidateModelsCache()
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "ok": true})
}

func (a *app) handleAdminUpstreamModelDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 不能为空"})
		return
	}
	if err := a.store.DeleteUpstreamModel(req.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	invalidateModelsCache()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// invalidateModelsCache 让 /v1/models 立刻重建（配置变更后必须调）。
//
// 不调的话：停用通道后最长 5 分钟客户端还能看到那个模型，选了就 404 ——
// 这是"面板说关了、实际还能看见"的典型现场，且看起来像 bug。
func invalidateModelsCache() {
	modelsCache.mu.Lock()
	modelsCache.at = time.Time{}
	modelsCache.payload = nil
	modelsCache.ids = nil
	modelsCache.relayIDs = nil
	modelsCache.mu.Unlock()
}

// relayVisibleModels 通道提供的、且号池**没有**的模型（合并 /v1/models 时用）。
//
// 号池优先：同名模型只出现在号池那一条里（owned_by=workbuddy），
// 免得客户端看到两个同名条目（不同 owned_by）而不知道走哪条。
func (a *app) relayVisibleModels(poolIDs []string) []string {
	set := a.store.relayModelSet()
	if len(set) == 0 {
		return nil
	}
	have := make(map[string]bool, len(poolIDs))
	for _, id := range poolIDs {
		have[id] = true
	}
	out := make([]string, 0, len(set))
	for m := range set {
		if !have[m] {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}
