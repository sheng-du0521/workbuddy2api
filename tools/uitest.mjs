// UI 冒烟测试：用 jsdom 真跑一遍 admin.html / dashboard.html 的脚本，
// 用桩 fetch 喂固定数据，逐个切换侧边栏导航，验证「无运行时异常 + 各页容器被正确填充」。
// 只做功能验证，不做布局/像素验证（那需要真浏览器）。
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { JSDOM, VirtualConsole } from 'jsdom';

// 仓库内版本（v0.24.0）：UI 目录相对**本文件**解析，不再硬编码绝对路径 ——
// 原版本住在仓库外、写死开发机的绝对路径，换机/换人即失效、也不进版本控制。
// 从 tools/ 上溯一级即仓库根。
const UI = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', 'cmd/webapp/ui');
const APP_JS = fs.readFileSync(path.join(UI, 'app.js'), 'utf8');

const now = Math.floor(Date.now() / 1000);
// series 刻意含三种分钟：空闲（req=0）/ 纯成功 / 含失败 —— 混合态才测得出
// 「失败段只画在失败的柱子上、空闲柱不长出红段」这两件事（v0.30.0）。
// fail 只在 req>0 的分钟出现 = 真接口的不变量（fail 是 req 的子集）。
const series = Array.from({ length: 60 }, (_, i) => {
  const req = i % 7;
  const fail = (i % 11 === 0 && req > 0) ? 1 : 0;
  return {
    minute: i, label: '0' + (i % 10) + ':00',
    req, ok: req - fail, fail, tokens: 100 * i, charged: 0.01 * i,
  };
});

// GROWTH_PV 一份成长任务进度样本（v0.17.1）：含未完成 / 可领 / 已完成 / 非自动四种状态，
// 便于断言「未完成排前、非自动标注、中文名」这些展示规则。
const GROWTH_PV = {
  uid: '11111111-1111-4111-8111-111111111111', updated_at: now, ok: true,
  total: 18, done: 15, auto_total: 16, auto_done: 15, ready: 1, cached: false,
  items: [
    { code: 'chat_5', name: '活跃对话上报 ×5', phase: 'zero', status: 'completed', current: 5, target: 5, done: false, ready: true, auto: true },
    { code: 'Expert_lighthouse', name: '轻量云专家', phase: 'talk', status: 'accepted', current: 0, target: 1, done: false, ready: false, auto: true },
    { code: 'black_cat', name: '夜猫子对话 ×3', phase: 'night', status: 'in_progress', current: 1, target: 3, done: false, ready: false, auto: false },
    { code: 'create_canvas', name: '设计画布', phase: 'zero', status: 'claimed', current: 1, target: 1, done: true, ready: false, auto: true },
  ],
};

const FX = {
  '/api/auth/me': { user: { id: 1, username: 'admin', is_admin: true } },
  '/api/admin/key': { key: 'testkey' },
  '/api/admin/live': {
    now, uptime: 3700, version: '0.8.0', inflight: 2,
    series,
    totals: { minutes: 60, requests: 120, ok: 115, fail: 5, tokens: 12345, charged: 1.23 },
    requests: { total: 200, ok: 190, fail: 10 },
    ratelimit: { allowed: 180, blocked: 20, rpm: 60 },
    pool: {
      cn: { total: 3, healthy: 2, cooling: 1, disabled: 0, breaker: 0, degraded: 0, inflight: 1 },
      global: { total: 1, healthy: 1, cooling: 0, disabled: 0, breaker: 0, degraded: 0, inflight: 0 },
      all: { total: 4, healthy: 3, cooling: 1, disabled: 0, breaker: 0, degraded: 0, inflight: 1 },
    },
    alerts: { active: ['min_healthy_cn'], webhook_set: false, min_healthy: 3, last_eval: now },
    events: [
      { at: now, user: 'admin-key', model: 'deepseek-v4-flash', ok: true, tokens: 12, charged: 0, elapsed_ms: 1250 },
      { at: now - 60, user: 'tester', model: 'glm-5.2', ok: false, err: 'upstream 503', status: 503, elapsed_ms: 300 },
    ],
    observed_models: 2,
  },
  '/api/admin/accounts': {
    accounts: [
      {
        // credits 刻意与缓存合计（300+500+100+334=1234）**不同**：v0.25.0 起卡片总额
        // 应取"缓存明细合计"而不是会漂移的池内快照，这个差额就是用来验证这一点的。
        uid: '11111111-1111-4111-8111-111111111111', nickname: '测试账号', realm: 'cn', credits: 4444,
        credits_known: true,
        credit_cached_remain: 1234,
        // v0.25.1：设备凭证三态。给两个账号各一种，覆盖「有/无」两个分支。
        device_token_state: 'account',
        // v0.26.4：模型级限额（6004）必须显示**具体模型**而不只是计数 ——
        // 6004 只挡那一个模型，账号对其它模型仍可用，界面要能看出这一点。
        rate_limited_models: 2,
        limited_models: [
          { model: 'glm-5.3', until: now + 3600, reset_at: now + 3600,
            reason: '6004 model rate limit [reset=body-text]' },
          { model: 'hy3-x', until: now + 1800, reason: '6004 model rate limit [reset=none→退避]' },
        ],
        // v0.24.9：积分明细缓存（服务端上次查到的结果）随列表接口一起下发。
        // 有它的账号，卡片**零出站**就能列出积分包；没有的账号仍回落到
        // 「快照值 · 点开查明细」。故意只给第一个账号，好让两个分支都被覆盖到。
        credit_packs: [
          { name: '体验版', remain: 300, used: 200, size: 500, expire_at: now + 3 * 86400 },
          { name: '拉新权益包', remain: 500, used: 0, size: 500, expire_at: now + 5 * 86400 },
          { name: '裂变包', remain: 100, used: 0, size: 100, expire_at: now + 9 * 86400 },
          { name: '长期包', remain: 334, used: 0, size: 400, expire_at: now + 40 * 86400 },
        ],
        credit_packs_at: now - 120,
        disabled: false, manual_off: false, cooling: false, cooling_for: 0, group: '反代',
        egress: 'socks5://1.2.3.4:1080', fail_count: 0, expires_at: now + 86400,
        // v0.17.2 仪表盘用：号池消耗与出口字段（生产接口一直有这两个字段）。
        served_requests: 13, consumed_credit: 0.05, charged_points: 0.05, owner_id: 0,
        // v0.26.7：已挂载时长（后端 mounted.go 统一算）。给 14 天，好让断言能看出
        // 「天数」与「起始日期」两段都渲染了。
        mounted_known: true, mounted_at: now - 14 * 86400, mounted_days: 14,
        mounted_text: '已挂载 14 天',
        task: { enabled: true, disabled: ['travel'], kinds: [] },
        // v0.13.0 消耗分组
        consume_group: 'proxy', consume_group_effective: 'proxy', consume_group_text: '反代',
        consume_group_auto: false,
      },
      {
        uid: 'abn-0000-0000-0000', nickname: '被封号', realm: 'cn', credits: 0,
        credits_known: true,
        device_token_state: 'none',
        disabled: true, manual_off: false, cooling: false, cooling_for: 0, group: '',
        // v0.26.7：**没有**可信挂载时刻（历史行）。必须整行不显示，
        // 而不是显示「已挂载 0 天」—— 那会被读成"刚挂上"。
        mounted_known: false, mounted_at: 0, mounted_days: 0, mounted_text: '',
        egress: '', fail_count: 3, expires_at: now + 3600,
        task: { enabled: true, disabled: [], kinds: [] },
        consume_group: 'abnormal', consume_group_effective: 'abnormal', consume_group_text: '异常',
        consume_group_auto: true, consume_group_note: '[auto]积分不足（无法消耗积分）',
      },
    ],
  },
  // v0.29.0：桩必须**按真接口行为**返回（字段名事故的护栏）。
  // 真接口下发的是 snake_case（id/status/created_at/alipay_account…）—— 桩若写成
  // Go 字段名或漏掉 status，前端就渲染不出审核按钮，而测试照样"通过"。
  '/api/admin/withdrawals': {
    pending_count: 1,
    status_counts: { pending: 1, approved: 1, rejected: 0 },
    // 三方一致性自检：一致时 ok=1（界面不显示任何横幅）。
    consistency: { ok: 1, frozen_micro: 500000, pending_micro: 500000, pending_count: 1,
      overdraw_users: 0, unreviewed_done: 0, frozen_pending_diff: 0 },
    params: { rate: 0.01, min_points: 100, review_mode: 'manual',
      review_mode_label: '人工审核', auto_max_points: 0, auto_max_cny: 0, auto_unlimited: true },
    withdrawals: [
      { id: 1, user_id: 2, username: 'tester', amount: 500, fee: 0, cny: 5,
        method: 'alipay', method_label: '支付宝',
        alipay_account: 'tester@example.com', real_name: '测试者',
        account_info: 'tester@example.com / 测试者',
        status: 'pending', status_label: '待审核', note: '', reviewed_at: 0, reviewed_by: 0,
        reviewer_name: '', created_at: now, auto: false },
      { id: 2, user_id: 2, username: 'tester', amount: 200, fee: 0, cny: 2,
        method: 'alipay', method_label: '支付宝',
        alipay_account: 'tester@example.com', real_name: '测试者',
        account_info: 'tester@example.com / 测试者',
        status: 'approved', status_label: '已打款', note: '已打款', reviewed_at: now, reviewed_by: 1,
        reviewer_name: 'admin', created_at: now - 3600, auto: false },
    ],
  },
  '/api/admin/settings': {
    multiplier: 1.5, register_enabled: true, reserve_rate: 0.02, reserve_max: 100,
    rate_limit_rpm: 60, alert_webhook: '', alert_min_healthy: 3,
    proxy_groups: '反代', proxy_accounts: '', proxy_min_gap_seconds: 45, pick_mode: 'weighted',
    egress_proxy_default: '', observed_models: 2, host_share_ratio: 0.6, referral_ratio: 0.1,
    withdraw_rate: 0.01, withdraw_min_points: 100,
    // v0.29.0：提现审核方式（默认人工）与免审额度上限。
    withdraw_review_mode: 'manual', withdraw_auto_max_points: 0,
    egress_max_per_proxy: 3, egress_jitter_ms: 0,
    // v0.26.1（R1）：线路失效处置策略，strict = 不静默换出口。
    egress_on_line_down: 'strict',
    // v0.26.1（R4/R6）：两个默认关闭的开关。
    fingerprint_samples: 'off', pick_sink_on_5xx: 'off',
    // v0.26.3：设备凭证的派生兜底与陈旧阈值。
    device_token_fallback: 'off', device_token_stale_days: 30,
  },
  // v0.14.0 出口代理（防封）：线路台账 + 风控视角聚合。
  '/api/admin/egress/proxies': {
    proxies: [{
      id: 'egr-1', name: '香港 01', url: 'socks5://1.2.3.4:1080', region: 'HK', note: '',
      enabled: true, last_ok: true, last_ip: '203.0.113.7', last_check: now,
      fail_streak: 0, created_at: now, updated_at: now,
    }],
    max_per_proxy: 3,
  },
  // v0.26.1：设备凭证审计（R2）+ 选号均衡度（R7）+ 候选队列预览（R8）。
  '/api/admin/device-tokens': {
    total: 4, by_state: { account: 1, stale: 1, derived: 1, global: 0, none: 1 },
    distinct_fps: 3, shared: [], fallback_on: true, stale_days: 30,
    rows: [
      { uid: 'acc-aaaaaaaaaaaa', nickname: '甲号', state: 'account', fp: 'a1b2c3d4e5f60718', token_len: 128, age_days: 3 },
      { uid: 'acc-bbbbbbbbbbbb', nickname: '乙号', state: 'stale', fp: 'bbbb111122223333', token_len: 96, age_days: 45 },
      { uid: 'acc-cccccccccccc', nickname: '丙号', state: 'derived', fp: 'dddd444455556666', token_len: 32 },
      { uid: 'acc-dddddddddddd', nickname: '丁号', state: 'none' },
    ],
    verdict: 'has-stale', advice: '有 1 个账号的凭证已超过 30 天未更新。',
  },
  '/api/admin/fingerprint-samples': {
    enabled: false, file: 'data/fingerprint_samples.jsonl', count: 0, full: false,
    kinds: {}, samples: [],
  },
  // v0.28.0：出站指纹规则（R4）。桩必须**按真接口形状**给（内置是只读视图、
  // 自定义可增删、max 是条数上限）—— 否则测的是桩的懒惰而不是界面的行为。
  '/api/admin/fingerprint-rules': {
    max: 200,
    builtin: [
      { id: 'fp-builtin-ident', kind: 'literal',
        match: "You are Claude Code, Anthropic's official CLI for Claude",
        replace: "You are Claude Code, Anthropic's official CLI tool for Claude",
        note: '内置（编译期常量，只读）', source: 'builtin', builtin: true },
      { id: 'fp-builtin-hdr', kind: 'regex', match: 'x-anthropic-billing-header:[^;\\n]*;?\\s*',
        replace: '', note: '内置：整段删除', source: 'builtin', builtin: true },
    ],
    custom: [],
  },
  // v0.26.3：消耗失败台账（统计 + 明细 + 处置策略表）。
  '/api/admin/consume-failures': {
    window_hours: 24, since: 0, limit: 100, retention: 5000, total: 12,
    rows: [
      { id: 3, at: 1791972000, stage: 'upstream', code: 6004, code_key: '6004', kind: 'soft_rate',
        scope: 'model', policy: '6004', auto_recover: true, status: 429, model: 'hy3-x',
        uid: 'acc-aaaaaaaaaaaa', source: 'proxy', reset_source: 'body-text',
        detail: '上游 429：模型用量超限｜请求形状 messages=3 stream' },
      { id: 2, at: 1791971000, stage: 'settle', code_key: 'consume:settle', scope: 'account',
        auto_recover: true, source: 'settle', detail: '预扣结算失败：not found' },
      { id: 1, at: 1791970000, stage: 'reserve', code_key: 'consume:reserve', scope: 'request',
        auto_recover: true, user_id: 1, source: 'reserve', detail: '预扣被拒：余额不足（需 12 点）' },
    ],
    stats: [
      { code_key: '6004', stage: 'upstream', count: 9, last_at: 1791972000, auto_recover: true,
        scope: 'model', handling: '只冷却该 (账号, 模型)：对齐上游重置墙钟，无时间信息则有界退避' },
      { code_key: 'consume:reserve', stage: 'reserve', count: 2, last_at: 1791970000,
        auto_recover: true, scope: 'request', handling: '拒绝请求（402），不扣费、不罚账号' },
      { code_key: 'consume:settle', stage: 'settle', count: 1, last_at: 1791971000,
        auto_recover: true, scope: 'account', handling: '留痕并释放预扣（不静默吞掉），该笔实际未扣费' },
      // 未声明处置的码：必须照样出现在统计里并显式标出「未声明处置」——
      // 上游随时会冒出新码，静默丢掉等于让台账在最需要它的时候失明。
      { code_key: '99999', stage: 'upstream', count: 1, last_at: 1791970500 },
    ],
    policies: [
      { key: '6004', code: 6004, name: '模型级用量超限', stage: 'upstream', scope: 'model',
        handling: '只冷却该 (账号, 模型)', auto_recover: true, note: '绝不写账号级冷却' },
      { key: '11140', code: 11140, name: '请求非法', stage: 'upstream', scope: 'account',
        handling: '专用 2h 软冷却 + 连续计数', auto_recover: true },
      { key: 'consume:settle', name: '预扣结算失败', stage: 'settle', scope: 'account',
        handling: '留痕并释放预扣', auto_recover: true },
      // 需人工介入的一档（界面上必须与"会自动恢复"区分开）
      { key: '12153', code: 12153, name: '会话失效（需重新登录）', stage: 'upstream',
        scope: 'account', handling: '连续计数，满阈值后停用并归档', auto_recover: false },
    ],
  },
  '/api/admin/selection-balance': {
    window_hours: 24, total_requests: 1050, accounts_used: 23, accounts_total: 27,
    coverage_pct: 85.2, ideal_share_pct: 3.7, max_share_pct: 7.9, skew_ratio: 2.13,
    median: 48, min: 2, top: [{ uid: 'acc-aaaaaaaaaaaa', count: 83, share_pct: 7.9 }],
    warn: false, verdict: 'ok', advice: '分布健康：最高占比 7.9%，约 2.1 倍偏斜。',
  },
  '/api/admin/selection-candidates': {
    model: '', realm: '', total: 3, included: 2,
    candidates: [
      { uid: 'acc-aaaaaaaaaaaa', included: true, weight: 12.5, rank: 1, in_top5: true, credits: 900 },
      { uid: 'acc-bbbbbbbbbbbb', included: true, weight: 8.1, rank: 2, in_top5: true, credits: 400 },
      { uid: 'acc-cccccccccccc', included: false, skip: 'cooling',
        skip_text: '账号级冷却中（至 10:00:00）', credits: 0 },
    ],
  },
  '/api/admin/egress/overview': {
    proxies: [{
      id: 'egr-1', name: '香港 01', url: 'socks5://1.2.3.4:1080', region: 'HK', note: '',
      enabled: true, last_ok: true, last_ip: '203.0.113.7', last_check: now, fail_streak: 0,
      account_count: 2, over_threshold: false, owners: [1, 2], owner_conflict: [],
    }],
    direct_account_count: 1, direct_owners: [0], direct_explicit: 1, default_egress: '',
    bad: [{ uid: 'bad-1', nickname: '出口坏的号', owner_id: 0, reason: 'socks5h 不受支持' }],
    max_per_proxy: 3, over_count: 0, unassigned: 1,
  },
  '/api/admin/status': {
    total: 4, healthy: 3, cooling: 1, disabled: 0, sticky: 2, servable: true,
    alerts: { active: [], webhook_set: false, min_healthy: 3, last_eval: 0 }, version: '0.8.0',
  },
  '/api/admin/users': {
    users: [
      { id: 1, username: 'admin', role: 'admin', status: 'active', balance: 0, created_at: now, requests: 0, tokens: 0, charged: 0, daily_limit: 0, monthly_limit: 0, daily_used: 0, monthly_used: 0 },
      { id: 2, username: 'tester', role: 'user', status: 'active', balance: 99.49, created_at: now, requests: 12, tokens: 3456, charged: 0.05, daily_limit: 100, monthly_limit: 1000, daily_used: 12.5, monthly_used: 800,
        host_count: 2, host_groups: [{ group: '主力', count: 1 }, { group: '备用', count: 1 }], host_earned: 12.5 },
    ],
  },
  // v0.27.0：人工余额调整的返回。必须回显 before/after —— 界面靠它显示
  // "已从 X 调整为 Y"（只提示"已调整"无法让人确认到底改成了多少）。
  '/api/admin/users/1/balance': { id: 1, balance: 300, before: 100, after: 300, delta: 200 },
  '/api/admin/codes': {
    codes: [
      { code: 'WB-AAAA-BBBB-CCCC', points: 100, used: false, used_by: 0, used_at: 0 },
      { code: 'WB-DDDD-EEEE-FFFF', points: 50, used: true, used_by: 2, used_at: now },
    ],
  },
  '/api/admin/tasks': {
    enabled: true,
    kinds: [{ kind: 'checkin', desc: 'd1' }, { kind: 'cat', desc: 'd2' }],
    // v0.17.4：运行态 + 下次触发时刻（让「定时有效」「任务在跑」对前端可见）。
    // 刻意让 checkin 处于运行中，以断言「执行中」徽标与按钮禁用。
    running: ['checkin'],
    next_runs: {
      checkin: { at: now + 3600, rel: 3600 },
      cat: { at: now + 7200, rel: 7200 },
    },
    // v0.17.4：上次**自动**触发快照。与下面的 runs 刻意形成对照 ——
    // runs 里 checkin 是 manual（人工跑过），而 last_auto_runs 里它是 schedule。
    // 这正是「人工触发覆盖台账后，定时跑过没仍可查」的证据。
    last_auto_runs: {
      checkin: { kind: 'checkin', trigger: 'schedule', started: new Date(Date.now() - 7200000).toISOString(), finished: new Date(Date.now() - 7199000).toISOString(), elapsed: '5s', total: 18, ok: 18, already: 0, fail: 0, skipped: 0 },
    },
    // v0.17.2：两条不同触发来源的记录 —— checkin 是 manual（复现用户看到的现象：
    // 自动排程的签到，因为被人工点过一次「立即执行」，最近一轮显示成「手动」），
    // cat 是 schedule。用来断言两个标签维度都正确渲染。
    runs: {
      'checkin:1': { kind: 'checkin', trigger: 'manual', started: new Date().toISOString(), finished: new Date().toISOString(), elapsed: '5s', total: 3, ok: 2, already: 1, fail: 0, skipped: 0, all_failed: false },
      'cat:1': { kind: 'cat', trigger: 'schedule', started: new Date(Date.now() - 3600000).toISOString(), finished: new Date(Date.now() - 3599000).toISOString(), elapsed: '3s', total: 1, ok: 1, already: 0, fail: 0, skipped: 0, all_failed: false },
      // v0.29.0：**刻意**塞一条已移除类别的记录（模拟上一版留下的台账文件 /
      // 手工改过的 json）。它必须在渲染前被丢掉 —— 多渲染一张"永远不会更新、
      // 点了也没反应"的卡片，比少一张更容易把人带偏（看起来像这个任务还在）。
      'school:1': { kind: 'school', trigger: 'manual', started: new Date(Date.now() - 86400000).toISOString(), finished: new Date(Date.now() - 86390000).toISOString(), elapsed: '16.8s', total: 33, ok: 33, already: 0, fail: 0, skipped: 0, all_failed: false },
    },
    scheduler_up: now - 3700, scheduler_sec: 3700,
  },
  '/api/admin/task-config': {
    // v0.17.0：成长任务改为手动选号执行，配置里不再有 growth_* 排程字段；
    // 只保留 growth_available 供设置页提示脚本链路是否可用。
    config: {
      enabled: true, jitter_minutes: 5,
      checkin_hours: [9, 21], travel_hours: [9, 21], activity_hours: [10],
      keepalive_hours: [22], cat_hours: [1],
      checkin_disabled: false, travel_disabled: false, activity_disabled: false,
      keepalive_disabled: false, cat_disabled: false,
      activity_report_count: 5, expiring_soon_days: 7, keepalive_days: 0, lazy_refresh_hours: 0,
      // v0.17.3：批内任务错峰（0 = 关闭）。
      batch_stagger_seconds: 150, batch_stagger_min_seconds: 60,
    },
    growth_available: true,
  },
  // 成长任务（v0.17.1）：手动选号执行 + 完成进度。
  // overview 只回本地信息（progress 可空 = 还没查过）；progress 才返回明细。
  '/api/admin/growth/overview': {
    accounts: [
      {
        uid: '11111111-1111-4111-8111-111111111111', nickname: '测试账号', realm: 'cn',
        disabled: false, has_token: true, growth_done_at: now - 3600, progress: GROWTH_PV,
      },
      {
        uid: 'abn-0000-0000-0000', nickname: '被封号', realm: 'cn',
        disabled: true, has_token: false, growth_done_at: 0, progress: null,
      },
    ],
    summary: { total: 2 },
  },
  '/api/admin/growth/progress': { views: [GROWTH_PV], count: 1 },
  '/api/host/growth/overview': {
    accounts: [{
      uid: 'host-1', nickname: '代挂号A', realm: 'cn',
      disabled: false, has_token: true, growth_done_at: 0, pool_state: 'in', progress: GROWTH_PV,
    }],
    total: 1,
  },
  '/api/host/growth/progress': { views: [GROWTH_PV], count: 1 },
  '/api/admin/growth/run': { kind: 'growth', status: 'started', count: 1, note: '已在后台开始执行 1 个账号' },
  '/api/host/growth/run': { kind: 'growth', status: 'started', count: 1, note: '已在后台开始执行 1 个账号' },
  // 号池仪表盘聚合（v0.17.3）：服务端算好的数字，浏览器只渲染。
  '/api/admin/pool-dashboard': {
    updated_at: now, total: 2, healthy: 1, cooling: 1, disabled: 1,
    // v0.17.8：冷却账号明细（环形图只给数量，这里给出是哪几个、还剩多久）。
    cooling_accounts: [{ name: '代挂号B', kind: 'hard', remaining_sec: 90, reason: 'upstream busy' }],
    // v0.23.3：停用明细与冷却**分开**（一个会自己好、一个要人工介入）。
    // 刻意给两种来源各一条，以断言「手动停用 / 系统禁用（原因）」都能显示。
    disabled_accounts: [
      { uid: 'acc-man0000001', name: '手动停的号', reason: '手动停用', manual: true },
      { uid: 'acc-sys0000001', name: '被封号', reason: '系统禁用（session dead）', manual: false },
    ],
    disabled_truncated: false,
    // v0.26.5：模型级冷却明细（与账号级冷却**分开**）。刻意给两条不同来源：
    // 一条上游明示、一条本地估 —— 两者在排障时含义完全不同，必须都能显示。
    model_cooling: [
      { uid: 'acc-aaaaaaaaaaaa', name: '测试账号', model: 'glm-5.3', remaining_sec: 3600,
        reset_at: now + 3600, source: 'upstream' },
      { uid: 'acc-bbbbbbbbbbbb', name: '代挂号B', model: 'kimi-k3-1', remaining_sec: 600,
        source: 'local' },
    ],
    model_cooling_accounts: 2,
    model_cooling_truncated: false,
    cn: 2, global: 0, hosting: 0,
    credit_sum: 1234, credit_avg: 617, credit_max: 1234, served_sum: 13, consumed_sum: 0.05,
    growth_done: 1,
    group: { apply: 0, proxy: 1, idle: 1, abnormal: 0 },
    expiry: { expired: 0, d7: 1, d30: 1, later: 0, none: 0 },
    egress: { none: 1, proxy: 1, direct: 0, error: 0 },
    task_cover: { checkin: 2, travel: 1, activity: 2, keepalive: 2, cat: 2 },
    fingerprint_anomaly: 0,
    top_credit: [{ label: '测试账号', value: 1234 }, { label: '被封号', value: 0 }],
    top_served: [{ label: '测试账号', value: 0.05 }],
  },
  // 账号代理（v0.17.9）：逐账号的配置值与实际出口（五种状态各来一条）。
  '/api/admin/egress/accounts': {
    accounts: [
      { uid: 'acc-bad', nickname: '坏代理', realm: 'cn', owner_id: 0,
        configured: 'http://[::1]:0', resolved: 'http://[::1]:0', state: 'bad',
        state_label: '配置异常', egress_error: '代理 URL 无法解析', disabled: false, cooling: false },
      { uid: 'acc-blank', nickname: '未指定', realm: 'cn', owner_id: 0,
        configured: '', resolved: '', state: 'direct_implicit',
        state_label: '未指定 · 直连', disabled: false, cooling: false },
      { uid: 'acc-direct', nickname: '直连号', realm: 'cn', owner_id: 2, owner_name: 'tester',
        configured: 'direct', resolved: 'direct', state: 'direct_explicit',
        state_label: '显式直连', disabled: false, cooling: false },
      { uid: 'acc-inherit', nickname: '继承号', realm: 'cn', owner_id: 0,
        configured: '', resolved: 'http://9.9.9.9:3128', state: 'inherit',
        state_label: '继承默认', disabled: false, cooling: true },
      { uid: 'acc-proxy', nickname: '指定线路', realm: 'cn', owner_id: 0,
        configured: 'socks5://1.1.1.1:1080', resolved: 'socks5://1.1.1.1:1080',
        state: 'proxy', state_label: '指定线路', proxy_id: 'p1', proxy_name: '香港 01',
        disabled: false, cooling: false },
    ],
    summary: { bad: 1, direct_implicit: 1, direct_explicit: 1, inherit: 1, proxy: 1 },
    default_egress: 'http://9.9.9.9:3128',
    max_per_proxy: 3,
  },
  // v0.19.0：账号上架 / 买断。
  '/api/admin/sales': {
    min_trade: 4000, unknown_credits: 0,
    on_sale: [{ uid: 'acc-sale000001', nickname: '在售号', realm: 'cn', credits: 5234,
      group: '闲置', owner_id: 0, listed_at: now - 3600, sale_note: '测试挂牌',
      price: 300, price_micro: 300000000 }],
    listable: [{ uid: 'acc-list000001', nickname: '候选号', realm: 'cn', owner_id: 0,
      pool_state: '', group: '反代' }],
    purchases: [{ id: 1, uid: 'acc-buy0000001', nickname: '已售号', buyer_id: 2,
      buyer_name: 'tester', seller_id: 0, seller_name: '平台', amount: 300,
      amount_cny: 3, seller_share: 0, created_at: now - 7200 }],
  },
  '/api/sale/accounts': {
    accounts: [{ uid: 'acc-sale000001', nickname: '在售号', realm: 'cn', credits: 5234,
      owner_id: 0, listed_at: now - 3600, sale_note: '测试挂牌', group: '闲置',
      price: 300, price_micro: 300000000 }],
    owned: [
      { uid: 'acc-my00000001', nickname: '我的可卖号', realm: 'cn', credits: 6000,
        credits_known: true, on_sale: true, pool_state: 'in', group: '用户申请', price: 120 },
      { uid: 'acc-my00000002', nickname: '我的低分号', realm: 'cn', credits: 120,
        credits_known: true, on_sale: false, pool_state: 'in', group: '用户申请',
        blocked: '积分不足 4000，不可上架' },
    ],
    purchases: [{ uid: 'acc-buy0000001', amount: 300, created_at: now - 7200 }],
    balance: 500, rate: 0.01, min_trade_credits: 4000,
  },
  // v0.26.7：更新检测 + 自动下载 + 更新简介。刻意给**多行 notes**（验证逐行渲染）
  // 与 **done 的下载状态**（验证产物徽标、体积、应用按钮三处都出现）。
  '/api/admin/update-status': {
    current: '0.26.6', latest: '0.26.7', has_update: true, severity: 'minor',
    notes: 'v0.26.7：\n① 补齐耗时与缓存观测；\n② 修报表日界缺陷。',
    release_url: 'https://example.com/releases',
    asset: { os: 'windows', arch: 'amd64', url: 'https://example.com/a.exe',
      sha256: 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' },
    manifest_url: 'https://example.com/version.json',
    manifest_used: 'https://example.com/version.json',
    checked_at: now, enabled: true, auto_download: true,
    download: { state: 'done', version: '0.26.7', size: 15728640, got_size: 15728640,
      sha256: 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
      file: 'webapp.exe.new', at: now,
      apply_hint: '1) 停止本程序；2) 运行 apply-update.bat' },
  },
  '/api/admin/update-apply': { ok: true, script: 'apply-update.bat',
    apply_hint: '1) 停止本程序；2) 运行 apply-update.bat' },
  // v0.27.0：用户侧计费倍率（当前 + 变更历史）。刻意给一条"降价"记录：
  // 涨/降两种方向的颜色与文案都要能渲染出来。
  '/api/multiplier': {
    current: 1.2,
    history: [
      { old_value: 1.5, new_value: 1.2, note: '算力成本回落', created_at: now - 86400,
        change_percent: -20, direction: 'down' },
      { old_value: 1.0, new_value: 1.5, note: '', created_at: now - 10 * 86400,
        change_percent: 50, direction: 'up' },
    ],
  },
  // 后台：调价历史（含调整人）。第二条刻意 announced=false —— 管理端必须看得见
  // "这次没发出公告"，否则用户收不到通知而没人知道。
  '/api/admin/multiplier-history': {
    current: 1.2,
    announce_ttl_hours: 168,
    history: [
      { id: 2, old_value: 1.5, new_value: 1.2, admin_id: 1, admin_name: 'admin',
        note: '算力成本回落', announced: true, announce_id: 7,
        created_at: now - 86400, change_percent: -20, direction: 'down' },
      { id: 1, old_value: 1.0, new_value: 1.5, admin_id: 1, admin_name: 'admin',
        note: '', announced: false, announce_id: 0,
        created_at: now - 10 * 86400, change_percent: 50, direction: 'up' },
    ],
  },
  // 后台：余额变动记录（含一条失败记录）。
  '/api/admin/balance-audits': {
    rate: 0.01,
    audits: [
      { id: 2, user_id: 2, username: 'tester', admin_id: 1, admin_name: 'admin',
        delta_micro: 50000, before_micro: 100000, after_micro: 150000,
        reason: '客服补偿', result: 'ok', created_at: now - 3600 },
      { id: 1, user_id: 2, username: 'tester', admin_id: 1, admin_name: 'admin',
        delta_micro: 10000, before_micro: 0, after_micro: 0,
        reason: '', result: 'rejected', error: '用户不存在', created_at: now - 7200 },
    ],
  },
  '/api/admin/usage': {
    days: 14, totals: { requests: 120, tokens: 12345, charged: 1.23 },
    daily: [{ day: '2026-10-09', requests: 10, tokens: 1000, charged: 0.1 }],
    models: [{ model: 'deepseek-v4-flash', requests: 80, tokens: 9000, charged: 0.9 }],
    // v0.26.7：性能与前缀缓存观测。刻意给三段不同量级的耗时（1 秒内 / 几秒 /
    // 两分多钟）—— 这正好覆盖 fmtDur 的三个分支，也是真实排查里同时存在的形态。
    perf: {
      latency_sampled: 40, avg_latency_ms: 99000, p50_latency_ms: 4520,
      p95_latency_ms: 126481, max_latency_ms: 216800,
      cache_sampled: 12, cache_hit_rate: 0.994,
      cache_note: '命中率按 token 加权：hit / (hit + miss)，仅统计上游明确回报过的请求',
    },
  },
  '/api/usage/report': {
    days: 14, totals: { requests: 12, tokens: 3456, charged: 0.05 },
    daily: [{ day: '2026-10-09', requests: 10, tokens: 3000, charged: 0.04 }],
    models: [{ model: 'deepseek-v4-flash', requests: 10, tokens: 3000, charged: 0.04 }],
  },
  '/api/usage/breakdown': {
    days: 14,
    self_use: { requests: 3, tokens: 900, charged: 0, upstream_credit: 0.9,
      daily: [{ day: '2026-10-09', requests: 3, tokens: 900, charged: 0, upstream_credit: 0.9 }],
      models: [{ model: 'deepseek-v4-flash', requests: 3, tokens: 900, charged: 0, upstream_credit: 0.9 }] },
    pool_use: { requests: 9, tokens: 2556, charged: 0.05, upstream_credit: 0.05,
      daily: [{ day: '2026-10-09', requests: 9, tokens: 2556, charged: 0.05, upstream_credit: 0.05 }],
      models: [{ model: 'deepseek-v4-flash', requests: 9, tokens: 2556, charged: 0.05, upstream_credit: 0.05 }] },
    pool_earn: { requests: 5, charged: 0.15, earned: 0.09, total: 12.5, count: 3,
      daily: [{ day: '2026-10-09', requests: 5, charged: 0.15, earned: 0.09 }],
      models: [{ model: 'deepseek-v4-flash', requests: 5, charged: 0.15, earned: 0.09 }] },
  },
  '/api/referral': {
    invite_code: 'ABCD2345', invite_link: '/?ref=ABCD2345#register', referred_count: 2,
    // rate 元/点：折算比例 0.01 → 100 点 = 1 元（v0.17.7 起返点按金额展示）
    rate: 0.01,
    ratios: {
      referral: 0.1, pool: 0.6,
      global: { referral: 0.1, pool: 0.6 },
      // pool 为「单独设定」、referral 为「跟随全局」：两种来源都要覆盖到断言。
      pool_detail: { override: 0.6, global: 0.5, effective: 0.6, is_custom: true },
      referral_detail: { override: -1, global: 0.1, effective: 0.1, is_custom: false },
    },
    summary: {
      referral_recharge: 8, referral_recharge_cny: 0.08,
      referral_pool: 1.2, referral_pool_cny: 0.012,
      referral_total: 9.2, referral_total_cny: 0.092,
      pool_total: 12.5, pool_total_cny: 0.125, pool_count: 3,
    },
    rows: [{
      kind: 'referral_recharge', from_user: 9, account: '',
      base: 80, base_cny: 0.8, rebate: 8, rebate_cny: 0.08,
      note: '充值返点', created_at: now,
    }],
  },
  '/api/models': { models: ['deepseek-v4-flash', 'glm-5.2'] },
  '/api/usage': {
    summary: { requests: 12, tokens: 3456, charged: 0.05, balance: 99.49, multiplier: 1.5 },
    logs: [{ id: 1, model: 'deepseek-v4-flash', prompt_tokens: 10, completion_tokens: 2, credit: 0, charged: 0, status: 'ok', error: '', created_at: now }],
  },
  // v0.17.8：列表**只有掩码**，不再返回明文（key 字段已移除）。
  // 完整值走显式接口 /api/keys/{id}/reveal。
  '/api/keys': {
    keys: [
      { id: 1, name: '笔记本', masked: 'sk-wb-••••••••mnop', prefix: 'sk-wb-abcdef', status: 'active', last_used_at: now, kind: '', host_group: '', account_uids: [], revealable: true },
      { id: 2, name: '我的笔记本', masked: 'sk-wb-••••••••tkey', prefix: 'sk-wb-hostk', status: 'active', last_used_at: now, kind: 'host', host_group: '主力', account_uids: ['host-1'], revealable: true },
      { id: 3, name: '停用的', masked: 'sk-wb-••••••••', prefix: 'sk-wb-off', status: 'banned', last_used_at: 0, kind: 'host', host_group: '', account_uids: [], revealable: false },
    ],
  },
  '/api/keys/1/reveal': { id: 1, key: 'sk-wb-abcdefghijklmnop', masked: 'sk-wb-••••••••mnop' },
  // 站内公告（v0.17.8，用户侧只回启用中的）
  // v0.22.0：接口只回「生效中」的公告，并带 state / time_window 派生字段。
  // 这里给一条**置顶 + 有时限**的，用来断言「📌 置顶」与「有效期至」都被渲染。
  '/api/announcements': {
    announcements: [{
      id: 7, title: '维护通知', content: '今晚 02:00-03:00 升级，期间可能短暂不可用。',
      level: 'warn', enabled: true, pinned: true, created_at: now, updated_at: now,
      start_at: 0, end_at: now + 86400,
      state: 'active', state_label: '生效中', time_window: '至 2026-10-13 12:00 止',
    }],
    count: 1,
  },
  // 管理端不做时间过滤：过期公告仍在列表里（否则没法延长时限），靠状态标签区分。
  '/api/admin/announcements': {
    announcements: [
      { id: 7, title: '维护通知', content: '今晚 02:00-03:00 升级，期间可能短暂不可用。',
        level: 'warn', enabled: true, pinned: false, created_at: now, updated_at: now,
        start_at: 0, end_at: 0,
        state: 'active', state_label: '生效中', time_window: '长期有效' },
      { id: 8, title: '限时活动', content: '三天内有效，过期自动下线。',
        level: 'info', enabled: true, pinned: true, created_at: now - 3600, updated_at: now - 3600,
        start_at: 0, end_at: now - 60,
        state: 'expired', state_label: '已过期', time_window: '至 2026-10-12 12:00 止' },
    ],
    count: 2,
    by_state: { active: 1, expired: 1 },
    limits: { title: 60, content: 1000, total: 100 },
  },
  // 健康检测最近一次结果（v0.23.0）：覆盖五态 + 自动处置动作 + 导入来源。
  '/api/admin/accounts/health/last': {
    has_result: true,
    summary: {
      total: 6, by_state: { healthy: 1, degraded: 2, abnormal: 1, uncertain: 1, skipped: 1 },
      definite: 1, uncertain: 1, started_at: now - 30, ended_at: now - 5,
      source: 'import',
    },
    results: [
      { uid: 'acc-bad0000001', nickname: '坏号', realm: 'cn', state: 'abnormal', state_label: '异常',
        reason_code: 'session_dead', reason: '登录已失效（12153 会话失效）', definite: true,
        actions: ['noted_session_dead', 'disabled', 'archived'], credits_known: false, latency_ms: 120, checked_at: now },
      { uid: 'acc-unc0000001', nickname: '被拦号', realm: 'cn', state: 'uncertain', state_label: '不确定',
        reason_code: 'waf_block', reason: '被上游 WAF 拦截（403）', definite: false,
        credits_known: false, latency_ms: 88, checked_at: now },
      { uid: 'acc-deg0000001', nickname: '降权号', realm: 'cn', state: 'degraded', state_label: '降权中',
        reason_code: 'degraded_local', reason: '凭证有效，但当前不参与选号：冷却中（剩余 42s）',
        credits_known: true, credits: 800, latency_ms: 95, checked_at: now },
      // v0.23.4：曾被系统禁用但探测通过 → 后端给结构化 action=revive，前端渲染「复活」按钮。
      // 这是本次修复的核心场景：此前这个号点「启用」没用（池内系统禁用位没被清），
      // 而且检测页也没有入口，运维只能自己去找。
      { uid: 'acc-rev00000001', nickname: '误判禁用号', realm: 'cn', state: 'degraded', state_label: '降权中',
        reason_code: 'degraded_local',
        reason: '凭证有效，但当前不参与选号：曾被系统禁用（12153 session dead）——本次探测通过，可考虑「复活」',
        action: 'revive', credits_known: true, credits: 1200, latency_ms: 91, checked_at: now },
      { uid: 'acc-skip000001', nickname: '手动摘除号', realm: 'cn', state: 'skipped', state_label: '已跳过',
        reason_code: 'manual_disabled', reason: '运维手动停用，未参与检测', action: 'enable',
        credits_known: false, latency_ms: 0, checked_at: now },
      { uid: 'acc-ok00000001', nickname: '好号', realm: 'cn', state: 'healthy', state_label: '正常',
        reason_code: 'ok', reason: '账号正常（凭证有效、余额充足、无降权）', definite: false,
        actions: ['cleared_dead', 'credits_synced'], credits_known: true, credits: 5234,
        latency_ms: 110, checked_at: now },
    ],
    limits: { max_accounts: 60 },
  },
  // 消耗排行（v0.17.8）：用户侧脱敏 + 附自己的名次。
  '/api/usage/ranking': {
    days: 14, order_by: 'charged', total_users: 5, limit: 10, masked: true,
    list: [
      { rank: 1, name: 'a***e', charged: 88, requests: 10, tokens: 1000, is_me: false },
      { rank: 2, name: 'tester', charged: 12, requests: 3, tokens: 300, is_me: true, user_id: 2 },
    ],
    me: { rank: 2, name: 'tester', charged: 12, requests: 3, tokens: 300, in_top: true, total_users: 5 },
  },
  '/api/admin/usage/ranking': {
    days: 14, order_by: 'charged', total_users: 5, limit: 20,
    list: [
      { rank: 1, user_id: 3, name: 'alice', charged: 88, requests: 10, tokens: 1000 },
      { rank: 2, user_id: 2, name: 'tester', charged: 12, requests: 3, tokens: 300 },
    ],
  },
  // 充值提现（用户侧）
  '/api/wallet': {
    balance: 99.49,
    params: { rate: 0.01, min_points: 100 },
    recharges: [{ code: 'WB-AAA…CCCC', credits: 100, added_at: now }],
    withdrawals: [
      { id: 1, user_id: 2, amount: 500, fee: 0, cny: 5, method: '支付宝', account_info: 'te***@example.com', status: 'pending', note: '', reviewed_at: 0, created_at: now },
      { id: 2, user_id: 2, amount: 200, fee: 0, cny: 2, method: '微信', account_info: 'wx***bc', status: 'approved', note: '已打款', reviewed_at: now, created_at: now - 3600 },
    ],
    summary: { pending: 500, approved: 200, rejected: 0, count: 2, pending_apply: 500, withdrawable: 99.49 },
  },
  // 用户代挂（管理侧）
  '/api/admin/host/accounts': {
    accounts: [{
      uid: 'host-1', nickname: '代挂号A', realm: 'cn', host_group: '主力', pool_state: 'in',
      host_note: '已通过审核', disabled: false, cooling: false, cooling_for: 0, credits: 800,
      expires_at: now + 86400, earned: 12.5, created_at: now, owner_id: 2, owner_name: 'tester',
      task: { enabled: true, disabled: [], kinds: [] },
    }],
    owners: [{ owner_id: 2, owner_name: 'tester', count: 1 }],
    pending: [{
      uid: 'host-2', nickname: '代挂号B', realm: 'cn', host_group: '备用', pool_state: 'pending',
      disabled: false, cooling: false, cooling_for: 0, credits: 300,
      expires_at: now + 86400, earned: 0, created_at: now, owner_id: 2, owner_name: 'tester',
      task: { enabled: true, disabled: [], kinds: [] },
    }],
  },
  // 用户代挂（用户侧）
  '/api/host/accounts': {
    accounts: [{
      uid: 'host-1', nickname: '代挂号A', realm: 'cn', host_group: '主力', pool_state: 'in',
      disabled: false, cooling: false, cooling_for: 0, credits: 800,
      expires_at: now + 86400, earned: 12.5, created_at: now, owner_id: 2,
      // v0.26.7：已挂载时长（与管理后台同口径）。
      mounted_known: true, mounted_at: now - 30 * 86400, mounted_days: 30,
      mounted_text: '已挂载 30 天',
      task: { enabled: true, disabled: ['travel'], kinds: [] },
    }],
    groups: { '主力': 1 },
    summary: { total: 1, in_pool: 1, pending: 0, earned: 12.5, earnings: 3 },
  },
  '/api/host/earnings': {
    rows: [{ account_uid: 'host-1', model: 'deepseek-v4-flash', charged: 0.05, earned: 0.03, created_at: now }],
    total: 12.5, count: 3,
  },
  // 逐套餐积分明细（/resources 结尾，见 fixtureFor 特判）
  __credit: {
    ok: true, remain: 800, used: 200, size: 1000, packs: 2, expiring_soon: 100, updated_at: now,
    resources: [
      { name: '体验版', remain: 300, used: 200, size: 500, expire_at: now + 3 * 86400 },
      { name: '拉新权益包', remain: 500, used: 0, size: 500, expire_at: now + 40 * 86400 },
    ],
  },
  // v0.13.1 任务完成情况（/task-status 结尾，见 fixtureFor 特判）
  __taskstatus: {
    uid: 'x', updated_at: now, cached: false,
    checkin: { supported: true, checked_in: true, ok: true },
    travel: { supported: true, ok: true, state: 'idle', daily_limit_reached: true },
  },
};

function fixtureFor(url) {
  const u = String(url);
  // /api/admin/users?q=xxx 真接口会按用户名子串 / ID 精确过滤。桩必须照做：
  // 一个"忽略 q、永远返回全部"的桩，会让"搜不到 → 空态"这类用例**必然失败**
  //（或更糟：让它假通过，因为总有一条恰好命中）。
  const mq = /\/api\/admin\/users\?q=([^&]*)/.exec(u);
  if (mq) {
    const q = decodeURIComponent(mq[1]).toLowerCase();
    const all = (FX['/api/admin/users'] || {}).users || [];
    return { users: all.filter((x) => String(x.username || '').toLowerCase().indexOf(q) >= 0
      || String(x.id) === q) };
  }
  if (/\/resources(\?|$)/.test(u)) return FX.__credit;
  if (/\/task-status(\?|$)/.test(u)) return FX.__taskstatus;
  const keys = Object.keys(FX).sort((a, b) => b.length - a.length);
  for (const k of keys) if (u.startsWith(k)) return FX[k];
  return {};
}

// plan: [{ nav, counts:[[id,n]], texts:[[id,substr]] }]
function run(pageFile, meOverride, plan) {
  return new Promise((resolve) => {
    const errors = [];
    const html = fs.readFileSync(path.join(UI, pageFile), 'utf8')
      .replace('<script src="/app.js"></script>', '<script>' + APP_JS + '</script>');

    const vc = new VirtualConsole();
    vc.on('jsdomError', (e) => {
      const m = String(e && e.message);
      if (/Not implemented: navigation/.test(m)) return;
      errors.push('jsdomError: ' + m);
    });
    vc.on('error', (...a) => errors.push('console.error: ' + a.join(' ')));

    const dom = new JSDOM(html, {
      runScripts: 'dangerously',
      pretendToBeVisual: true,
      virtualConsole: vc,
      url: 'http://127.0.0.1:8780/' + pageFile,
      beforeParse(win) {
        if (meOverride) FX['/api/auth/me'] = { user: meOverride };
        win.onerror = (msg, src, line, col) => errors.push('window.onerror: ' + msg + ' @' + line + ':' + col);
        // v0.26.6：fail 列表里的路径返回 500 —— 失败路径（常驻横幅 / 重试按钮）
        // 此前完全测不到，因为桩恒成功。"能不能扛住失败"恰恰是这批改动的主题。
        win.__failPaths = [];
        win.fetch = (url) => {
          const u = String(url);
          if (win.__failPaths.some((f) => u.indexOf(f) >= 0)) {
            return Promise.resolve({
              ok: false, status: 500,
              text: () => Promise.resolve('{"error":"桩：模拟服务端故障"}'),
              json: () => Promise.resolve({ error: '桩：模拟服务端故障' }),
            });
          }
          return Promise.resolve({
            ok: true, status: 200,
            text: () => Promise.resolve(JSON.stringify(fixtureFor(url))),
            json: () => Promise.resolve(fixtureFor(url)),
          });
        };
        // v0.26.6：原生 confirm() 已被自定义确认框取代。
        // __autoConfirm 让**既有流程用例**不必逐个去点确认键（它们要测的是流程，
        // 不是弹窗）；需要真实验证弹窗的用例会把 dialog 设为 cancel/manual 关掉它。
        win.__autoConfirm = true;
        // v0.27.0：原生 prompt() 已被 promptDialog 取代。默认设 null = "用户取消"
        // —— 保证用例不会因为一个弹窗卡住；需要走通的用例用 prompt 字段显式给值。
        win.__autoPrompt = null;
        // jsdom 未实现 scrollIntoView（浏览器里有）。补一个空实现，
        // 让"把它滚进视野"这类正常代码在测试环境里也能跑通。
        if (win.Element && !win.Element.prototype.scrollIntoView) {
          win.Element.prototype.scrollIntoView = function () {};
        }
        win.confirm = () => true;
        win.prompt = () => '1';
        win.alert = () => {};
        win.open = () => ({ closed: false, location: { href: '' }, close() {} });
        win.navigator.clipboard = { writeText: () => Promise.resolve() };
      },
    });

    const win = dom.window;
    const doc = win.document;
    const lines = [];
    let ok = true;

    const clickNav = (nav) => {
      const it = Array.from(doc.querySelectorAll('.nav-item')).find((x) => x.dataset.nav === nav);
      if (!it) { ok = false; lines.push(`[FAIL] nav item "${nav}" 不存在`); return false; }
      it.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
      return true;
    };

    let i = 0;
    const step = () => {
      if (i >= plan.length) {
        if (errors.length) ok = false;
        console.log(`--- ${pageFile} ---`);
        lines.forEach((l) => console.log('  ' + l));
        errors.forEach((e) => console.log('  [ERR] ' + e));
        console.log(`  RESULT: ${ok ? 'PASS' : 'FAIL'}`);
        try { dom.window.close(); } catch (e) {}
        resolve(ok);
        return;
      }
      const stage = plan[i++];
      // fx: [[path, value]] —— 用例级覆盖某个接口的返回体。
      //
      // 为什么需要：有些面板的正确性恰恰体现在**两种状态**上（样本回流面板
      // "未开启"与"已收集"要说完全不同的话）。共享一份静态桩的话，只能测到
      // 其中一种，另一种只能靠人眼 —— 而人眼恰好最容易漏掉"未开启被画成 0 条"
      // 这类误报。
      for (const [fxPath, fxVal] of (stage.fx || [])) FX[fxPath] = fxVal;
      // dialog: 'cancel' | 'manual' —— 关掉自动确认，让自定义确认框真实渲染。
      //   cancel → 点「取消」（验证"取消后什么都不该发生"）
      //   manual → 保持打开（后面的断言直接看弹窗内容）
      if (stage.dialog) win.__autoConfirm = false;
      // prompt: {字段: 值} —— 让 promptDialog 直接返回该对象（等价于"用户填了这个"）。
      // 传 'cancel' 表示用户取消了那个对话框。
      if (stage.prompt === 'cancel') win.__autoPrompt = null;
      else if (stage.prompt) win.__autoPrompt = stage.prompt;
      else win.__autoPrompt = null;
      // fail: [pathSubstring] —— 用例级让某些接口失败（可叠加，便于"先成功后失败"）。
      if (stage.fail) win.__failPaths = (win.__failPaths || []).concat(stage.fail);
      // run: ['js 源码'] —— 在页面作用域里执行一小段代码。
      // 为什么需要：像 skeletonCards / countUp 这类**纯工具函数**没有用户可点的入口，
      // 只能直接调用；再为它们专门造一个页面入口是本末倒置。
      for (const code of (stage.run || [])) {
        try { win.eval(code); } catch (e) { ok = false; lines.push('[FAIL] run 抛错: ' + e.message); }
      }
      if (stage.nav) clickNav(stage.nav);
      // click: [selector] —— 点击元素（用于打开弹窗、勾选复选框等）。
      // 复选框特殊处理：jsdom 不保证合成 click 会触发它的「激活行为」（勾选态可能不变），
      // 若照搬 click 会让「读 checked 的处理函数」拿不到预期状态 —— 显式翻转 + 补发 change。
      for (const sel of (stage.click || [])) {
        const target = doc.querySelector(sel);
        if (!target) { ok = false; lines.push(`[FAIL] 待点击元素 ${sel} 不存在`); continue; }
        if (target.tagName === 'INPUT' && target.type === 'checkbox') {
          target.checked = !target.checked;
          target.dispatchEvent(new win.Event('change', { bubbles: true }));
        } else {
          target.dispatchEvent(new win.Event('click', { bubbles: true }));
        }
      }
      // clickText: [[selector, text]] —— 按**可见文案**点击，而不是按位置。
      //
      // 为什么必须按文案：位置型选择器（`.seg:nth-child(4)`）在选项列表变动时会
      // **静默指向另一个选项** —— 断言照样"通过"，但测的已经不是原来那件事了。
      // v0.26.5 插入一个「限模」筛选档时就踩到了这个坑：nth-child 整体后移一位，
      // 失败信息（"卡片里没有'被封号'"）看不出根因，只会让人以为筛选坏了。
      for (const [sel, txt] of (stage.clickText || [])) {
        const cands = Array.from(doc.querySelectorAll(sel));
        const target = cands.find((x) => (x.textContent || '').includes(txt));
        if (!target) {
          ok = false;
          lines.push(`[FAIL] clickText ${sel} 含 "${txt}" 未找到（现有：`
            + cands.map((x) => (x.textContent || '').trim()).join(' | ') + '）');
          continue;
        }
        target.dispatchEvent(new win.Event('click', { bubbles: true }));
      }
      // press: [[selectorOrNull, key, modifiers]] —— 派发 keydown。
      // 用来验证键盘交互（⌘K 命令面板、↑↓ 选择、Enter 打开）。
      // selector 为 null 时发到 body（事件照常冒泡/捕获到 document）。
      for (const [sel, key, mods] of (stage.press || [])) {
        const target = sel ? doc.querySelector(sel) : doc.body;
        if (!target) { ok = false; lines.push(`[FAIL] press 目标 ${sel} 不存在`); continue; }
        const m = String(mods || '');
        target.dispatchEvent(new win.KeyboardEvent('keydown', {
          key: key, bubbles: true, cancelable: true,
          ctrlKey: m.indexOf('ctrl') >= 0, metaKey: m.indexOf('meta') >= 0,
          shiftKey: m.indexOf('shift') >= 0,
        }));
      }
      // input: [[id, value]] —— 给输入框赋值并派发 input 事件（验证即时过滤等交互）。
      for (const [id, value] of (stage.input || [])) {
        const inp = doc.getElementById(id);
        if (!inp) { ok = false; lines.push(`[FAIL] 输入框 #${id} 不存在`); continue; }
        inp.value = value;
        inp.dispatchEvent(new win.Event('input', { bubbles: true }));
      }
      // 确认框：默认（dialog 未声明或声明为其他值）自动点「确认」，
      // 让流程用例继续往下走；dialog:'cancel' 点取消；dialog:'manual' 留给断言。
      if (stage.dialog !== 'manual') {
        const sel = stage.dialog === 'cancel' ? '[data-dlg-cancel]' : '[data-dlg-ok]';
        const b = doc.querySelector(sel);
        if (b) b.dispatchEvent(new win.Event('click', { bubbles: true }));
      }
      setTimeout(() => {
        for (const [id, want] of (stage.counts || [])) {
          const el = doc.getElementById(id);
          const got = el ? el.children.length : -1;
          const pass = got === want;
          if (!pass) ok = false;
          lines.push(`${pass ? '[ok]' : '[FAIL]'} ${stage.nav} #${id} children=${got} want=${want}`);
        }
        for (const [id, sub] of (stage.texts || [])) {
          const el = doc.getElementById(id);
          const txt = el ? (el.value !== undefined && el.value !== '' ? el.value : el.textContent) : '(missing)';
          const pass = String(txt).includes(sub);
          if (!pass) ok = false;
          lines.push(`${pass ? '[ok]' : '[FAIL]'} ${stage.nav} #${id} = "${String(txt).slice(0, 40)}" 期待含 "${sub}"`);
        }
        // has: [[cssSelector, minCount]] —— 断言文档中存在至少 N 个匹配元素。
        for (const [sel, min] of (stage.has || [])) {
          const n = doc.querySelectorAll(sel).length;
          const pass = n >= min;
          if (!pass) ok = false;
          lines.push(`${pass ? '[ok]' : '[FAIL]'} ${stage.nav || '-'} has "${sel}" = ${n} (>=${min})`);
        }
        // lacks: [[scopeSelector, substr]] —— 断言该容器内**不**出现该文案
        // （用于验证重复控件确实被删掉，避免只验证「新的在」而漏掉「旧的还在」）。
        for (const [sel, sub] of (stage.lacks || [])) {
          const scope = doc.querySelector(sel);
          const txt = scope ? scope.textContent : '(missing)';
          const pass = !String(txt).includes(sub);
          if (!pass) ok = false;
          lines.push(`${pass ? '[ok]' : '[FAIL]'} ${stage.nav || '-'} lacks "${sub}" in "${sel}"`);
        }
        // absent: [cssSelector] —— 断言文档中**不存在**该元素（删除类改动必须这么验，
        // 否则「控件删了」这件事只靠 lacks 查文本来间接证明，容易漏）。
        for (const sel of (stage.absent || [])) {
          const n = doc.querySelectorAll(sel).length;
          const pass = n === 0;
          if (!pass) ok = false;
          lines.push(`${pass ? '[ok]' : '[FAIL]'} ${stage.nav || '-'} absent "${sel}" = ${n} (want 0)`);
        }
        // before: [[selA, selB]] —— 断言 A 在文档中**排在 B 之前**（两个都要唯一存在）。
        // 顺序类需求必须这么验：只验「两块都在」证明不了谁在前。
        for (const [selA, selB] of (stage.before || [])) {
          const ea = doc.querySelector(selA), eb = doc.querySelector(selB);
          const pass = !!(ea && eb && ea !== eb &&
            (ea.compareDocumentPosition(eb) & win.Node.DOCUMENT_POSITION_FOLLOWING));
          if (!pass) ok = false;
          lines.push(`${pass ? '[ok]' : '[FAIL]'} ${stage.nav || '-'} before: "${selA}" 在 "${selB}" 之前`);
        }
        // display: [[cssSelector, 'none'|'shown']] —— 断言**内联** display 的显隐态。
        // 用来验证「无数据时整块隐藏」（避免首屏留一片空卡）这类需求。
        for (const [sel, want] of (stage.display || [])) {
          const dex = doc.querySelector(sel);
          const disp = dex ? (dex.style.display || '') : '(missing)';
          const pass = want === 'none' ? disp === 'none' : (disp !== 'none' && disp !== '(missing)');
          if (!pass) ok = false;
          lines.push(`${pass ? '[ok]' : '[FAIL]'} ${stage.nav || '-'} display "${sel}" = "${disp}" (want ${want})`);
        }
        // absentIn: [[scopeSelector, childSelector]] —— 「某元素不在**这个容器**里」。
        // 迁移类改动必须用这一条：元素只是搬了家（仍存在于同一文档），全局 absent
        // 会误报失败，而只验「新位置有」又漏掉「旧位置还留着」。
        for (const [scopeSel, childSel] of (stage.absentIn || [])) {
          const scope = doc.querySelector(scopeSel);
          const n = scope ? scope.querySelectorAll(childSel).length : -1;
          const pass = n === 0;
          if (!pass) ok = false;
          lines.push(`${pass ? '[ok]' : '[FAIL]'} ${stage.nav || '-'} absentIn "${scopeSel}" ← "${childSel}" = ${n} (want 0)`);
        }
        // textIn: [[cssSelector, substr]] —— 断言至少有一个匹配元素的文本含该子串
        // （用于验证「某个特定元素」的文案，而 texts 只能按 id 取）。
        for (const [sel, sub] of (stage.textIn || [])) {
          const els = Array.from(doc.querySelectorAll(sel));
          const hit = els.find((e) => String(e.textContent).includes(sub));
          const pass = !!hit;
          if (!pass) ok = false;
          lines.push(`${pass ? '[ok]' : '[FAIL]'} ${stage.nav || '-'} textIn "${sel}" 含 "${sub}" (${els.length} 个候选)`);
        }
        step();
      }, 260);
    };
    setTimeout(step, 450);
  });
}

(async () => {
  const adminPlan = [
    // v0.17.8：侧边栏底部新增「深色/浅色」与「改密码」两个入口（两页都有）。
    // v0.30.0：曲线与事件流必须同时体现**成功与失败**（此前只显示成功 ——
    // 根因是大量失败在被记录之前就被前置环节拒绝了，详见 live.go）。
    { nav: 'overview', counts: [['ovBars', 60], ['ovFeed', 2], ['ovPool', 4], ['rpBars', 60]],
      has: [['#btnTheme', 1], ['#btnChangePw', 1],
        // 失败段：fixture 里恰有 5 个"req>0 且 fail>0"的分钟
        ['#ovBars .bar > i', 5],
        // 事件流里的失败行（红点 + 底色）必须真的渲染出来
        ['#ovFeed .ev.bad', 1], ['#ovFeed .ev.bad .dot', 1]],
      // 空闲分钟（req=0）**不得**长出红段 —— 那会把"没流量"读成"全失败"。
      // ⚠️ 必须用 absent（"必须为 0 个"）：has 的语义是 >=n，写 has:[sel,0] 永远通过。
      absent: ['#ovBars .bar.empty > i'],
      textIn: [['#ovReq60d', '失败 5'], ['#ovKpis', '失败'],
        // 失败行带 HTTP 状态码：文案说"为什么失败"，状态码说"归哪一类"
        ['#ovFeed', 'HTTP 503'], ['#ovFeed', 'upstream 503']] },
    // v0.12.0：号池页顶部两个扫码按钮 + 导入桌面账号按钮。
    // v0.13.0：新增消耗分组统计条（全部 + 4 组）+ 批量设置控件。
    // v0.13.1：卡片 footer 合并为**单个**分组下拉（消耗分组），并新增任务完成情况行。
    // v0.17.3：号池仪表盘已从「账号池」迁到「实时概览」，并改为服务端聚合、
    // 随 3 秒轮询实时刷新。下面两条断言一起锁死「迁走了」这件事：
    // 概览页要有，账号池页不能再有。
    { nav: 'overview',
      counts: [['poolKpis', 7], ['poolRealmKpis', 4]],
      has: [['#poolHealth .donut-svg', 1], ['#poolHealth .kv-row', 3],
        ['#poolGroup .donut-svg', 1], ['#poolTopCredit .hbar-row', 2],
        ['#poolExpiry .hbar-row', 5], ['#poolEgress .hbar-row', 4],
        ['#poolTaskCover .hbar-row', 5], ['#poolServed .hbar-row', 1]],
      texts: [['poolDashStamp', '更新于']],
      // v0.17.8：冷却账号明细（把「冷却 N 个」落到「是哪几个、还剩多久」）
      // + 「刷新余额」按钮（解决导入新号后「积分合计」不更新）。
      textIn: [['#poolKpis', '积分合计'], ['#poolKpis', '1,234'], ['#poolKpis', '出口异常'],
        ['#poolKpis', '指纹'], ['#poolHealth', '可用'], ['#poolGroup', '反代'],
        ['#poolTaskCover', '自动签到'],
        ['#poolCooling', '代挂号B'], ['#poolCooling', '硬冷却'], ['#poolCooling', '1 分 30 秒'],
        // v0.26.5：模型级冷却明细（用户口径「软冷却模型的时候冷却账号明细是否显示」）。
        // 它不是"复制"进账号级明细，而是**独立一块** —— 且必须能区分时长来源。
        ['#poolModelCooling', 'glm-5.3'], ['#poolModelCooling', 'kimi-k3-1'],
        ['#poolModelCooling', '上游明示'], ['#poolModelCooling', '本地估'],
        ['#poolModelCooling', '1 小时'], ['#poolModelCoolingNote', '2 个账号受影响']] },
    { nav: 'overview',
      has: [['#poolModelCooling', 1], ['#poolModelCooling .egr-help-table', 1],
        ['#poolModelCooling tbody tr', 2],
        // ★ 关键：模型级冷却**不得**混进账号级冷却明细（口径不同，混了会把
        // 「整号不可用」与「只是某模型不可用」算成同一个数字）。
        ['#poolCooling .hbar-row', 1]],
      lacks: [['#poolCooling', 'glm-5.3'], ['#poolCooling', 'kimi-k3-1']] },
    { nav: 'overview',
      has: [['#poolCooling .hbar-row', 1], ['#btnCreditRefresh', 1]] },
    // v0.23.3：停用明细从「冷却账号明细」里**拆出来单列**（一个会自己好、一个要人工介入）。
    // 两件事都要验：新位置有（#poolDisabled 里两种来源都显示），旧位置没有
    // （#poolCooling 里不能出现停用文案 —— 只验前者会漏掉「复制而非迁移」）。
    { nav: 'overview',
      // v0.23.4：停用明细每行加「启用 / 复活」按钮 —— 此前只能看不能操作，
      // 运维得自己回账号池几十张卡里翻出那个号。
      has: [['#poolDisabled', 1], ['#poolDisabledNote', 1], ['#poolDisabled .tag', 2],
        ['#poolDisabled .row button', 2], ['#poolCooling .hbar-row', 1]],
      textIn: [['#poolDisabled', '手动停的号'], ['#poolDisabled', '手动停用'],
        ['#poolDisabled', '被封号'], ['#poolDisabled', 'session dead'],
        // 手动停用 → 「启用」；系统禁用 → 「复活」（文案按后端 manual 选）。
        ['#poolDisabled', '启用'], ['#poolDisabled', '复活'],
        ['#poolDisabledNote', '2 个已停用']],
      lacks: [['#poolCooling', '手动停用'], ['#poolCooling', '系统禁用']] },
    { nav: 'pool', counts: [['accCards', 2], ['groupBar', 6]],
      // 仪表盘是**迁移**不是复制：账号池这一页里不能再有它的容器
      // （用 absentIn 限定作用域 —— 元素仍存在于同一文档的概览页，全局 absent 会误报）。
      absentIn: [['.pane[data-pane="pool"]', '#poolKpis'],
        ['.pane[data-pane="pool"]', '#poolHealth'],
        ['.pane[data-pane="pool"]', '#poolDashStamp'],
        ['.pane[data-pane="pool"]', '#poolTaskCover'],
        // v0.23.2：健康检测面板也迁走了（独立成页），同样不能在账号池页留容器。
        ['.pane[data-pane="pool"]', '#healthCards'],
        ['.pane[data-pane="pool"]', '#btnHealthAll']],
      has: [['#oauthBtns .oauth-btns button', 2], ['#importHint', 1], ['#btnImportDesktop', 1],
        ['#bulkConsumeGroup', 1], ['#btnBulkGroup', 1], ['#accCards .ecard footer select', 2],
        ['#accCards .ecard .tstatus', 2],
        // v0.15.0：卡片不再有「代理 / 直连」按钮（出口统一在「出口代理」页安排）。
        // v0.23.0：再加「检测」（健康检测）→ 每卡 5 个按钮。
        ['#accCards .ecard footer button', 5],
        // v0.23.2：本页只留两个健康检测入口（检测勾选 / 健康检测 →），检测动作与结果都在
        // 独立的「健康检测」页 —— 本页不再为全量检测结果留版面。
        ['#btnHealthSelPool', 1], ['#btnGoHealth', 1]],
      lacks: [['#accCards', '反代组'], ['#accCards', '未分组'],
        // v0.26.7：「被封号」那张卡没有可信挂载时刻，**整行不该出现** ——
        // 显示「已挂载 0 天」会被读成"刚挂上"，与"不知道挂了多久"相反。
        // 用 xxx 断言该账号卡的「已挂载」计数为 1（即只有第一张卡有）。
        ['#accCards', '已挂载 0 天']],
      textIn: [['#btnImportDesktop', '批量导入 JSON'], ['#accCards', '未指定'], ['#accCards', 'SOCKS'],
        // v0.15.4：每张卡有勾选框；工具栏有全选/清除/已选计数。
        ['#accCards', '未指定'],
        // v0.26.4：模型级限额要显示**具体模型**与恢复时间，并明说其它模型仍可用 ——
        // 用户口径是「6004 只挡对应模型，其他模型正常接」，界面必须能验证这一点。
        ['#accCards', '模型级限额'], ['#accCards', 'glm-5.3'], ['#accCards', '其它模型正常可用'],
        // 上游没给时间的那条要标出「本地估」（区分"上游说的"与"我们猜的"）
        ['#accCards', '本地估'],
        // v0.26.7：已挂载时长（天数 + 起始日期两段都要渲染）。
        ['#accCards', '已挂载'], ['#accCards', '已挂载 14 天']],
      has: [['#accCards .ecard .pick input', 2], ['#chkAll', 1], ['#btnClearSel', 1], ['#selCount', 1],
        ['#bulkConsumeGroup option', 4]],
      texts: [['selSummary', '反代选号']] },
    // v0.24.9：积分明细「缓存态」渲染。
    //
    // 三个不变量，缺任何一个这条修复就不成立：
    //   ① 有缓存的账号**渲染即列出积分包**（零出站拿回明细展示能力）；
    //   ② 明细之外仍保留「刷新明细」按钮（出站只能靠用户主动点，不得自动化）；
    //   ③ 没缓存的账号回落到 v0.24.3 的「快照值 · 点开查明细」——不能凭空造明细。
    { nav: 'pool',
      has: [['#accCards .credit .pack', 3],          // 只展示 remaining>0 的前 3 行
        ['#accCards .credit', 2],                    // 两张卡都有积分区块
        ['#btnWarmCredit', 1]],                      // 一键把整页明细补齐（服务端串行）
      textIn: [['#accCards .credit-packs', '4 个积分包'],
        ['#accCards .credit-sub', '近期到期'],
        ['#accCards .credit-more', '查看全部 4 个积分包'],
        ['#accCards .credit-more', '刷新明细'],
        ['#accCards .credit-upd', '更新'],
        // v0.25.0：总额取「缓存明细合计」（300+500+100+334=1,234），**不是**池内快照 4,444。
        // 两者必须来自同一份数据，否则卡片会出现"大字说 4444、明细加起来 1234"的自相矛盾。
        ['#accCards .credit-total', '1,234'],
        // ③：未缓存账号保持旧路径（同一页里两个分支都能看到）。
        ['#accCards .credit-packs', '快照值 · 点开查明细'],
        // v0.25.1：设备凭证三态必须在卡上可见（「全池都没带」这种审计结论
        // 只有显示出来才可能被发现 —— 此前它是完全静默的）。
        ['#accCards', '设备凭证'], ['#accCards', '无设备凭证']],
      // 池内快照（4,444）**不得**出现在卡片上 —— 它只用于选号/门槛等业务判断。
      lacks: [['#accCards .credit', '4,444']] },
    // v0.15.4：勾选单个卡片后，计数与「批量设置」按钮文案应随选择变化。
    // v0.23.0：健康检测面板 —— 面板 + 两个入口 + 五态结果卡片 + 处置动作可见
    // + 汇总按五态分组 + 来源标注（导入后自动）。
    // v0.23.2：面板从「账号池」页**迁到独立页**（侧边栏「健康检测」）。
    // 迁移类改动两处都要验：新页要有（且用带作用域的 has 证明「新位置」），
    // 旧页不能再有（上面 pool 那组的 absentIn）。只验其中一边都会漏掉「复制而非迁移」。
    { nav: 'health',
      has: [['.nav-item[data-nav="health"]', 1], ['.pane[data-pane="health"].active', 1],
        ['#btnHealthAll', 1], ['#btnHealthSel', 1], ['#healthSummary', 1],
        ['.pane[data-pane="health"] #healthCards .ecard', 6]],
      counts: [['healthCards', 6]],
      textIn: [['#healthSummary', '导入后自动'], ['#healthSummary', '正常 1'],
        ['#healthSummary', '异常 1'], ['#healthSummary', '自动处置 1'],
        ['#healthCards', '登录已失效'], ['#healthCards', '已禁用'],
        ['#healthCards', '被上游 WAF 拦截'], ['#healthCards', '降权中'],
        ['#healthCards', '运维手动停用'], ['#healthCards', '已同步余额'],
        // v0.24.5：探活口径变了（余额 + **真实对话** 两次），且达阈值后是**删除**。
        // 这两条是"界面必须如实说明自己会对账号做什么"，不是文案装饰 ——
        // 旧断言里的「轻量探活 / 不产生对话调用」正是被实测推翻的说法。
        ['.pane[data-pane="health"]', '两次探活'],
        ['.pane[data-pane="health"]', '极小的真实对话'],
        ['.pane[data-pane="health"]', '11140 请求非法连续 3 次后删除'],
        // 导航项本身要能被认出来（独立入口 = 侧边栏有这一项）
        ['.nav-item[data-nav="health"]', '健康检测'],
        // 本页没有账号列表：挑号仍回账号池页勾选（勾选跨页保留），这一点要写明白
        ['.pane[data-pane="health"]', '勾选状态跨页保留']] },
    // v0.23.4：检测结果卡带「一键动作」按钮 —— 后端给结构化 action（revive/enable），
    // 前端不匹配中文原因文案（文案会改，匹配会静默失效）。
    // 6 条结果里只有 2 条带 action（曾被系统禁用 → 复活；手动停用 → 启用），
    // 其余 4 条（异常/不确定/正常/仅冷却降权）刻意不给按钮 —— 一键解决不了它们。
    { nav: 'health',
      has: [['#healthCards .action-row button', 2]],
      textIn: [['#healthCards .action-row', '复活'], ['#healthCards .action-row', '启用'],
        ['#healthCards', '误判禁用号'], ['#healthCards', '曾被系统禁用']] },
    // v0.23.2：账号池页的「健康检测 →」是纯跳转 —— 点完应当停在健康检测页
    // （页面切换靠 .active 类，不是内联 display，所以用带 .active 的选择器断言）。
    { nav: 'pool', click: ['#btnGoHealth'],
      has: [['.pane[data-pane="health"].active', 1], ['.nav-item[data-nav="health"].active', 1]],
      // 且账号池页必须**让出 active**（页面切换靠 .active 类，不是内联 display，
      // 所以不能用 display 断言；absent 正好是「必须为 0 个」的语义）。
      absent: ['.pane[data-pane="pool"].active'] },
    // v0.23.3：账号池页新增「状态筛选 + 关键词搜索」。
    // 分组 chip 回答「参不参与消耗」，这两样回答「哪个在冷却 / 哪些没积分 /
    // 那个昵称叫什么」—— 都是分组答不了的问题。
    { nav: 'pool',
      // v0.26.5：新增第 9 档「限模」（模型级冷却）。
      has: [['#accStateFilter .seg', 9], ['#accQuery', 1], ['#accFilterHint', 1]],
      textIn: [['#accStateFilter', '限模']],
      counts: [['accCards', 2]] },
    // 状态筛选：点「已停用」→ 只剩那张停用卡（计数由卡片数体现）。
    { nav: 'pool', clickText: [['#accStateFilter .seg', '已停用']],
      counts: [['accCards', 1]],
      textIn: [['#accCards', '被封号'], ['#accFilterHint', '状态：已停用'],
        ['#accFilterHint', '命中 1 个']],
      lacks: [['#accCards', '测试账号']] },
    // v0.26.5：「限模」是**正交**维度 —— 该号有模型级冷却，但它对其它模型仍可用，
    // 所以它同时命中「可用」与「限模」。这不是矛盾，正是 6004 语义的正确表达。
    { nav: 'pool', clickText: [['#accStateFilter .seg', '限模']],
      counts: [['accCards', 1]],
      textIn: [['#accCards', '测试账号'], ['#accFilterHint', '状态：限模'],
        ['#accCards', '模型级限额'], ['#accCards', 'glm-5.3'],
        // 必须有这句：只给一个"限模×2"的计数，运维没法判断是哪些模型、什么时候恢复。
        ['#accCards', '其它模型正常可用']],
      lacks: [['#accCards', '被封号']] },
    // 限模的号**仍然算可用**（模型级冷却不吃账号可用性）——这是最关键的一条口径断言：
    // 若哪天有人"顺手"把它并进 cooling/off，这条会立刻红。
    { nav: 'pool', clickText: [['#accStateFilter .seg', '可用']],
      counts: [['accCards', 1]],
      textIn: [['#accCards', '测试账号'], ['#accFilterHint', '状态：可用']] },
    // 同一按钮再点一次不切回全部（分段控件语义）；切回「全部」恢复 2 张。
    { nav: 'pool', clickText: [['#accStateFilter .seg', '全部']],
      counts: [['accCards', 2]] },
    // 关键词搜索：昵称子串命中 1 张。
    { nav: 'pool', input: [['accQuery', '测试']],
      counts: [['accCards', 1]],
      textIn: [['#accFilterHint', '关键词：测试'], ['#accFilterHint', '命中 1 个']],
      lacks: [['#accCards', '被封号']] },
    // 搜不到任何账号时空态要说清是「筛掉了」而不是「号池空了」。
    // 注意不能用 counts=0：空态会插入一句提示（占一个子元素），
    // 该断言只能靠「卡片元素消失 + 文案正确」来表达。
    { nav: 'pool', input: [['accQuery', '不存在的号']],
      absent: ['#accCards .ecard'],
      textIn: [['#accCards', '没有账号同时匹配']] },
    // 清空关键词后恢复（顺带验证 input 清空也生效）。
    { nav: 'pool', input: [['accQuery', '']],
      counts: [['accCards', 2]] },
    { nav: 'pool', click: ['#accCards .ecard .pick input'],
      textIn: [['#btnBulkGroup', '已选 1']],
      texts: [['selCount', '已选 1']] },
    // v0.15.4：全选当前筛选（2 张卡）→ 已选 2；再点一次取消 → 计数清空。
    { nav: 'pool', click: ['#chkAll'],
      textIn: [['#btnBulkGroup', '已选 2']],
      texts: [['selCount', '已选 2']] },
    // v0.15.3：批量导入 JSON 弹窗（点按钮打开后断言弹窗内控件与说明文案）。
    { click: ['#btnImportDesktop'],
      has: [['.modal .import-ta', 1], ['.modal .import-adv', 1],
        ['.modal input[type=file][multiple]', 1], ['.modal button', 2]],
      textIn: [['.modal', '批量导入账号'], ['.modal', '服务端本地路径']] },
    // v0.26.7：代挂卡片的「已挂载 N 天」标签（号主最常问的问题）。
    { nav: 'host', counts: [['hostPending', 1], ['hostCards', 1], ['hostGroupBar', 2]], texts: [['hostSummary', '代挂账号']],
      // v0.24.2（P3-13）：hostStateTag 已下沉 app.js —— 共享实现仍要能渲染出状态标签，
      // 「删掉了」与「还能用」两件事必须都验（只验其一会漏掉「删过头」）。
      textIn: [['#hostCards', '已入池']] },
    // v0.14.0：出口代理页（线路池 + 防封体检 + 批量分配）。
    // v0.17.9：「出口代理」改名为「代理」，页内分 出口代理 / 账号代理 / 去哪弄代理 三子区。
    { nav: 'egress', counts: [['egrCards', 1]],
      has: [['#egressAlert', 1], ['#egressBadge', 1], ['#egrName', 1], ['#egrUrl', 1], ['#egrRegion', 1],
        ['#btnEgrSave', 1], ['#btnEgrCheckAll', 1], ['#egrScope', 1], ['#egrMaxPer', 1], ['#egrIsolate', 1],
        ['#btnEgrDry', 1], ['#btnEgrApply', 1], ['#egrPlan', 1], ['#egrCards .ecard footer button', 4],
        // v0.14.1：支持直连 —— 批量「设为直连 / 清空出口」两个按钮。
        ['#btnEgrDirect', 1], ['#btnEgrClear', 1],
        // 三个子区 + 子区导航，且线路池/账号代理/帮助三块都在同一文档里。
        ['#egrTabs .seg', 4], ['[data-egr-sub="pool"]', 1],
        ['[data-egr-sub="accounts"]', 1], ['[data-egr-sub="device"]', 1],
        ['[data-egr-sub="help"]', 1]],
      textIn: [['#egressAlert', '出口异常'], ['#egrCards', '香港 01'], ['#egrCards', '203.0.113.7'],
        ['#egrCards', '探活'], ['#egressAlert', '显式直连'],
        // 导航改名（旧名不能再出现）
        ['.nav-item[data-nav="egress"]', '代理'],
        // 帮助子区：讲清「去哪弄代理」，且明确不背书
        ['#egrTabs', '去哪弄代理'], ['[data-egr-sub="help"]', '先记一条硬约束'], ['[data-egr-sub="help"]', '自建'],
        // 硬约束必须写出来：上游是国内域名 → 海外出口反而异常（与通用经验相反，最容易买错）
        ['[data-egr-sub="help"]', '不要买海外住宅代理'], ['[data-egr-sub="help"]', '国内独享'],
        ['[data-egr-sub="help"]', '不为任何服务商背书'],
        // 随包验证工具与实测结论（避免管理员被服务商宣传带跑）
        ['[data-egr-sub="help"]', 'tools/proxyscan'], ['[data-egr-sub="help"]', '免费代理测试报告.md']],
      lacks: [['.nav-item[data-nav="egress"]', '出口代理']],
      // 默认停在「出口代理」子区：公共层先给线路池，账号层按需切。
      display: [['[data-egr-sub="pool"]', 'shown'], ['[data-egr-sub="accounts"]', 'none'],
        ['[data-egr-sub="device"]', 'none'], ['[data-egr-sub="help"]', 'none']] },
    // 切到「账号代理」：KPI 6 块 + 5 张账号卡 + 筛选段控件，且 pool 子区被藏起。
    { clickText: [['#egrTabs .seg', '账号代理']],
      counts: [['egrAccKpis', 6], ['egrAccCards', 5]],
      has: [['#egrAccFilter .seg', 6], ['#egrAccQuery', 1], ['#egrAccCards .ecard footer button', 15]],
      display: [['[data-egr-sub="accounts"]', 'shown'], ['[data-egr-sub="pool"]', 'none']],
      textIn: [['#egrAccKpis', '配置异常'], ['#egrAccKpis', '未指定 · 直连'],
        ['#egrAccCards', '香港 01'], ['#egrAccCards', '本机 IP 直连'],
        ['#egrAccCards', '继承默认'], ['#egrAccCards', '配置值'], ['#egrAccCards', '实际出口']] },
    // 帮助子区可切换到达。
    { clickText: [['#egrTabs .seg', '去哪弄代理']],
      has: [['[data-egr-sub="help"] .egr-help-table', 1], ['[data-egr-sub="help"] .egr-help-list', 4]],
      display: [['[data-egr-sub="help"]', 'shown'], ['[data-egr-sub="accounts"]', 'none']],
      textIn: [['[data-egr-sub="help"]', '方案 A：自建'], ['[data-egr-sub="help"]', '3proxy'],
        ['[data-egr-sub="help"]', '方案 B：买商业代理'], ['[data-egr-sub="help"]', '合规'],
        ['[data-egr-sub="help"]', '别一次全量切过去']] },
    // v0.26.3：消耗失败台账（统计 + 明细 + 处置策略表三块）。
    { nav: 'report',
      has: [['#cfHours', 1], ['#cfCode', 1], ['#cfStage', 1], ['#btnCfRefresh', 1],
        ['#cfStats', 1], ['#cfRows', 1], ['#cfPolicies', 1],
        // 统计表 + 明细表 + 策略表 = 3 张
        ['.pane[data-pane="report"] .egr-help-table', 3]],
      textIn: [
        ['#cfStats', '失败总数'], ['#cfStats', '9'], ['#cfStats', '只挡该 (账号, 模型)'],
        // 未声明处置的码必须显式标出来 —— 否则运维会以为是"没问题"
        ['#cfStats', '未声明处置'],
        // 四个阶段必须分开标签（预扣是用户侧、上游是账号侧，处置完全不同）
        ['#cfStage', '预扣'], ['#cfStage', '上游'], ['#cfStage', '结算'], ['#cfStage', '扣费'],
        // 明细里要能看到错误码与重置来源（后者区分"上游说的"与"本地估的"）
        ['#cfRows', '6004'], ['#cfRows', 'body-text'], ['#cfRows', '预扣被拒'],
        // 策略表整体回显（含未命中的码，便于区分"没出现"与"没声明"）
        ['#cfPolicies', '11140'], ['#cfPolicies', '需人工']] },
    // v0.26.1：新增「设备与选号诊断」子区 —— 设备凭证共用检测 + 均衡度 + 候选队列。
    { clickText: [['#egrTabs .seg', '设备与选号诊断']],
      has: [['[data-egr-sub="device"]', 1], ['#devTokSummary', 1], ['#devTokRows', 1],
        ['#devTokText', 1], ['#btnDevTokImport', 1], ['#selBalance', 1], ['#selCands', 1], ['#fpSamples', 1],
        // v0.28.0：指纹规则面板（自定义可增删 / 内置只读 / 试跑 / 样本回放）。
        ['#fpRulesCustom', 1], ['#fpRulesBuiltin', 1], ['#fpRuleKpis', 1],
        ['#fpRuleKind', 1], ['#fpRuleMatch', 1], ['#fpRuleReplace', 1], ['#fpRuleNote', 1],
        ['#btnFpRuleSave', 1], ['#btnFpRuleTest', 1], ['#btnFpRuleReplay', 1],
        ['#fpRuleTestText', 1], ['#fpRuleTestOut', 1], ['#fpRuleReplayOut', 1],
        ['#fpRuleKind option', 2], ['#fpRuleKpis .stat-card', 3],
        // 四张表：凭证逐号、均衡度 Top、候选队列、内置规则（自定义为空时是空态块）
        ['[data-egr-sub="device"] .egr-help-table', 4],
        // 自定义为空：必须是「空态提示」而不是一张空表（后者看着像加载失败）
        ['#fpRulesCustom .feed-empty', 1]],
      // 内置规则**不给**删除按钮（它来自代码，删不掉）—— 给了按钮才是缺陷，
      // 所以这条必须用 absent（「必须为 0 个」）而不是 has（`>=0` 永远成立）。
      absent: ['#fpRulesBuiltin [data-fp-del]'],
      display: [['[data-egr-sub="device"]', 'shown'], ['[data-egr-sub="pool"]', 'none']],
      textIn: [['#devTokSummary', '真凭证 1'], ['#devTokSummary', '无凭证 1'],
        // v0.26.3：五态必须分别可见（stale 与 derived 的处置方向相反，
        // 合成一个"有凭证 N 个"会把它们混掉）。
        ['#devTokSummary', '真凭证·陈旧 1'], ['#devTokSummary', '派生兜底 1'],
        ['#devTokSummary', '派生兜底 已开'], ['#devTokSummary', '陈旧阈值 30 天'],
        // 均衡度：三个关键数字 + 结论
        ['#selBalance', '23/27'], ['#selBalance', '7.9%'], ['#selBalance', '2.13'],
        // 候选队列：进候选者的权重排名 + 落选者的原因（本功能的全部意义就在这两列）
        ['#selCands', '#1'], ['#selCands', 'Top5'], ['#selCands', '账号级冷却中'],
        ['#selCands', '落选'],
        // 样本开关关着时必须明说"未开启"：显示"0 条"会让人误以为"没被拦过"
        ['#fpSamples', '未开启'],
        // v0.28.0：指纹规则面板 —— 内置只读要能看见、自定义为空要有引导、
        // 条数上限要显示（否则运维不知道"还能加多少"）。
        ['#fpRuleKpis', '自定义规则'], ['#fpRuleKpis', '0 / 200'], ['#fpRuleKpis', '内置规则'],
        ['#fpRulesBuiltin', '内置'], ['#fpRulesBuiltin', 'regex'],
        ['#fpRulesCustom', '还没有自定义规则'],
        ['#fpRuleKind', '正则替换'], ['#fpRuleKind', '逐字替换']],
      // 凭证原文绝不出现（接口只回指纹与长度）
      lacks: [['[data-egr-sub="device"]', 'AUTHTOKEN']] },
    // v0.26.5：样本面板的**启用态** —— 关着时测的是"必须明说未开启"，
    // 这里测它真的把容量 / 分布 / 明细画出来了（两态都要钉住，缺一即为漏测）。
    { fx: [['/api/admin/fingerprint-samples', {
        enabled: true, file: 'data/fingerprint_samples.jsonl', count: 2, full: false,
        size: 1048576, max: 2097152, lines: 3, last_at: 1791972000,
        kinds: { content_blocked: 1, channel_denied: 1 },
        models: [{ name: 'glm-5.3', count: 2 }],
        samples: [
          { at: 1791972000, kind: 'content_blocked', model: 'glm-5.3', key: 'k1',
            system: 'You are Claude Code, Anthropic official CLI', tools: ['bash'] },
          { at: 1791971000, kind: 'channel_denied', model: 'deepseek-v4-pro', key: 'k2',
            system: 'Main branch (', tools: ['read_file'] },
        ],
      }]],
      clickText: [['#egrTabs .seg', '设备与选号诊断']],
      has: [['#fpSampleKpis .stat-card', 5], ['#fpSampleDist', 1],
        ['#btnFpSampleRefresh', 1], ['#btnFpSampleClear', 1],
        ['#fpSamples .egr-help-table', 1], ['[data-fp-copy]', 2],
        // 两条横条分布（类型 + 模型）各自成组
        ['#fpSampleDist .progress', 1]],
      textIn: [
        ['#fpSampleKpis', '去重样本'], ['#fpSampleKpis', '1.00 MB / 2.00 MB'],
        ['#fpSampleKpis', '50%'],
        ['#fpSampleDist', '按拦截类型'], ['#fpSampleDist', 'content_blocked'],
        ['#fpSampleDist', '按模型'], ['#fpSampleDist', 'glm-5.3'],
        ['#fpSamples', 'You are Claude Code'], ['#fpSamples', '复制全文'],
        // v0.28.0：样本行必须能"采纳为规则"（采集 → 处置的闭环入口）。
        ['#fpSamples', '采纳为规则'],
      ] },
    // v0.28.0：规则面板的**有自定义规则**态 —— 关着时测"空态引导"，
    // 这里测真的把表 / 命中列 / 删除按钮画出来了（两态都要钉，缺一即为漏测）。
    { fx: [['/api/admin/fingerprint-rules', {
        max: 200,
        builtin: [
          { id: 'fp-builtin-ident', kind: 'literal', match: 'You are Claude Code, Anthropic official CLI',
            replace: 'You are Claude Code, Anthropic official CLI tool',
            note: '内置（编译期常量，只读）', source: 'builtin', builtin: true },
        ],
        custom: [
          { id: 'fp-aaaabbbbcccc', kind: 'literal', match: 'CUSTOM-TOKEN-HERE',
            replace: 'CUSTOM TOKEN HERE', note: '来自 10-15 样本', source: 'sample', added_at: 1791972000 },
          { id: 'fp-ddddeeeeffff', kind: 'regex', match: 'trace_[0-9a-f]+;?',
            replace: '', note: '整段删除', source: 'manual', added_at: 1791972001 },
        ],
      }]],
      clickText: [['#egrTabs .seg', '设备与选号诊断']],
      has: [['#fpRulesCustom .egr-help-table', 1], ['#fpRulesBuiltin .egr-help-table', 1],
        ['#fpRulesCustom [data-fp-del]', 2], ['#fpRulesCustom [data-fp-hits]', 2]],
      // 内置表**不得**有删除按钮（两态都要验，否则"只在内置为空时才隐藏"这种
      // 假实现会漏过）。
      absent: ['#fpRulesBuiltin [data-fp-del]', '#fpRulesCustom .feed-empty'],
      textIn: [['#fpRuleKpis', '2 / 200'], ['#fpRulesCustom', 'CUSTOM-TOKEN-HERE'],
        ['#fpRuleKpis', '逐字 → 正则'],
        // 正则规则替换串为空时必须显示「（删除）」而不是空白格 ——
        // 空白会让人以为"没填"，而它的语义恰好相反。
        ['#fpRulesCustom', '（删除）'],
        // 命中数占位：回放前显示「—」而不是 0（"没评估"≠"一条没命中"）。
        ['#fpRulesCustom', '—']] },
    // R1：线路失效处置策略 = 面板可读可改（默认「不静默换出口」）
    { nav: 'egress',
      has: [['#egrOnLineDown', 1], ['#btnEgrOnLineDown', 1], ['#egrOnLineDown option', 2]],
      textIn: [['#egrOnLineDown', '不静默换出口'], ['#egrOnLineDown', '自动迁移到可用线路']] },
    // 设置页新增出口池参数（上限 / 抖动）。
    { nav: 'settings', has: [['#setEgressMaxPer', 1], ['#setEgressJitter', 1], ['#setEgress', 1],
        // v0.26.1：两个默认关闭的开关（R6 降档 / R4 样本回流）
        ['#setPickSink', 1], ['#setFpSamples', 1],
        // v0.26.5：样本面板的跨页入口 + 实时条数；更新提醒的导航徽标容器
        ['#btnFpGo', 1], ['#fpCfgCount', 1], ['#updBadge', 1],
        // v0.29.0：提现审核方式（下拉 2 项）+ 免审额度上限
        ['#setWithdrawMode', 1], ['#setWithdrawMode option', 2],
        ['#setWithdrawAutoMax', 1], ['#wdModeHint', 1],
        ['#setPickSink option', 2], ['#setFpSamples option', 2],
        // v0.26.3：设备凭证的派生兜底与陈旧阈值
        ['#setDevTokFallback', 1], ['#setDevTokStaleDays', 1],
        ['#setDevTokFallback option', 2],
        // v0.15.1：选号模式下拉（加权随机 / 顺序轮转）。
        ['#setPickMode', 1], ['#setPickMode option', 2]],
      textIn: [['#setWithdrawMode', '人工审核'], ['#setWithdrawMode', '自动审核（提交即通过并打款）'],
        ['#wdModeHint', '当前：人工审核'],
        ['#setPickMode', '顺序轮转'], ['#setPickMode', '加权随机'],
        ['#setPickSink', '关闭（默认）'], ['#setFpSamples', '关闭（默认）'],
        ['#setFpSamples ~ .hint', '唯一会把请求内容写到磁盘'],
        ['#setDevTokFallback ~ .hint', '上游 SDK 签发的真凭证']] },
    // v0.17.0：成长任务移出定时任务页（改为手动选号执行）—— taskGrid 回到 6 张，
    // 且不应再有 growth 的时段/开关/批次输入；提示框改为「手动选号」口径。
    // v0.17.1：新增独立「成长任务」侧边栏。
    // v0.17.4：调度状态条 + 运行中徽标 + 下次触发时刻 + 自动刷新开关。
    // 这些断言锁死「定时有效可见」与「任务跑完会自己出现」两件事。
    // v0.27.0：开学季任务整体移除 → 任务卡从 6 张降为 5 张。
    { nav: 'tasks', counts: [['taskGrid', 5], ['taskKinds', 2], ['runCards', 2], ['schedBar', 4]],
      has: [['#tasksAuto', 1], ['#schedStamp', 1],
        // checkin 在 fixture 里是 running → 其按钮被禁用并显示「执行中：」。
        ['#taskKinds .action-card.running', 1], ['#runCards .run-card.running', 1],
        ['#runCards .tag.pulse', 1]],
      // 成长任务已移出排程：既不该有它的任务卡/时段/开关，也不该有每批上限输入。
      absent: ['#tcGrowthBatch', '#tc_growth_hours', '#tc_growth_off', '#taskcard_growth',
        // v0.27.0：开学季（活动已结束）已整体移除，不留任何入口。
        '#taskcard_school', '#tc_school_hours', '#tc_school_off'],
      // v0.17.2：台账卡片分两个维度显示 —— 「自动排程 · 9/21 点」（任务性质）
      // 与「本轮 · 人工」（fixture 里 checkin 是 manual 触发）。
      // lacks 断言旧标签「手动」已消失：签到明明是自动任务，之前只挂一个孤立
      // 的「手动」标签，会被直接读成「签到是手动的」。
      // v0.29.0：台账里**残留**的 school 记录也不得进界面 —— fixture 的 runs 里
      // **刻意塞了一条**，因此这里查 'school' 文案就是在证明"那张卡片没被画出来"
      // （卡片标题取 TASK_LABELS[kind] || kind → 若没过滤，就会渲染成裸 "school"）。
      lacks: [['#taskGrid', '成长任务'], ['#runCards', '手动'], ['#runCards', 'school']],
      textIn: [['#growthNoteBox', '手动选号'], ['#growthNoteBox', '零消耗'],
        // 签到：自动排程 + 本轮人工（两个维度并存）
        ['#runCards', '自动排程 · 9/21 点'], ['#runCards', '本轮 · 人工'],
        // 夜猫子：自动排程 + 本轮定时
        ['#runCards', '自动排程 · 1 点'], ['#runCards', '本轮 · 定时'],
        // v0.17.4：运行态与下次触发时刻可见
        ['#schedBar', '执行中 1 项'], ['#schedBar', '最近一次'],
        ['#taskKinds', '执行中：'],
        ['#runCards', '新一轮执行中'], ['#runCards', '下次'],
        // 「上次自动触发」那一行：即使台账里 checkin 是 manual，也能看到定时那轮的结果
        ['#runCards', '上次自动触发'], ['#runCards', '定时 · 成功 18']] },
    // v0.17.1：独立「成长任务」侧边栏（管理后台）——账号进度卡 + 选号执行 + 进度条。
    { nav: 'growth', counts: [['gCards', 2]],
      has: [['#gAll', 1], ['#gBtnClear', 1], ['#gBtnQuery', 1], ['#gBtnForceQuery', 1], ['#gBtnRun', 1],
        ['#gCards .ecard', 2], ['#gCards .ecard .pick input', 2], ['#gCards .progress', 1]],
      textIn: [['#gCards', '自动项 15/16'], ['#gCards', '轻量云专家'], ['#gCards', '未完成'],
        ['#gCards', '非自动'], ['#gCards', '已停用']] },
    // 勾选第一个可执行账号 → 按钮文案与已选计数随选择变化（另一张是停用号，勾选框被禁用）。
    { nav: 'growth', click: ['#gCards .ecard .pick input'],
      textIn: [['#gBtnRun', '已选 1']],
      texts: [['gSelCount', '已选 1']],
      has: [['#gCards .ecard .pick input:disabled', 1]] },
    // v0.19.0：账号市场（管理端）——在售 / 可上架候选 / 买断台账三块。
    { nav: 'market',
      texts: [['marketSummary', '在售 1 个']],
      has: [['#marketOnSale .ecard', 1], ['#marketListable .ecard', 1],
        ['#marketOnSale .ecard button', 1], ['#marketListable .ecard button', 1]],
      textIn: [['#marketOnSale', '在售号'], ['#marketOnSale', '闲置'],
        ['#marketOnSale', '挂牌价'],
        ['#marketListable', '候选号'], ['#marketPurchases', '平台'],
        ['#marketPurchases', '300']] },
    // v0.12.0：用户搜索（输入 test → 只剩 tester 一张卡）。
    { nav: 'users', counts: [['userCards', 2]] },
    { input: [['userSearch', 'test']], counts: [['userCards', 1]], texts: [['userSummary', '匹配 1']] },
    { input: [['userSearch', 'zzzz']], counts: [['userCards', 1]], texts: [['userSummary', '匹配 0']] },
    { input: [['userSearch', '']], counts: [['userCards', 2]] },
    // v0.12.0：兑换码卡片每条带单个复制按钮。
    { nav: 'codes', counts: [['codeCards', 2]], has: [['#codeCards .code-name .icon-btn', 2]] },
    // v0.29.0：提现审核页 —— 审核方式必须可见、待审卡片必须**有审核按钮**、
    // 状态必须是中文（这三件事此前全部失效，原因见 handleAdminWithdrawals 的注释：
    // 列表接口内嵌无 json tag 的 *Withdrawal → status 为 undefined → 按钮不渲染）。
    { nav: 'withdrawals', counts: [['wdKpis', 6], ['wdCards', 2], ['wdFilter', 1]],
      has: [['#wdCards .ecard footer button', 2], ['#wdModeBar', 1], ['#wdConsistency', 1],
        // 一致时**不显示**告警横幅（常驻的"一切正常"提示几天后就会被无视）。
        ['#wdConsistency .hintbar', 0]],
      absent: ['#wdConsistency .hintbar'],
      texts: [['wdBadge', '1']],
      textIn: [['#wdModeBar', '人工审核'], ['#wdModeBar', '逐笔通过或驳回'],
        // 流程说明（静态文案）必须把"通过=成功+从冻结额出账"与"驳回=原路退回"写清楚 ——
        // 这正是用户第二问要的"明确审核流程"。
        ['.pane[data-pane="withdrawals"]', '已从可提现额度划入冻结额'],
        ['.pane[data-pane="withdrawals"]', '从'],
        ['.pane[data-pane="withdrawals"]', '原路退回到可提现额度'],
        ['#wdKpis', '审核方式'], ['#wdKpis', '人工审核'],
        // 状态中文来自服务端下发的 status_label（本地映射仅兜底）
        ['#wdCards', '待审核'], ['#wdCards', '已打款'],
        // 审核按钮的文案就是"通过"要做什么
        ['#wdCards', '通过并打款'],
        // 管理端收款信息必须是明文（打码就没法照着打款）
        ['#wdCards', 'tester@example.com']],
      // 旧桩里的 '微信' 已随「仅支付宝」口径移除；泄漏收款信息的打码形态也不该出现。
      lacks: [['#wdCards', 'undefined']] },
    // v0.29.0：自动审核 + 账目不一致 —— 两个告警都要显式可见。
    { nav: 'withdrawals',
      fx: [['/api/admin/withdrawals', {
        pending_count: 1,
        status_counts: { pending: 1, approved: 0, rejected: 0 },
        consistency: { ok: 0, frozen_micro: 0, pending_micro: 500000, pending_count: 1,
          overdraw_users: 1, unreviewed_done: 2, frozen_pending_diff: -500000 },
        params: { rate: 0.01, min_points: 100, review_mode: 'auto',
          review_mode_label: '自动审核', auto_max_points: 500, auto_max_cny: 5, auto_unlimited: false },
        withdrawals: [
          { id: 9, user_id: 2, username: 'tester', amount: 100, fee: 0, cny: 1,
            method: 'alipay', method_label: '支付宝',
            alipay_account: 'auto@example.com', real_name: '自动者',
            account_info: 'auto@example.com / 自动者',
            status: 'approved', status_label: '已打款', note: '自动审核放行（免审额度内）',
            reviewed_at: now, reviewed_by: 0, reviewer_name: '', created_at: now, auto: true },
        ],
      }]],
      has: [['#wdConsistency .hintbar', 1]],
      textIn: [['#wdModeBar', '自动审核'], ['#wdModeBar', '免审'],
        ['#wdKpis', '自动审核'],
        // 自动放行的记录必须能一眼看出来（谁放的款）
        ['#wdCards', '自动审核'], ['#wdCards', '系统自动'],
        // 账目不一致必须喊出来，并给出可核对的数字
        ['#wdConsistency', '一致性自检未通过'],
        ['#wdConsistency', '超过了余额'],
        ['#wdConsistency', '审核时间为空']] },
    // v0.26.7：性能与前缀缓存面板（耗时 + 命中率）。三档耗时形态各断言一个，
    // 避免"数字渲染对了但单位错了"这类不会被计数断言抓到的问题。
    { nav: 'report',
      has: [['#perfKpis .kpi-card', 3]],
      textIn: [['#perfKpis', '平均耗时'], ['#perfKpis', 'P95 耗时'], ['#perfKpis', '缓存命中率'],
        ['#perfKpis', '1 分 39 秒'], ['#perfKpis', '99.4'], ['#perfKpis', '2 分 6 秒'],
        ['#perfNote', '耗时样本 40 条'], ['#perfNote', 'token 加权']],
      // 空态提示语不得出现（这是"有数据"的用例）。
      lacks: [['#perfNote', '还没有性能观测']] },
    // 空态：本窗口没有任何观测时**必须明说"没有观测"，不许画成 0**。
    // 把命中率画成 0% 会被读成"缓存完全没生效"，与"还没采到"的下一步动作相反。
    { nav: 'report',
      fx: [['/api/admin/usage', {
        days: 14, totals: { requests: 5, tokens: 100, charged: 0.1 },
        daily: [{ day: '2026-10-09', requests: 5, tokens: 100, charged: 0.1 }],
        models: [{ model: 'm', requests: 5, tokens: 100, charged: 0.1 }],
        perf: { latency_sampled: 0, cache_sampled: 0, avg_latency_ms: 0,
          p50_latency_ms: 0, p95_latency_ms: 0, max_latency_ms: 0, cache_hit_rate: 0 },
      }]],
      textIn: [['#perfKpis', '—'], ['#perfNote', '还没有性能观测'],
        ['#perfNote', '显示为"—"而不是 0']],
      lacks: [['#perfKpis', '0.0']] },

    { nav: 'report', counts: [['reportTotals', 4], ['reportChart', 1], ['reportModelBars', 1], ['reportModelTokens', 1], ['reportDailyTable', 1], ['rpBars', 60]],
      // v0.17.8：用户消耗排行（横向条 + 明细表；按消耗点数降序）。
      has: [['#reportUserRank .hbar-row', 2], ['#reportUserRank table', 1], ['#rankLimit', 1], ['#btnRankRefresh', 1]],
      textIn: [['#reportUserRank', 'alice'], ['#reportUserRank', 'tester'], ['#reportUserRank', '按消耗点数降序']] },
    // v0.17.8：公告管理页（发布表单 + 列表 + 启停/编辑/删除）。
    // v0.22.0：有效期控件（起止时间 + 快捷时长 + 清空）、状态标签、时间窗、
    //          摘要按四态分组。
    { nav: 'announce', counts: [['annCards', 2]],
      has: [['#annTitle', 1], ['#annContent', 1], ['#annLevel', 1], ['#annPinned', 1],
        // 有效期三件套
        ['#annStartAt', 1], ['#annEndAt', 1], ['#annDuration', 1],
        ['#btnAnnApplyDuration', 1], ['#btnAnnClearWindow', 1], ['#annWindowPreview', 1],
        ['#btnAnnCreate', 1], ['#annCards .ecard', 2], ['#annCards .ecard footer button', 4]],
      textIn: [['#annCards', '维护通知'], ['#annCards', '生效中'],
        // 过期公告仍在管理端，且被标成「已过期」——这是「到期仅隐藏、不改数据」的可见证据
        ['#annCards', '限时活动'], ['#annCards', '已过期'],
        ['#annCards', '长期有效'], ['#annCards', '📌 置顶'],
        // 摘要按四态分组
        ['#annSummary', '生效 1'], ['#annSummary', '已过期 1'],
        ['.pane[data-pane="announce"]', '总数上限 100 条'],
        ['.pane[data-pane="announce"]', '时效优先于置顶'],
        // 空表单下的默认提示
        ['#annWindowPreview', '长期有效']] },
    { nav: 'settings', texts: [['setProxyGroups', '反代'], ['hostShareHint', '推广返点'], ['withdrawHint', '最小提现']] },

    // ================================================================
    // v0.27.0 · 后台：调价历史（含调整人）+ 调价说明输入
    // ================================================================
    { nav: 'settings',
      has: [['#setMultNote', 1], ['#multHistBox', 1], ['#setMultOld', 1],
        ['#multHistBox .feed .ev', 2]],
      textIn: [['#multHistBox', 'admin'], ['#multHistBox', '1.5 → 1.2'],
        ['#multHistBox', '1 → 1.5'], ['#multHistBox', '算力成本回落'],
        // ⚠ 未发出公告的那条必须显式标出来：用户收不到通知，管理端看不见就没人知道。
        ['#multHistBox', '未发出公告'],
        ['#multHistBox', '建议手工在「公告」页补一条']] },

    // v0.26.7：更新检测与自动下载面板。
    { nav: 'settings',
      has: [['#setUpdateAuto', 1], ['#btnApplyUpdate', 1],
        ['#updateStatus .upd-notes', 1],
        // 简介按行渲染：给的是 3 行 notes，就必须渲染成 3 个 .upd-line
        // （整段塞一个 div 会让"有几条改动"看不出来）。
        ['#updateStatus .upd-line', 3],
        // 产物已就绪：徽标 + 应用按钮可见
        ['#updateStatus .tag', 1], ['#updateStatus .upd-dl', 1],
        ['#updateStatus .upd-hint', 1]],
      textIn: [['#updateStatus', '功能更新'], ['#updateStatus', '0.26.7'],
        ['#updateStatus', '① 补齐耗时与缓存观测'],
        ['#updateStatus', '产物已就绪'],
        ['#updateStatus', 'SHA-256 校验通过'],
        ['#updateStatus', 'apply-update.bat']],
      // 已就绪时应用按钮应可见（display 不为 none —— 由 display 断言覆盖）
      display: [['#btnApplyUpdate', true]] },

    // ================================================================
    // v0.26.6 · 共享 UI 套件（对照 workbuddy-manager 的交互调研结论）
    // ================================================================

    // ① 顶栏有一个**可点的**搜索入口：快捷键只对已经知道它的人有用，
    //    这一批的目标是"可发现性"，所以入口必须是可见控件。
    { nav: 'overview',
      has: [['#btnPalette', 1], ['#btnPalette .pal-kbd', 1], ['#navFilter', 1]],
      textIn: [['#btnPalette', '搜索']] },

    // ② ⌘K / Ctrl+K 打开命令面板；面板列出页面（单一事实源：菜单即清单）。
    { nav: 'overview', press: [[null, 'k', 'ctrl']],
      has: [['#paletteBack', 1], ['#palInput', 1], ['#palList', 1],
        ['#palList .pal-item', 15],
        // 每项右侧带归属分组：光看名字「用户」分不清是页面还是数据行。
        ['#palList .pal-gp', 15]],
      textIn: [['#palList', '账号池'], ['#palList', '运行'], ['#palList', '系统']] },

    // ③ 输入关键词过滤 + ↑↓ 移动 + Enter 打开（键盘全程可用）。
    //    搜「代理」应命中「代理」页，回车后跳到该页。
    { input: [['palInput', '代理']],
      has: [['#palList .pal-item', 1]],
      textIn: [['#palList', '代理']] },
    { press: [['#palInput', 'Enter']],
      // 回车后：面板关闭 + 已经切到代理页
      has: [['.pane[data-pane="egress"].active', 1]],
      absent: ['#paletteBack'] },

    // ④ Esc 关闭（不跳转）
    { press: [[null, 'k', 'ctrl']], has: [['#paletteBack', 1]] },
    { press: [['#palInput', 'Escape']], absent: ['#paletteBack'] },

    // ⑤ 搜不到时给的是"空态"而不是一片空白（要能分清"打错了"与"确实没有"）
    { press: [[null, 'k', 'ctrl']], has: [['#paletteBack', 1]] },
    // 输入是**异步过滤**（动态源要打接口），所以过滤结果放在下一步断言 ——
    // 同一步里断言会读到"还没过滤完"的 DOM（这个坑本轮踩过一次）。
    { input: [['palInput', 'zzzz不存在zzzz']] },
    { has: [['.pal-empty', 1]],
      textIn: [['.pal-empty', '没有匹配项'], ['.pal-count', '共 0 项']] },
    { press: [['#palInput', 'Escape']], absent: ['#paletteBack'] },

    // ⑥ 侧边栏过滤：菜单涨到十几个之后，"用眼睛在列表里找"开始变慢。
    { nav: 'overview', input: [['navFilter', '代理']],
      has: [['#navFilter', 1], ['#navFilter[placeholder]', 1],
        // 过滤后只剩「代理」一项（页面标签命中）
        ['.nav .nav-item:not([style*="display: none"])', 1]] },
    { input: [['navFilter', '']],
      // 后台菜单共 15 项（清空过滤后必须**全部**回来，否则就是"过滤后回不去"）
      has: [['.nav .nav-item:not([style*="display: none"])', 15]] },

    // ⑦ 确认框：原生 confirm() 已全部替换。危险操作要一眼看出是破坏性的，
    //    且必须是**可样式化、带忙碌态**的自定义弹窗。
    // ================================================================
    // v0.27.0 · 后台：余额变动记录（可追溯：谁、什么时候、改了多少、结果）
    // ================================================================
    // 找到「测试账号」那张卡的「记录」按钮并点开。
    { nav: 'users', clickText: [['#userCards .ecard footer button', '记录']],
      has: [['.modal', 1], ['.modal .feed .ev', 2]],
      textIn: [['.modal', '余额变动记录'], ['.modal', 'admin'],
        ['.modal', '客服补偿'],
        // 失败记录也要展示原因 —— "我点了却没生效"的答案往往就在这里。
        ['.modal', '失败：用户不存在'],
        // 变动前后余额必须显示（只给"改了多少"无法对账）。
        ['.modal', '100 → 150']] },

    // 余额调整必须真的走通（v0.27.0）。旧实现依赖原生 prompt：被浏览器弹窗策略
    // 拦截时它**不显示界面且返回 null**，调用方按"取消"处理 —— 用户看到的就是
    // "点了没反应、没有任何报错"。这条用例钉住"填了值就一定发得出去"。
    { nav: 'users', prompt: { delta: '200', reason: '回归验证' },
      clickText: [['#userCards .ecard footer button', '加余额']] },
    { has: [['.toast-card', 1]],
      textIn: [['.toast-host', '余额已从'], ['.toast-host', '300']] },

    // 取消则什么都不做（**不能**按空值继续执行 —— 旧代码 `prompt() || ''` 在
    // "取消"与"留空"之间不做区分，会把取消当成空备注继续打款/继续改余额）。
    { nav: 'users',
      // 先清掉上一条用例留下的提示卡：toast 有 2.8 秒生命期，不清的话这里断言
      // "没有新提示"会读到旧内容而假失败。
      run: ["Array.from(document.querySelectorAll('.toast-card')).forEach(function(n){n.remove()})"],
      prompt: 'cancel',
      clickText: [['#userCards .ecard footer button', '加余额']] },
    { lacks: [['.toast-host', '余额已从']], has: [['.toast-card', 0]] },

    { nav: 'pool', dialog: 'manual', click: ['#accCards .ecard .pick input'] },
    { nav: 'pool', dialog: 'manual', click: ['#btnBulkDelete'],
      has: [['.dlg', 1], ['.dlg.danger', 1], ['[data-dlg-ok]', 1], ['[data-dlg-cancel]', 1]],
      textIn: [['.dlg', '永久删除'], ['.dlg', '不可撤销'], ['.dlg-title', '从号池']] },
    // 取消 → 弹窗消失且**什么都没发生**（批量选择仍在，说明没有误删）
    { dialog: 'cancel', click: ['[data-dlg-cancel]'],
      absent: ['.dlg-back'],
      textIn: [['#btnBulkGroup', '已选 1']] },

    // ⑧ 通知：从"单元素 2.8 秒消失"改为**堆叠卡片**（图标 / 可关闭 / 倒计时线）。
    //    错误提示切页就找不到，是旧实现最实际的毛病。
    { run: ["toast('第一条','ok')", "toast('第二条','err')"],
      has: [['.toast-host', 1], ['.toast-card', 2], ['.toast-card.err', 1],
        ['.toast-card .toast-ico', 2], ['.toast-card .toast-bar', 2],
        ['.toast-x', 2]],
      textIn: [['.toast-host', '第一条'], ['.toast-host', '第二条']] },
    { run: ["document.querySelector('.toast-x').click()"],
      has: [['.toast-card', 1]] },

    // ⑨ 骨架屏：首屏用它代替"加载中…"（形状先出现，内容后到位，不跳版）。
    { nav: 'overview',
      run: ["skeletonCards(document.getElementById('ovKpis'), 4)"],
      has: [['#ovKpis .skel-card', 4], ['#ovKpis .skel', 12]] },
    // 表格骨架：用一个真实存在的 tbody（不能拿 document.body 兜底 —— 那会把
    // 整个页面清空，后面所有用例都会以"找不到元素"失败，根因却看不出来）。
    { nav: 'codes', run: ["skeletonRows(document.querySelector('#codeRows') || document.querySelector('.pane.active tbody'), 2, 3)"] },

    // ⑩ 常驻失败横幅 + 重试：失败**不能只弹 toast**（几秒后消失，页面上的旧数字
    //    看起来仍然可信）。这是本批最实质的一处修正。
    { nav: 'overview', fail: ['/api/admin/live'],
      press: [[null, 'k', 'ctrl']],
      has: [['.load-banner', 1], ['.load-banner button', 1]],
      textIn: [['.load-banner', '实时态势已连续'], ['.load-banner', '重试']] },
    { nav: 'overview', press: [[null, 'Escape']],
      has: [['.load-banner', 1]] },
  ];
  const dashPlan = [
    { nav: 'overview', counts: [['ovChart', 1], ['ovToday', 4], ['ovFeed', 1], ['ovModels', 1]],
      texts: [['balance', '99.49']],
      has: [['#btnTheme', 1], ['#btnChangePw', 1],
        // 公告卡 + 未读弹窗（v0.17.8）
        ['#annCard .ecard', 1], ['.modal', 1],
        // 实时消耗排行（横向条 2 行）
        ['#ovRankBars .hbar-row', 2]],
      // 公告**置顶在概览页最前**（在 KPI 之前），且有公告时外框必须显示出来。
      before: [['#annBox', '#ovKpis']],
      display: [['#annBox', 'shown']],
      textIn: [['#annCard', '维护通知'], ['.modal', '维护通知'], ['.modal', '我知道了'],
        // v0.22.0：置顶标识 + 有效期提示
        ['#annCard', '📌 置顶'], ['#annCard', '有效期至'],
        ['#ovRankMe', '第 2'], ['#ovRankBars', 'tester']] },
    // 关掉公告弹窗（标记已读，后续刷新不再弹）。
    { click: ['.modal button'] },
    // v0.12.0：多 Key 生成的数量输入框存在（默认 1）。
    // v0.13.1：侧边导航「API 反代」更名为「API 密钥」。
    { nav: 'keys', counts: [['keyCards', 3], ['keyBindList', 1], ['keyKindSeg', 1]], texts: [['keyCount', '1']],
      textIn: [['.nav-item[data-nav="keys"]', 'API 密钥']],
      // v0.17.8：**默认脱敏** —— 初始渲染里既不能有明文，也不能是空的。
      has: [['#keyCards .ecard .mono', 3], ['#keyCards .ecard .icon-btn', 7]],
      lacks: [['.nav-item[data-nav="keys"]', 'API 反代'],
        ['#keyCards', 'sk-wb-abcdefghijklmnop'], ['#keyCards', 'sk-wb-hostkey']] },
    { nav: 'keys',
      textIn: [['#keyCards', 'sk-wb-••••••••mnop'],
        ['#keyCards', 'sk-wb-••••••••tkey']] },
    // 点「查看」→ 显式取回明文（只有这一下才会出现）。
    // 不再带 nav（避免 clickNav 触发的重新渲染把刚展开的明文冲掉）。
    { click: ['#keyCards .ecard .icon-btn'],
      textIn: [['#keyCards', 'sk-wb-abcdefghijklmnop']] },
    { nav: 'test', counts: [['testKey', 2], ['modelOpts', 2]] },
    { nav: 'usage', counts: [['bkCards', 3], ['usKpis', 6], ['usChart', 1], ['usModels', 1], ['myDailyTable', 1], ['usFeed', 1]] },

    // ================================================================
    // v0.27.0 · 计费倍率可视化（用户侧）
    // ================================================================
    // 三个信息缺一不可：当前倍率（算成本要用）、趋势（"最近变贵了吗"）、
    // 逐条记录（几次、什么时候、从多少到多少、为什么）。
    { nav: 'usage',
      has: [['#multKpis', 1], ['#multTrend', 1], ['#multHistory', 1],
        // miniGrid 的单元是 .mk（不是 .kpi-card —— 那是 .kpis 用的另一套）。
        ['#multKpis .mk', 4],
        // 调价记录用事件流（不是表格）：时间 + 变化 + 方向 + 说明四段都要有。
        ['#multHistory .feed .ev', 2],
        // 趋势线必须画出来（有历史记录时不能只剩空态提示）。
        ['#multTrend svg', 1]],
      textIn: [['#multKpis', '当前倍率'], ['#multKpis', '1.2'],
        ['#multKpis', '最近调整'], ['#multKpis', '下调'],
        // 涨/降两种方向都要能看出：-20% 与 +50% 各一条
        ['#multHistory', '1.5 → 1.2'], ['#multHistory', '1 → 1.5'],
        ['#multHistory', '-20.0%'], ['#multHistory', '+50.0%'],
        ['#multHistory', '算力成本回落'],
        // 无说明时要给中性文案，不能是空白（空白看起来像渲染出错）
        ['#multHistory', '倍率上调']],
      // 用户侧**不得**出现调整人（后端已剔除）：调价是平台行为，不是某个人的行为。
      lacks: [['#multKpis', 'admin'], ['#multHistory', 'admin']] },
    // v0.27.0：钱包 KPI 增加「审核中（已冻结）」一项 → 9 个。
    { nav: 'wallet', counts: [['wdKpis', 9], ['wdCards', 2], ['rcCards', 1]],
      textIn: [['#wdKpis', '审核中（已冻结）']] },
    // v0.12.0：代挂页顶部两个扫码按钮（国内版 / 国际版）。
    // v0.13.1：卡片新增任务完成情况（已签到 / 旅行状态）。
    // v0.19.0：账号市场（用户控制台）——在售卡片 + 买断记录 + 余额。
    { nav: 'market',
      has: [['#marketCards .ecard', 1], ['#marketCards .ecard button', 1], ['#marketMine table', 1],
        ['#marketOwned .ecard', 2], ['#marketOwned .ecard button', 1]],
      textIn: [['#marketCards', '在售号'], ['#marketCards', '买断'],
        ['#marketBalance', '500'], ['#marketMine', '300'],
        ['#marketOwned', '我的可卖号'], ['#marketOwned', '我的低分号'],
        ['#marketOwned', '积分不足 4000'],
        ['#marketBalance', '4000']] },
    { nav: 'host', counts: [['hostCards', 1], ['hostEarnCards', 1], ['hostUnifiedList', 6]],
      has: [['#hostOauthBtns .oauth-btns button', 2], ['#hostCards .ecard .tstatus', 1]],
      // v0.26.7：代挂卡片的「已挂载 N 天」标签（号主最常问的问题）。
      // 断言放在这里而不是独立用例：host 数据是异步加载的，独立用例可能先于数据到达。
      textIn: [['#hostCards', '已挂载 30 天']],
      lacks: [['#hostCards', '分组：']],
      texts: [['hostSummary', '累计收益']],
      // v0.24.2（P3-13）：共享 hostStateTag 仍然生效（与管理后台同一份实现）。
      textIn: [['#hostCards', '已入池']] },
    // v0.17.1：用户控制台也有独立的「成长任务」侧边栏（只含本人代挂账号）。
    { nav: 'growth', counts: [['hgCards', 1]],
      has: [['#hAll', 1], ['#hgBtnClear', 1], ['#hgBtnQuery', 1], ['#hgBtnRun', 1],
        ['#hgCards .ecard .pick input', 1], ['#hgCards .progress', 1]],
      textIn: [['#hgCards', '自动项 15/16'], ['#hgCards', '轻量云专家']] },
    // 返点页（v0.17.7）：金额一律 ¥；比例标签带「单独设定 / 跟随全局」来源标注。
    { nav: 'rebates', counts: [['rbPoolKpis', 2], ['rbRefKpis', 4], ['rbRows', 1]],
      texts: [['rbCode', 'ABCD2345'], ['rbPoolRatio', '号池分成 60%（单独设定）']],
      // 累计号池返点 12.5 点 × 0.01 = ¥0.13（四舍五入到 2 位）；流水 +8 点 = +¥0.08
      textIn: [['#rbPoolKpis', '¥0.13'], ['#rbPoolKpis', '折合 12.5 点'],
        ['#rbRefKpis', '¥0.09'], ['#rbRows', '+¥0.08'], ['#rbRows', '折合 80 点'],
        ['#rbPoolRatio', '单独设定'], ['#rbRefRatio', '跟随全局']] },
    // v0.17.8：深色模式手选即时生效（按钮文案与 <html data-theme> 同步变化）。
    { click: ['#btnTheme'], has: [['html[data-theme="dark"]', 1]], textIn: [['#btnTheme', '浅色']] },
    { click: ['#btnTheme'], has: [['html[data-theme="light"]', 1]], textIn: [['#btnTheme', '深色']] },
    { nav: 'docs', texts: [['baseUrl', '/v1']] },

  ];
  const a = await run('admin.html', { id: 1, username: 'admin', is_admin: true }, adminPlan);
  const b = await run('dashboard.html', { id: 2, username: 'tester', is_admin: false, daily_limit: 100, daily_used: 12.5, monthly_limit: 1000, monthly_used: 800 }, dashPlan);
  // v0.17.8：**无公告时整块隐藏** —— 公告置顶在首屏最上方，若还留一个
  // 「暂无公告」的空卡，等于用最贵的位置说了一句废话。
  // v0.23.1：控制台入池按钮必须区分「积分未知」与「积分不足」。
  // 桩里把账号换成「未入池 + 积分未获取（credits_known=false）」，断言出现
  // 「同步积分」按钮与对应提示 —— 否则用户会被误导成「积分太少」而无从下手。
  const savedHost = FX['/api/host/accounts'];
  FX['/api/host/accounts'] = {
    accounts: [{
      uid: 'host-un', nickname: '未入池号', realm: 'cn', host_group: '', pool_state: '',
      disabled: false, cooling: false, cooling_for: 0, credits: 0, credits_known: false,
      expires_at: now + 86400, earned: 0, created_at: now, owner_id: 2,
      task: { enabled: true, disabled: [], kinds: [] },
    }],
    groups: {},
    summary: { total: 1, in_pool: 0, pending: 0, earned: 0, earnings: 0 },
  };
  const cApply = await run('dashboard.html', { id: 2, username: 'tester', is_admin: false }, [
    { nav: 'host',
      counts: [['hostCards', 1]],
      // 未入池的可操作按钮 = 申请进号池 + 同步积分 + 删除（3 个；分组下拉是 select 不计）。
      has: [['#hostCards .ecard footer button', 3]],
      textIn: [['#hostCards', '申请进号池'], ['#hostCards', '同步积分'],
        ['#hostCards', '积分未获取'], ['#hostCards', '未分组'],
        ['#hostCards', '不影响入池']],
      // 积分未知时不得再显示「积分不足」这种误判文案（lacks 查文本，absentIn 只认选择器）。
      lacks: [['#hostCards', '积分不足 2000']] },
  ]);
  FX['/api/host/accounts'] = savedHost;

  const savedAnn = FX['/api/announcements'];
  FX['/api/announcements'] = { announcements: [], count: 0 };
  const b2 = await run('dashboard.html', { id: 2, username: 'tester', is_admin: false }, [
    { nav: 'overview', display: [['#annBox', 'none']],
      has: [['#annCard .ecard', 0]], lacks: [['#annCard', '暂无公告']] },
  ]);
  FX['/api/announcements'] = savedAnn;
  // v0.24.2（P3-13）：三处重复 helper 已下沉 app.js。
  // 这里做一条**静态**断言（源码级）——jsdom 只能验「渲染结果对不对」，
  // 证明不了「重复定义是否真的删干净了」（重复定义会静默覆盖共享实现，
  // 渲染结果看起来完全正常，直到某天只改了一边）。
  const dupDefs = ['function hostStateTag', 'function taskModelOf', 'function taskModelToJSON'];
  const dupBad = [];
  for (const f of ['admin.html', 'dashboard.html']) {
    const src = fs.readFileSync(path.join(UI, f), 'utf8');
    for (const def of dupDefs) if (src.includes(def)) dupBad.push(`${f} 仍存在重复定义：${def}`);
  }
  for (const def of dupDefs) if (!APP_JS.includes(def)) dupBad.push(`app.js 缺少共享定义：${def}`);
  dupBad.forEach((m) => console.log('  [FAIL] dedup: ' + m));
  if (!dupBad.length) console.log('  [ok] dedup: 三个共享 helper 只在 app.js 定义一次');

  // v0.24.3（2026-10-13 封号事故）：账号卡片**不得**再「渲染即出站」。
  //
  // creditPanel 会真实调上游查积分（/resources → writeCreditDetail → 上游），
  // 而账号池页有几十张卡片 —— 渲染即出站 = 几十个**并发**请求、同一个出口 IP、
  // 几秒内打完 → 上游风控判「请求非法(11140)」→ 30 个健康账号被误判停用。
  //
  // 这条断言是那场事故的**回归守卫**：一旦有人把自动查询改回来，这里立刻红。
  // jsdom 层面验不出这个（渲染结果看起来完全正常），所以做源码级检查。
  const probeBad = [];
  for (const f of ['admin.html', 'dashboard.html']) {
    const src = fs.readFileSync(path.join(UI, f), 'utf8');
    if (src.includes('appendChild(creditPanel(')) {
      probeBad.push(`${f} 仍在渲染时自动出站：appendChild(creditPanel(...))`);
    }
    if (!src.includes('creditSnapshotLine(')) {
      probeBad.push(`${f} 未改用 creditSnapshotLine（快照 + 点开才查）`);
    }
  }
  if (!APP_JS.includes('function creditSnapshotLine')) {
    probeBad.push('app.js 缺少 creditSnapshotLine');
  }
  probeBad.forEach((m) => console.log('  [FAIL] probe-gate: ' + m));
  if (!probeBad.length) console.log('  [ok] probe-gate: 账号卡不再渲染即出站（改为点开才查）');

  // v0.31.28：**「查询失败」不能渲染成「不支持」**。
  //
  // 2026-10-18 全池 token 被上游作废时，账号卡显示的是「签到：不支持」——
  // 因为后端出错时 `supported` 是 Go 零值 false，而前端**先判 supported、再判 ok**。
  // 用户看到"功能不支持"，真实原因是凭证失效，处理动作完全不同。
  // 这条守卫钉三件事：判断顺序（先 ok 后 supported）、后端的 401 人话翻译、refresh 单飞。
  const tsBad = [];
  {
    const ROOT = path.resolve(UI, '..', '..', '..');
    const iFail = APP_JS.indexOf("if (!c.ok) box.appendChild(chipWithTitle('签到：查询失败'");
    const iSup = APP_JS.indexOf("else if (c.supported === false) box.appendChild(el('span', 'tag', '签到：不支持'))");
    if (iFail < 0 || iSup < 0) tsBad.push('app.js 找不到签到状态的两个分支');
    else if (iFail > iSup) tsBad.push('app.js 先判 supported 再判 ok —— 查询失败会被渲染成「签到：不支持」');
    const iTFail = APP_JS.indexOf("if (!t.ok) box.appendChild(chipWithTitle('旅行：查询失败'");
    const iTSup = APP_JS.indexOf("else if (t.supported === false) box.appendChild(el('span', 'tag', '旅行：不支持'))");
    if (iTFail < 0 || iTSup < 0) tsBad.push('app.js 找不到旅行状态的两个分支');
    else if (iTFail > iTSup) tsBad.push('旅行状态同样要先判 ok 再判 supported');

    const tsSrc = fs.readFileSync(path.join(ROOT, 'cmd/webapp/taskstatus.go'), 'utf8');
    if (!tsSrc.includes('func taskStatusErrText(')) {
      tsBad.push('taskstatus.go 缺少 401/12153 的人话翻译（用户只会看到一坨 HTML）');
    }
    const upSrc = fs.readFileSync(path.join(ROOT, 'internal/upstream/client.go'), 'utf8');
    if (!upSrc.includes('refreshFlights')) {
      tsBad.push('client.go 缺少 refresh 单飞：并发刷新会让第二个请求 12153 并作废旧凭证');
    }
  }
  tsBad.forEach((m) => console.log('  [FAIL] task-status-semantics: ' + m));
  if (!tsBad.length) {
    console.log('  [ok] task-status-semantics: 查询失败与「不支持」已分开；refresh 有单飞');
  }

  // v0.31.19：前端字面量里的 /api 路径必须都能在后端路由表里找到。
  //
  // 为什么要单做这条：`admin.html` 的「预览方案 / 执行分配」两个按钮打的是
  // `/api/admin/egress/pool/assign`，而 `main.go` 里注册的是
  // `/api/admin/egress/assign` —— 于是**批量分配整块功能一直是 404**，
  // 而 jsdom 用例（桩 fetch 不看路径）与 Go 用例（直接调 handler）都照样全绿。
  // 这类"前后端各写各的"漂移，只有把两边的路径**放在一起比**才算测过。
  const routeBad = [];
  {
    const mainSrc = fs.readFileSync(path.join(path.dirname(UI), 'main.go'), 'utf8');
    const norm = (x) => x.replace(/\{[^}]+\}/g, '*');
    const routes = new Set(
      Array.from(mainSrc.matchAll(/HandleFunc\("(\w+) ([^"]+)"/g)).map((m) => norm(m[2])));
    for (const f of ['admin.html', 'dashboard.html', 'app.js']) {
      const src = fs.readFileSync(path.join(UI, f), 'utf8');
      for (const m of src.matchAll(/['"](\/api\/[A-Za-z0-9_\-/]+)['"]/g)) {
        const url = m[1];
        if (url.endsWith('/')) continue; // 拼接前缀（如 '/api/keys/' + id），静态判不了
        if (!routes.has(norm(url))) routeBad.push(`${f} 引用了未注册的接口：${url}`);
      }
    }
  }
  routeBad.forEach((m) => console.log('  [FAIL] route-drift: ' + m));
  if (!routeBad.length) console.log('  [ok] route-drift: 前端引用的 /api 路径都能在后端路由表里找到');

  // v0.24.9：明细展示必须来自**列表接口下发的缓存**，不得为了"恢复明细"把出站改回来。
  //
  // 上面那条 probe-gate 守卫的是「不要渲染即出站」；这条守卫的是它的**反面陷阱**：
  // 为了让卡片重新显示明细，最省事的做法就是调回 creditPanel(url) —— 那等于
  // 把 2026-10-13 的事故原样重演。所以这里额外钉住两条：
  //   ① 明细的唯一数据来源是 a.credit_packs（随列表下发，零出站）；
  //   ② 刷新失败时必须能回退到缓存渲染 —— 否则探活总闸限流（30s 内刚查过）会让
  //      整块明细消失，看起来就像"功能又坏了"（用户实际反馈过的现象）。
  const creditCacheBad = [];
  {
    if (!APP_JS.includes('a.credit_packs')) {
      creditCacheBad.push('creditSnapshotLine 未读取 credit_packs（明细缓存）');
    }
    if (!/loadCreditPanel\(box, url, title, render\)/.test(APP_JS)) {
      creditCacheBad.push('刷新明细未把缓存渲染函数传下去（失败时限流会让明细整块消失）');
    }
    if (!/function loadCreditPanel\(box, url, title, fallback\)/.test(APP_JS)) {
      creditCacheBad.push('loadCreditPanel 缺少失败回退参数');
    }
  }
  {
    // 「预热明细」必须真的接到服务端那个**串行**端点上，且必须能带勾选项。
    // 接了错端点（比如逐号并发打 /resources）会让这个按钮变成新的风控事故按钮。
    const src = fs.readFileSync(path.join(UI, 'admin.html'), 'utf8');
    if (!src.includes("post('/api/admin/accounts/credit-warmup'")) {
      creditCacheBad.push('「预热明细」未接到 /api/admin/accounts/credit-warmup');
    }
    if (!/credit-warmup'[^)]*uids: picked/.test(src)) {
      creditCacheBad.push('「预热明细」未把勾选项作为 uids 传给服务端');
    }
    if (/\/accounts\/' \+ encodeURIComponent\([^)]*\) \+ '\/resources'[\s\S]{0,200}?\.forEach/.test(src)) {
      creditCacheBad.push('前端出现了并发逐号查 /resources 的写法（＝重演封号事故）');
    }
  }
  {
    // v0.25.0：积分补新器必须**串行**（一次一个、等上一个返回），且一轮跑完就停。
    // 把它改成 forEach/并发就是原样重演 2026-10-13 的封号事故 ——
    // 而渲染结果看起来完全正常，只有源码级检查拦得住。
    const src = fs.readFileSync(path.join(UI, 'admin.html'), 'utf8');
    if (!/creditPumpBusy\) return;/.test(src)) {
      creditCacheBad.push('补新器缺少串行闸（creditPumpBusy）——并发刷上游会重演封号事故');
    }
    if (/creditPumpQueue\s*\.\s*forEach/.test(src)) {
      creditCacheBad.push('补新器出现 forEach 并发刷新');
    }
    if (!src.includes("document.hidden")) {
      creditCacheBad.push('补新器未检查页面可见性——切走的标签页不该继续打上游');
    }
    // 单号刷新端点：一次一个，服务端不排队。
    if (!src.includes("'/api/admin/accounts/credit-refresh'")) {
      creditCacheBad.push('补新器未接单号刷新端点 /api/admin/accounts/credit-refresh');
    }
    // 导入防关联：弹窗必须**在粘贴之前**就显示出口 IP 状态（否则运维粘完才被拒）。
    if (!src.includes("'/api/admin/import/ip-status'")) {
      creditCacheBad.push('导入弹窗未调用出口 IP 状态端点（无法在粘贴前告知需不需要换 IP）');
    }
    for (const id of ['setImportMaxNew', 'setImportGate', 'setIPResetCmd', 'setCreditAutoSec']) {
      if (!src.includes(`id="${id}"`)) creditCacheBad.push(`设置页缺少配置项 ${id}`);
    }
    // v0.25.1 设备风控凭证（X-Device-Token）：
    //   ① 凭证输入框必须是 password 且**永不回显** —— 一旦 loadSettings 往里写值，
    //      明文凭证就会铺在管理页上（截图/共享即泄露）；
    //   ② 「留空 = 不修改」与「清除」必须是两个不同的入口 ——
    //      混在一个输入框里，一次误点保存就会把凭证清掉（静默失效，最难查）；
    //   ③ 账号卡必须显示凭证状态（此前"全池都没带"完全不可见）。
    if (!/type="password"[^>]*id="setDeviceToken"/.test(src)) {
      creditCacheBad.push('设备凭证输入框不是 password 类型');
    }
    if (/\$\('setDeviceToken'\)\.value\s*=/.test(src)) {
      // 唯一允许的写入是"保存后清空自己"，形如 $('setDeviceToken').value = '';
      const assigns = src.match(/\$\('setDeviceToken'\)\.value\s*=\s*[^;]+;/g) || [];
      if (assigns.some((x) => !/=\s*''\s*;/.test(x))) {
        creditCacheBad.push('设备凭证明文被回显到页面上（只允许保存后清空自身）');
      }
    }
    if (!src.includes("id=\"btnClearDeviceToken\"")) {
      creditCacheBad.push('缺少「清除全局凭证」的独立入口（与"留空=不修改"语义相反，不能混用）');
    }
    if (!src.includes('device_token_state')) {
      creditCacheBad.push('账号卡未显示设备凭证状态（缺失时完全静默，审计才发现）');
    }
    // 门禁拒绝时必须有**可执行的**引导，而不是只丢一句 error。
    if (!src.includes('e.data && e.data.guard')) {
      creditCacheBad.push('导入被拒时未展开 guard（运维看不到"该换 IP 还是该分批"）');
    }
  }
  creditCacheBad.forEach((m) => console.log('  [FAIL] credit-cache: ' + m));
  if (!creditCacheBad.length) console.log('  [ok] credit-cache: 明细走列表下发的缓存渲染，刷新失败回退缓存，预热走服务端串行端点');

  // ---- 卡片总额的取值口径（v0.25.0）----
  //
  // 这里单跑一次 creditSnapshotLine（真调函数，不看源码），钉住三条：
  //   ① 有缓存 → 总额取**缓存明细合计**，不是池内快照；
  //   ② 服务端字段缺失 → **自算兜底**，绝不能静默变成 0；
  //   ③ 无缓存 → 回落池内快照。
  //
  // ② 是真机踩出来的：补新器就地重画卡片时漏传 credit_cached_remain，
  //    卡片总额就显示成 0 —— 一个"字段漏传就变 0"的接口是脆的，
  //    而它只在那条异步路径上出现，常规用例根本照不到。
  {
    const bad = [];
    const d2 = new JSDOM('<!doctype html><html><body></body></html>', {
      runScripts: 'dangerously', url: 'http://127.0.0.1:8780/admin.html',
      beforeParse(win) {
        win.fetch = () => Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve('{}'), json: () => Promise.resolve({}) });
      },
    });
    const sc = d2.window.document.createElement('script');
    sc.textContent = APP_JS;
    d2.window.document.body.appendChild(sc);
    const win = d2.window;
    const rows = [{ name: 'a', remain: 1000, size: 1000, expire_at: Math.floor(Date.now() / 1000) + 86400 }];
    const total = (a) => win.creditSnapshotLine(a, '查积分明细', '/x', 't').querySelector('.credit-total').textContent;
    const cases = [
      ['有缓存', { credits: 4444, credits_known: true, credit_packs_at: 1791896591, credit_cached_remain: 1234, credit_packs: rows }, '1,234'],
      ['字段缺失兜底', { credits: 4444, credits_known: true, credit_packs_at: 1791896591, credit_packs: rows }, '1,000'],
      ['无缓存', { credits: 4444, credits_known: true, credit_packs: [] }, '4,444'],
    ];
    for (const [label, a, want] of cases) {
      const got = total(a);
      const pass = got.includes(want);
      if (!pass) bad.push(`${label}：总额应含 ${want}，得 "${got}"`);
      console.log(`  ${pass ? '[ok]' : '[FAIL]'} credit-total: ${label} => "${got}" (期望含 ${want})`);
    }
    try { d2.window.close(); } catch (e) {}
    if (!bad.length) console.log('  [ok] credit-total: 总额口径（缓存合计 / 缺失兜底 / 无缓存快照）三条都对');
  }

  // ---- 批量删除的安全边界（v0.24.5）----
  // 删除**不可逆**，所以这里守住两条只有源码级的检查才验得到的不变量
  // （跑起来完全看不出差别：点错一次才后悔）：
  //   ① 「删除选中」必须**只**作用于勾选项 —— 绝不能像「批量设置分组」那样在
  //      无勾选时兜底成"当前筛选"（筛选会随档位变化，那等于一键删掉整屏账号）；
  //   ② 「删除对话探活失败的号」必须同时要求 chat_probed 与 !chat_ok ——
  //      只判 !chat_ok 会把"本次没做探活"也当成失败，进而删掉其实健康的号。
  const delBad = [];
  {
    const src = fs.readFileSync(path.join(UI, 'admin.html'), 'utf8');
    for (const id of ['btnBulkDelete', 'btnHealthDeleteFailed']) {
      if (!src.includes(`id="${id}"`)) delBad.push(`admin.html 缺少按钮 ${id}`);
    }
    if (!src.includes("r.chat_probed && !r.chat_ok")) {
      delBad.push('healthFailedAccounts 未同时要求 chat_probed 与 !chat_ok（会把"没探活"误判成失败）');
    }
    // 取「删除选中」处理器的函数体，单独检查它没有筛选兜底。
    const i = src.indexOf("$('btnBulkDelete').onclick");
    if (i < 0) {
      delBad.push('未找到「删除选中」的点击处理器');
    } else {
      const body = src.slice(i, src.indexOf("$('btnGrowthRun')", i) > 0
        ? src.indexOf("// 成长任务", i) : i + 2000);
      if (body.includes('visibleAccounts')) {
        delBad.push('「删除选中」在无勾选时兜底成了"当前筛选"（删除必须只作用于勾选项）');
      }
      if (!body.includes('/api/admin/accounts/batch-delete')) {
        delBad.push('「删除选中」未调用批量删除接口');
      }
    }
  }
  delBad.forEach((m) => console.log('  [FAIL] bulk-delete: ' + m));
  if (!delBad.length) console.log('  [ok] bulk-delete: 删除只作用于勾选项，失败号须同时证明"测过且失败"');

  // v0.31.22：**开关必须真的能点**（行为级验证，不按源码文本验）。
  //
  // 本项目的 `.switch` 把真实 input 做成 0×0 透明（style.css：
  // `.switch input { position:absolute; opacity:0; width:0; height:0 }`），
  // 因此**只有被 `<label>` 包裹时**点击才会落到 input 上。admin/dashboard 里有
  // 四处是裸写的 ——「启用定时任务」「开放注册」「调试对话-流式」×2：它们看起来
  // 是开关、实际点不动；而用户不会报"开关坏了"，只会觉得"设了没用"。
  //
  // 这类问题**只有真的点一下**才看得见（源码里 `<span class="switch">` 完全正常），
  // 所以这里按行为验。同一段顺带验插件卡上的开关能打到**本端**的接口
  // （管理端 /api/admin/plugins、用户端 /api/plugins）—— 这是"用户也能自己
  // 启用/停用插件"的最小闭环。
  const switchBad = [];
  {
    const probe = async (pageFile, navs, sels, fx) => {
      const html = fs.readFileSync(path.join(UI, pageFile), 'utf8')
        .replace('<script src="/app.js"></script>', '<script>' + APP_JS + '</script>');
      const vc = new VirtualConsole();
      vc.on('jsdomError', () => {}); // 这里只看下面的断言，运行噪音忽略
      vc.on('error', () => {});
      const dom = new JSDOM(html, {
        runScripts: 'dangerously', pretendToBeVisual: true, virtualConsole: vc,
        url: 'http://127.0.0.1:8780/' + pageFile,
        beforeParse(win) {
          Object.assign(FX, fx || {});
          win.__calls = [];
          win.fetch = (url, opt) => {
            win.__calls.push(String(url) + ' ' + String((opt && opt.method) || 'GET'));
            return Promise.resolve({
              ok: true, status: 200,
              text: () => Promise.resolve(JSON.stringify(fixtureFor(url))),
              json: () => Promise.resolve(fixtureFor(url)),
            });
          };
          win.confirm = () => true; win.prompt = () => '1'; win.alert = () => {};
          win.open = () => ({ closed: false, location: { href: '' }, close() {} });
          win.navigator.clipboard = { writeText: () => Promise.resolve() };
          // ⚠️ 这两个必须与上面的 run() 保持一致：本项目的自定义弹窗
          // （confirmAsk / promptDialog）会**等用户响应**，不预置就是永久等待 ——
          // 表现是 init 永远走不完（`window.navGo` 一直不挂），而报错一片安静。
          win.__autoConfirm = true;
          win.__autoPrompt = null;
          if (win.Element && !win.Element.prototype.scrollIntoView) {
            win.Element.prototype.scrollIntoView = function () {};
          }
        },
      });
      const w = dom.window;
      const doc = w.document;
      const wait = (ms) => new Promise((r) => setTimeout(r, ms));
      // ⚠️ 必须先等页面把导航绑好：`setupNav` 是在 init（含若干 await）之后才跑的，
      // 结束时才挂上 `window.navGo`。早于它的点击**静默无效**（`nav-item.onclick`
      // 还是默认的 null），随后断言会报"找不到开关"—— 根因只是"点早了"。
      for (let i = 0; i < 40 && typeof w.navGo !== 'function'; i++) await wait(100);
      for (const nav of (navs || [])) {
        const it = Array.from(doc.querySelectorAll('.nav-item')).find((x) => x.dataset.nav === nav);
        if (!it) { switchBad.push(`${pageFile} 没有导航 ${nav}`); continue; }
        it.dispatchEvent(new w.MouseEvent('click', { bubbles: true }));
        await wait(350);
      }
      for (const [sel, want] of sels) {
        const cb = doc.querySelector(sel);
        if (!cb) { switchBad.push(`${pageFile} 找不到开关 ${sel}`); continue; }
        const sw = cb.closest('.switch');
        if (!sw) { switchBad.push(`${pageFile} 的 ${sel} 不在 .switch 组件里`); continue; }
        const before = cb.checked;
        sw.querySelector('i').dispatchEvent(new w.MouseEvent('click', { bubbles: true, cancelable: true }));
        if (cb.checked === before) {
          switchBad.push(`${pageFile} 的开关 ${sel} 点不动（裸 .switch：没被 label 包裹，也没有兜底委托）`);
          continue;
        }
        if (want) {
          await wait(300);
          if (!w.__calls.some((u) => u.indexOf(want) >= 0)) {
            switchBad.push(`${pageFile} 的开关 ${sel} 切换后未请求 ${want}`);
          }
        }
        cb.checked = before; // 复原，避免影响后续断言
      }
      try { w.close(); } catch (e) {}
    };

    const PLUGIN_ADMIN = { plugins: [{
      id: 'claim-compensation', name: '多账号领取积分补偿', desc: '桩',
      audience: 'both', builtin: true, enabled: true, user_enabled: true, active: true,
    }] };
    // 用户端刻意给「平台启用、但我停用」的组合：这条路径最容易写错
    // （前端若只看 `enabled` 就会把开关画成"已启用"，与服务端口径不符）。
    const PLUGIN_USER = { plugins: [{
      id: 'claim-compensation', name: '多账号领取积分补偿', desc: '桩',
      audience: 'both', builtin: true, enabled: true, user_enabled: false, active: false,
    }] };
    // ⚠️ 必须**显式**固定登录身份：上面的 run(pageFile, meOverride, ...) 会把
    // FX['/api/auth/me'] 改成别的用户并留在那儿。管理员页的 init 第一句就是
    // `if (!me) return;`（requireAdmin），身份不对 → init 直接早退、`window.navGo`
    // 永不挂上，断言只会报一句"找不到开关"，看不出真实原因。
    const fxOf = (k, v) => ({
      '/api/auth/me': { user: { id: 1, username: 'admin', is_admin: true, status: 'active' } },
      [k]: v,
    });

    await probe('admin.html', ['tasks', 'plugins'],
      [['#tcEnabled', null], ['#setRegister', null], ['#stream', null],
       ['#pluginCards .switch input', '/api/admin/plugins/claim-compensation/enabled POST']],
      fxOf('/api/admin/plugins', PLUGIN_ADMIN));
    await probe('dashboard.html', ['plugins'],
      [['#testStream', null],
       ['#pluginCards .switch input', '/api/plugins/claim-compensation/enabled POST']],
      fxOf('/api/plugins', PLUGIN_USER));
  }
  switchBad.forEach((m) => console.log('  [FAIL] switch-clickable: ' + m));
  if (!switchBad.length) {
    console.log('  [ok] switch-clickable: 端上开关都能点（含裸 .switch），插件开关打到本端接口');
  }

  // v0.31.23：**代理页的线路卡必须真的渲染出来**。
  //
  // 换来这条的是一次"整块页面全空"：`GET /api/admin/egress/proxies` 当时下发的是
  // **Go 字段名**（`Name`/`URL`/`LastIP`），而前端读 `p.name`/`p.url` ——
  // `(p.name || p.url).charAt(0)` 抛 TypeError，一张卡都画不出来；
  // 而异常被 `loadEgress` 自己的 try/catch **吞成一句 toast**，
  // 页面上只剩空白：既看不出是前端 bug，也看不出是真没数据。
  //
  // 这条按**行为**验：喂一份 snake_case 的线路数据，断言 #egrCards 里**有卡片**。
  // 后端那一侧另有 `TestEgressProxiesShape` 做键名双向断言 —— 两边合起来才闭环。
  const egrBad = [];
  {
    const html = fs.readFileSync(path.join(UI, 'admin.html'), 'utf8')
      .replace('<script src="/app.js"></script>', '<script>' + APP_JS + '</script>');
    const vc = new VirtualConsole();
    vc.on('jsdomError', () => {});
    vc.on('error', () => {});
    const dom = new JSDOM(html, {
      runScripts: 'dangerously', pretendToBeVisual: true, virtualConsole: vc,
      url: 'http://127.0.0.1:8780/admin.html',
      beforeParse(win) {
        win.__autoConfirm = true;
        win.__autoPrompt = null;
        win.fetch = (url) => {
          const u = String(url);
          if (u.indexOf('/api/auth/me') >= 0) {
            return Promise.resolve({ ok: true, status: 200,
              text: () => Promise.resolve('{"user":{"id":1,"username":"admin","is_admin":true}}'),
              json: () => Promise.resolve({ user: { id: 1, username: 'admin', is_admin: true } }) });
          }
          if (u.indexOf('/api/admin/egress/proxies') >= 0) {
            const U1 = 'http://10.1.1.1:8080';
            const U2 = 'http://10.1.1.2:8080';
            const body = JSON.stringify({
              // v0.31.26：两条线路**同一个出口 IP** —— 页面必须说出"实际只有 1 个出口"。
              // 只看条数会给出错误的安全感（2026-10-17 现场：2 条线路都是 39.91.80.16）。
              proxies: [
                {
                  id: 'p1', name: '线路甲', url: U1, region: '某地', note: '备注',
                  enabled: true, last_ok: true, last_ip: '9.9.9.9', last_check: now,
                  fail_streak: 0, created_at: now, updated_at: now, owner_user_id: 0,
                },
                {
                  id: 'p2', name: '线路乙', url: U2, region: '某地', note: '同一条宽带',
                  enabled: true, last_ok: true, last_ip: '9.9.9.9', last_check: now,
                  fail_streak: 0, created_at: now, updated_at: now, owner_user_id: 0,
                },
              ],
              bound: { [U1]: 3, [U2]: 0 },
              bound_names: { [U1]: ['甲'], [U2]: [] },
              owners: {},
              machines: {},
              exit_ip: { [U1]: '9.9.9.9', [U2]: '9.9.9.9' },
              shared_exit: { [U1]: [U2], [U2]: [U1] },
              shared_exit_groups: [{ exit_ip: '9.9.9.9', urls: [U1, U2], names: ['线路甲', '线路乙'], owners: [0, 0] }],
              distinct_exit_ips: 1,
            });
            return Promise.resolve({ ok: true, status: 200,
              text: () => Promise.resolve(body), json: () => Promise.resolve(JSON.parse(body)) });
          }
          return Promise.resolve({ ok: true, status: 200,
            text: () => Promise.resolve('{}'), json: () => Promise.resolve({}) });
        };
        win.confirm = () => true; win.prompt = () => '1'; win.alert = () => {};
        win.open = () => ({ closed: false, location: { href: '' }, close() {} });
        win.navigator.clipboard = { writeText: () => Promise.resolve() };
        if (win.Element && !win.Element.prototype.scrollIntoView) {
          win.Element.prototype.scrollIntoView = function () {};
        }
      },
    });
    const w = dom.window;
    const wait = (ms) => new Promise((r) => setTimeout(r, ms));
    for (let i = 0; i < 40 && typeof w.navGo !== 'function'; i++) await wait(100);
    if (typeof w.navGo === 'function') w.navGo('egress');
    await wait(600);
    const box = w.document.getElementById('egrCards');
    const n = box ? box.children.length : -1;
    if (n !== 2) {
      egrBad.push(`代理页线路卡应渲染 2 张，实得 ${n}（0 = 卡构造时抛错被 try/catch 吞掉）`);
    }
    if (box && String(box.textContent).indexOf('线路甲') < 0) {
      egrBad.push('线路卡里没有线路名 —— 前端读的键与服务端下发的不一致');
    }
    // v0.31.26 出口身份：同 IP 的两条线路必须在卡片与总览上说明"实际只有 1 个 IP"。
    const txt = box ? String(box.textContent) : '';
    if (txt.indexOf('共用出口') < 0) {
      egrBad.push('两条线路 last_ip 相同，但卡片没有"共用出口"提示 —— 「多条线路 = 多个 IP」的错觉会保留');
    }
    const hint = w.document.getElementById('egrPoolHint');
    const htxt = hint ? String(hint.textContent) : '';
    if (htxt.indexOf('1 个不同出口 IP') < 0) {
      egrBad.push(`线路池总览应按 IP 报数（期望含「1 个不同出口 IP」），实得「${htxt}」`);
    }
    try { w.close(); } catch (e) {}
  }
  egrBad.forEach((m) => console.log('  [FAIL] egr-cards: ' + m));
  if (!egrBad.length) console.log('  [ok] egr-cards: 代理页线路卡能渲染（键名与前端读取一致）');

  // v0.31.24：**用户视角的代理页必须能操作，且文案不误导**。
  //
  // 这一版把出口的三个执行端点从 adminGuard 放开给登录用户
  // （在此之前普通用户点「一键拉起」只会拿到 403）。前端是配套的一半：
  // 服务端开好了权限，页面若仍按"只读"渲染，用户看到的还是那句
  // "仅管理员可执行" —— 权限下沉就白做了。
  //
  // 同时钉住**另一半**：用户视角**不能出现「全局默认出口」**。
  // 对一个没有这个开关的人讲它，只会让他以为页面坏了，
  // 更坏的是让他以为"我改了什么会影响全平台"。
  const egrSelfBad = [];
  {
    const html = fs.readFileSync(path.join(UI, 'dashboard.html'), 'utf8')
      .replace('<script src="/app.js"></script>', '<script>' + APP_JS + '</script>');
    const vc = new VirtualConsole();
    vc.on('jsdomError', () => {});
    vc.on('error', () => {});
    const SELF_VIEW = {
      machines: [{
        id: 'u2:pc-a', hostname: 'host-a', online: true, tunnel_reachable: true, ready: true,
        proxy_ok: true, tunnel_ok: true, exit_ip: '1.2.3.4（山东）',
        proxy_port: 1080, tunnel_port: 11081, proxy_url: 'http://127.0.0.1:11081',
        applied: true, host_bound: 2, host_total: 2, last_at: now, is_default: false,
      }],
      agent_online: true, tunnel_reachable: true, applied: true, ready: true,
      default_egress: '', host_bound: 2, host_total: 2,
      can_manage: true, manage_scope: 'self', poll_sec: 5,
      agent_command: 'wb2api-egress-agent.exe -server http://x -token ag2-stub',
      active_machine: 'u2:pc-a',
    };
    const dom = new JSDOM(html, {
      runScripts: 'dangerously', pretendToBeVisual: true, virtualConsole: vc,
      url: 'http://127.0.0.1:8780/dashboard.html',
      beforeParse(win) {
        win.__autoConfirm = true;
        win.__autoPrompt = null;
        win.fetch = (url) => {
          const u = String(url);
          if (u.indexOf('/api/egress/home') >= 0) {
            const body = JSON.stringify(SELF_VIEW);
            return Promise.resolve({ ok: true, status: 200,
              text: () => Promise.resolve(body), json: () => Promise.resolve(SELF_VIEW) });
          }
          // 插件列表（v0.31.34）：三种 kind 各一个，用来验"打开 = 独立界面"。
          if (u.indexOf('/api/plugins') >= 0 && u.indexOf('/enabled') < 0) {
            const PLUG = { plugins: [
              { id: 'claim-compensation', name: '多账号领取积分补偿', desc: '桩',
                audience: 'both', builtin: true, enabled: true, active: true, kind: 'claim' },
              { id: 'health-check', name: '账号健康检测', desc: '桩',
                audience: 'both', builtin: true, enabled: true, active: true, kind: 'health' },
              { id: 'host-egress', name: '本地出口代理', desc: '桩',
                audience: 'user', builtin: true, enabled: true, active: true, kind: 'egress' },
            ] };
            return Promise.resolve({ ok: true, status: 200,
              text: () => Promise.resolve(JSON.stringify(PLUG)),
              json: () => Promise.resolve(PLUG) });
          }
          // 我的账号出口（v0.31.31）：两条线路、两个不同出口 IP、两个号。
          // 页面必须按 IP 报数，并且逐号给出"从哪出去"。
          if (u.indexOf('/api/host/egress/lines') >= 0) {
            const LINES = {
              lines: [
                { url: 'http://127.0.0.1:12070', name: '家里', owner_user_id: 2, mine: true,
                  platform: false, enabled: true, last_ok: true, exit_ip: '1.1.1.1',
                  bound_mine: 1, bound_total: 1, note: '' },
                { url: 'http://127.0.0.1:12071', name: '公司', owner_user_id: 2, mine: true,
                  platform: false, enabled: true, last_ok: true, exit_ip: '2.2.2.2',
                  bound_mine: 1, bound_total: 1, note: '' },
              ],
              accounts: [
                { uid: 'hx-1', nickname: '甲', configured: 'http://127.0.0.1:12070',
                  resolved: 'http://127.0.0.1:12070', resolved_name: '家里', exit_ip: '1.1.1.1',
                  exit_source: 'line' },
                { uid: 'hx-2', nickname: '乙', configured: '',
                  resolved: 'http://127.0.0.1:12071', resolved_name: '公司', exit_ip: '2.2.2.2',
                  exit_source: 'line' },
              ],
              default_egress: '', distinct_exit_ips: 2,
            };
            return Promise.resolve({ ok: true, status: 200,
              text: () => Promise.resolve(JSON.stringify(LINES)),
              json: () => Promise.resolve(LINES) });
          }
          if (u.indexOf('/api/auth/me') >= 0) {
            return Promise.resolve({ ok: true, status: 200,
              text: () => Promise.resolve('{"user":{"id":2,"username":"u2","is_admin":false}}'),
              json: () => Promise.resolve({ user: { id: 2, username: 'u2', is_admin: false } }) });
          }
          return Promise.resolve({ ok: true, status: 200,
            text: () => Promise.resolve('{}'), json: () => Promise.resolve({}) });
        };
        win.confirm = () => true; win.prompt = () => '1'; win.alert = () => {};
        win.open = () => ({ closed: false, location: { href: '' }, close() {} });
        win.navigator.clipboard = { writeText: () => Promise.resolve() };
        if (win.Element && !win.Element.prototype.scrollIntoView) {
          win.Element.prototype.scrollIntoView = function () {};
        }
      },
    });
    const w = dom.window;
    const wait = (ms) => new Promise((r) => setTimeout(r, ms));
    for (let i = 0; i < 40 && typeof w.navGo !== 'function'; i++) await wait(100);
    // v0.31.34：代理页已并入插件，侧导航不再有 egress 项 ——
    // 必须走 openPluginView 进入（这也顺带验证了「插件能打开独立界面」）。
    if (typeof w.openPluginView === 'function') {
      w.openPluginView({ id: 'host-egress', name: '本地出口代理', kind: 'egress', desc: '桩' });
    }
    await wait(700);
    const box = w.document.getElementById('egressHomeBox');
    const txt = box ? String(box.textContent) : '';
    if (!box || !box.children.length) {
      egrSelfBad.push('用户视角的代理页没渲染出任何内容（应能看到自己的机器与操作区）');
    } else {
      if (txt.indexOf('拉起') < 0) {
        egrSelfBad.push('用户视角应出现「一键拉起」按钮（权限已下沉，前端不能仍按只读渲染）');
      }
      if (txt.indexOf('仅管理员') >= 0) {
        egrSelfBad.push('用户视角不该再出现「仅管理员可执行」的旧文案');
      }
      // ⚠️ 断言的是**配置行**形态（`全局默认出口：…`），不是这个词本身：
      // 用户视角的说明文案里**允许**出现"平台的全局默认出口不会被动" ——
      // 那是一句安抚，不是给了一个开关。真正要禁的是把它**列成一行配置**，
      // 那会让人以为"这是我的设置项"。
      if (txt.indexOf('全局默认出口：') >= 0) {
        egrSelfBad.push('用户视角不该把「全局默认出口」列成配置行（那不是他有的开关）');
      }
      if (txt.indexOf('我的代挂号绑定') < 0) {
        egrSelfBad.push('用户视角的绑定计数应叫「我的代挂号绑定」（而不是「自挂号绑定」）');
      }
    }

    // v0.31.31：**逐号出口分配**必须真的渲染出来（这是"两台电脑各出各的 IP"的落地动作）。
    const accBox = w.document.getElementById('egrAccBox');
    const accTxt = accBox ? String(accBox.textContent) : '';
    if (!accBox || !accBox.children.length) {
      egrSelfBad.push('#egrAccBox 没渲染 —— 用户端仍无法逐号指定出口');
    } else {
      // 条数口径写在卡片标题旁（#egrAccHint）：那里才是"共 N 条线路、实际 M 个 IP"。
      const accHint = w.document.getElementById('egrAccHint');
      const hintTxt = accHint ? String(accHint.textContent) : '';
      if (hintTxt.indexOf('2 个不同出口 IP') < 0) {
        egrSelfBad.push(`我的账号出口应按 IP 报数（期望含「2 个不同出口 IP」），实得「${hintTxt.slice(0, 120)}」`);
      }
      if (accTxt.indexOf('出口 IP 1.1.1.1') < 0 || accTxt.indexOf('出口 IP 2.2.2.2') < 0) {
        egrSelfBad.push('逐号列表应显示每个号的实际出口 IP（两个 IP 都要出现）');
      }
      if (accTxt.indexOf('按出口 IP 均衡') < 0) {
        egrSelfBad.push('缺少「按出口 IP 均衡」按钮 —— 用户没有办法一键把号分散到不同出口');
      }
    }
    // v0.31.34：**代理并入插件**，且插件打开后是独立界面（不是卡片里的小面板）。
    //
    // 这一条同时守住三件事：
    //   ① 侧导航不再有「代理」项（否则等于两个入口，状态会打架）；
    //   ② 代理页 DOM 带 data-plugin-view，且带返回栏（用户必须能退出来）；
    //   ③ 点插件卡的「打开」真的整页切过去，再点返回真的回到插件列表。
    const dashSrc = fs.readFileSync(path.join(UI, 'dashboard.html'), 'utf8');
    if (dashSrc.indexOf('data-nav="egress"') >= 0) {
      egrSelfBad.push('用户端侧导航仍有「代理」项 —— 它已并入插件，两个入口会各显示一套状态');
    }
    if (dashSrc.indexOf('data-plugin-view="host-egress"') < 0) {
      egrSelfBad.push('代理页缺少 data-plugin-view 标记，openPluginView 找不到它');
    }
    if (dashSrc.indexOf('data-plugin-back') < 0) {
      egrSelfBad.push('代理页缺少「← 插件」返回栏，用户进去就出不来');
    }

    // 行为：进插件列表 → 点「本地出口代理」的「打开」→ 断言整页切换 → 返回。
    if (typeof w.navGo === 'function') w.navGo('plugins');
    await wait(500);
    const cards = Array.from(w.document.querySelectorAll('#pluginCards .card'));
    if (cards.length !== 3) {
      egrSelfBad.push(`插件页应渲染 3 张卡，实得 ${cards.length}`);
    }
    const egrCardEl = cards.find((c) => String(c.textContent).indexOf('本地出口代理') >= 0);
    if (!egrCardEl) {
      egrSelfBad.push('插件列表里没有「本地出口代理」—— 代理入口彻底丢了');
    } else {
      const openBtn = Array.from(egrCardEl.querySelectorAll('button'))
        .find((b) => String(b.textContent).trim() === '打开');
      if (!openBtn) {
        egrSelfBad.push('「本地出口代理」卡上没有「打开」按钮');
      } else {
        openBtn.click();
        await wait(400);
        const egrPane = w.document.querySelector('.pane[data-pane="egress"]');
        if (!egrPane || !egrPane.classList.contains('active')) {
          egrSelfBad.push('点「打开」后代理页没有变成独立界面（pane 未激活）');
        }
        const title = w.document.getElementById('pageTitle');
        if (!title || String(title.textContent).indexOf('本地出口代理') < 0) {
          egrSelfBad.push('进入插件后标题应改成插件名（用户要知道自己在哪一页）');
        }
        const back = egrPane ? egrPane.querySelector('[data-plugin-back]') : null;
        if (!back) {
          egrSelfBad.push('独立界面缺少返回按钮');
        } else {
          back.click();
          await wait(400);
          const plugPane = w.document.querySelector('.pane[data-pane="plugins"]');
          if (!plugPane || !plugPane.classList.contains('active')) {
            egrSelfBad.push('点「← 插件」没有回到插件列表');
          }
        }
      }
    }

    try { w.close(); } catch (e) {}
  }
  egrSelfBad.forEach((m) => console.log('  [FAIL] egress-self: ' + m));
  if (!egrSelfBad.length) {
    console.log('  [ok] egress-self: 代理并入插件且能进独立界面；用户视角不出现「全局默认出口」');
  }

  // v0.31.32：**重新登录必须能自助完成**（扫码 或 导入 auth JSON），
  // 而不是再给一句"去找管理员"。这条同时守住三处：
  //   · 卡片上的按钮真的调了弹窗（不是 alert 三步提示）；
  //   · 导入入口打的是用户侧接口（不是管理端接口 —— 那会 403）；
  //   · 管理端有"纳入用户线路"的显式开关（借用住宅 IP 必须是知情动作）。
  const rlBad = [];
  {
    const ROOT = path.resolve(UI, '..', '..', '..');
    const dash = fs.readFileSync(path.join(UI, 'dashboard.html'), 'utf8');
    if (!dash.includes('hostReloginDialog(')) {
      rlBad.push('dashboard.html 的「重新登录」没有打开弹窗（用户仍然无自助路径）');
    }
    if (dash.includes("alert('请按这三步操作")) {
      rlBad.push('dashboard.html 仍留旧的"三步去找管理员"提示 —— 那是死路');
    }
    if (!dash.includes("btnHostImport")) {
      rlBad.push('dashboard.html 缺少「导入 JSON」入口');
    }
    if (!APP_JS.includes("'/api/host/accounts/import'")) {
      rlBad.push('app.js 没有调用用户侧导入接口 /api/host/accounts/import');
    }
    if (APP_JS.includes("'/api/admin/accounts/import")) {
      rlBad.push('用户端不得调用管理端导入接口（会 403，且语义是"替平台收号"）');
    }
    const adminHtml = fs.readFileSync(path.join(UI, 'admin.html'), 'utf8');
    if (!adminHtml.includes('egrIncludeUser')) {
      rlBad.push('admin.html 缺少「纳入用户线路」开关 —— 借用住宅 IP 必须显式知情');
    }
    const pluginSrc = fs.readFileSync(path.join(ROOT, 'cmd/webapp/host_import.go'), 'utf8');
    if (!pluginSrc.includes('OwnerUserID != u.ID')) {
      rlBad.push('host_import.go 缺少归属校验（会把别人的号导到自己名下）');
    }
  }
  // v0.31.35：**每个插件都要有独立页面**（不是只有出口代理有）。
  //
  // 现场反馈是"插件点开后缺少可视化的单独界面"—— 当时只有 egress 有专属 pane，
  // claim / health 还挤在卡片里的小面板。这条按**行为**验三种形态：
  // 逐个点开 → 目标 pane 激活、标题变成插件名、内容非空、有返回栏，再返回列表。
  const pvBad = [];
  {
    const html = fs.readFileSync(path.join(UI, 'dashboard.html'), 'utf8')
      .replace('<script src="/app.js"></script>', '<script>' + APP_JS + '</script>');
    const vc = new VirtualConsole();
    vc.on('jsdomError', () => {});
    vc.on('error', () => {});
    const PLUGINS = { plugins: [
      { id: 'claim-compensation', name: '多账号领取积分补偿', desc: '桩', audience: 'both',
        builtin: true, enabled: true, active: true, kind: 'claim' },
      { id: 'health-check', name: '账号健康检测', desc: '桩', audience: 'both',
        builtin: true, enabled: true, active: true, kind: 'health' },
      { id: 'host-egress', name: '本地出口代理', desc: '桩', audience: 'user',
        builtin: true, enabled: true, active: true, kind: 'egress' },
    ] };
    const HEALTH = {
      accounts: [
        { uid: 'p1', nickname: '甲', token_dead: false, checked: true,
          result: { state: 'ok', state_label: '正常', chat_probed: true, chat_ok: true, credits: 100 } },
        { uid: 'p2', nickname: '乙', token_dead: true, checked: false, result: null },
      ],
      summary: { total: 2, checked: 1, definite: 0, ended_at: now },
    };
    const dom = new JSDOM(html, {
      runScripts: 'dangerously', pretendToBeVisual: true, virtualConsole: vc,
      url: 'http://127.0.0.1:8780/dashboard.html',
      beforeParse(win) {
        win.__autoConfirm = true;
        win.__autoPrompt = null;
        win.fetch = (url, opts) => {
          const u = String(url);
          const body = (b) => Promise.resolve({ ok: true, status: 200,
            text: () => Promise.resolve(JSON.stringify(b)), json: () => Promise.resolve(b) });
          if (u.indexOf('/api/auth/me') >= 0) {
            return body({ user: { id: 2, username: 'u2', is_admin: false } });
          }
          if (u.indexOf('/api/plugins') >= 0 && u.indexOf('/claim') >= 0) {
            // dry_run：服务端解析出的目标列表。
            return body({ total: 2, dry_run: true, targets: [
              { uid: 'p1', nickname: '甲' }, { uid: 'p2', nickname: '乙' }] });
          }
          if (u.indexOf('/api/plugins') >= 0 && u.indexOf('/enabled') < 0) return body(PLUGINS);
          if (u.indexOf('/api/host/accounts/health/last') >= 0) return body(HEALTH);
          if (u.indexOf('/api/egress/home') >= 0) {
            return body({ machines: [], agent_online: false, tunnel_reachable: false,
              can_manage: true, manage_scope: 'self', poll_sec: 5 });
          }
          if (u.indexOf('/api/host/egress/lines') >= 0) {
            return body({ lines: [], accounts: [], distinct_exit_ips: 0 });
          }
          return body({});
        };
      },
    });
    const w = dom.window;
    const wait = (ms) => new Promise((r) => setTimeout(r, ms));
    for (let i = 0; i < 40 && typeof w.navGo !== 'function'; i++) await wait(100);
    if (typeof w.navGo === 'function') w.navGo('plugins');
    await wait(500);

    const wants = [
      { name: '多账号领取积分补偿', pane: 'plugin-claim-compensation', must: '开始领取' },
      { name: '账号健康检测', pane: 'plugin-health-check', must: '开始检测' },
      { name: '本地出口代理', pane: 'egress', must: '本地出口代理' },
    ];
    for (const want of wants) {
      const cards = Array.from(w.document.querySelectorAll('#pluginCards .card'));
      const card = cards.find((c) => String(c.textContent).indexOf(want.name) >= 0);
      if (!card) { pvBad.push('插件列表里没有「' + want.name + '」'); continue; }
      const btn = Array.from(card.querySelectorAll('button'))
        .find((b) => String(b.textContent).trim() === '打开');
      if (!btn) { pvBad.push('「' + want.name + '」卡上没有「打开」按钮'); continue; }
      btn.click();
      await wait(450);
      const pane = w.document.querySelector('.pane[data-pane="' + want.pane + '"]');
      if (!pane || !pane.classList.contains('active')) {
        pvBad.push('「' + want.name + '」打开后独立页面未激活（pane=' + want.pane + '）');
        continue;
      }
      const txt = String(pane.textContent || '');
      if (txt.indexOf(want.must) < 0) {
        pvBad.push('「' + want.name + '」独立页面内容为空或缺少关键动作「' + want.must + '」');
      }
      if (!pane.querySelector('[data-plugin-back]')) {
        pvBad.push('「' + want.name + '」独立页面缺少「← 插件」返回栏');
      }
      const title = w.document.getElementById('pageTitle');
      if (!title || String(title.textContent).indexOf(want.name) < 0) {
        pvBad.push('「' + want.name + '」打开后标题没变成插件名');
      }
      const back = pane.querySelector('[data-plugin-back]');
      if (back) back.click();
      await wait(300);
    }

    // 最后再确认一次：所有插件都能回到列表（不是只有第一个能退）。
    const plug = w.document.querySelector('.pane[data-pane="plugins"]');
    if (!plug || !plug.classList.contains('active')) {
      pvBad.push('从插件独立页面返回后，没有回到插件列表');
    }
    try { w.close(); } catch (e) {}
  }
  pvBad.forEach((m) => console.log('  [FAIL] plugin-views: ' + m));
  if (!pvBad.length) {
    console.log('  [ok] plugin-views: claim / health / egress 三种插件都有独立页面且能返回');
  }

  rlBad.forEach((m) => console.log('  [FAIL] host-relogin: ' + m));
  if (!rlBad.length) {
    console.log('  [ok] host-relogin: 重新登录可扫码/导入 JSON 自助完成；借用用户线路需显式勾选');
  }

  // v0.31.33：**账号代理与出口代理的一致化**必须结构性成立，不能靠约定。
  //
  // 规则：凡是"把任意 URL 写进 accounts.egress_proxy"的入口，都必须走
  // `a.setAccountEgress`（它负责规范化 + 登记进线路池）。直接调
  // `a.store.SetAccountEgress` 就等于制造一条"池里没有、也没有出口 IP"的线路 ——
  // 那正是两边 IP 对不上的来源。这条守卫就是防它悄悄长回来。
  const ecBad = [];
  {
    const ROOT = path.resolve(UI, '..', '..', '..');
    const read = (rel) => fs.readFileSync(path.join(ROOT, rel), 'utf8');
    const store = read('cmd/webapp/store.go');
    if (!store.includes('egress.Normalize')) {
      ecBad.push('store.SetAccountEgress 没有规范化 URL —— 同一条代理的两种写法会变成两条线路');
    }
    const adminExt = read('cmd/webapp/admin_ext.go');
    if (adminExt.includes('a.store.SetAccountEgress')) {
      ecBad.push('admin_ext.go 绕过 setAccountEgress 直接写账号出口（不会登记进线路池）');
    }
    const hostSrc = read('cmd/webapp/host.go');
    if (!hostSrc.includes('a.setAccountEgress(')) {
      ecBad.push('host.go 的「我的出口」没有走统一入口（用户填的地址不会进线路池）');
    }
    const align = read('cmd/webapp/egress_align.go');
    if (!align.includes('ensureEgressLineRegistered')) {
      ecBad.push('egress_align.go 缺少自动登记逻辑');
    }
    const adminHtml = fs.readFileSync(path.join(UI, 'admin.html'), 'utf8');
    if (!adminHtml.includes('btnEgrConverge')) {
      ecBad.push('admin.html 缺少「对齐账号出口」入口（历史脏数据没有修复手段）');
    }
    if (!adminHtml.includes('orphan_account_egress')) {
      ecBad.push('admin.html 没有展示"池外出口"计数 —— 不一致时会静默存在');
    }
  }
  ecBad.forEach((m) => console.log('  [FAIL] egress-consistency: ' + m));
  if (!ecBad.length) {
    console.log('  [ok] egress-consistency: 账号出口写入即登记进线路池；页面有池外出口告警与一键对齐');
  }

  // v0.31.25：**还没接入 agent 时，页面必须给出"下载安装包"这条路**。
  //
  // 这一版之前，这里的文案是"请先在目标电脑上运行下面那行命令" —— 用户要自己
  // 复制命令、开终端、核对参数、还要搞到 SSH 密钥。现在整条链路是
  // "点下载 → 双击 .bat → 页面自动检测并接着拉起"，所以这个按钮**必须存在**：
  // 它不存在，整个自动化就没有入口。
  const egrSetupBad = [];
  {
    const html = fs.readFileSync(path.join(UI, 'dashboard.html'), 'utf8')
      .replace('<script src="/app.js"></script>', '<script>' + APP_JS + '</script>');
    const vc = new VirtualConsole();
    vc.on('jsdomError', () => {});
    vc.on('error', () => {});
    const EMPTY_VIEW = {
      machines: [], agent_online: false, tunnel_reachable: false, applied: false, ready: false,
      default_egress: '', host_bound: 0, host_total: 0,
      can_manage: true, manage_scope: 'self', poll_sec: 5, agent_command: '',
    };
    const dom = new JSDOM(html, {
      runScripts: 'dangerously', pretendToBeVisual: true, virtualConsole: vc,
      url: 'http://127.0.0.1:8780/dashboard.html',
      beforeParse(win) {
        win.__autoConfirm = true;
        win.__autoPrompt = null;
        win.__navs = [];
        win.fetch = (url) => {
          const u = String(url);
          if (u.indexOf('/api/egress/home') >= 0) {
            const body = JSON.stringify(EMPTY_VIEW);
            return Promise.resolve({ ok: true, status: 200,
              text: () => Promise.resolve(body), json: () => Promise.resolve(EMPTY_VIEW) });
          }
          if (u.indexOf('/api/auth/me') >= 0) {
            return Promise.resolve({ ok: true, status: 200,
              text: () => Promise.resolve('{"user":{"id":2,"username":"u2","is_admin":false}}'),
              json: () => Promise.resolve({ user: { id: 2, username: 'u2', is_admin: false } }) });
          }
          return Promise.resolve({ ok: true, status: 200,
            text: () => Promise.resolve('{}'), json: () => Promise.resolve({}) });
        };
        win.confirm = () => true; win.prompt = () => '1'; win.alert = () => {};
        win.open = () => ({ closed: false, location: { href: '' }, close() {} });
        win.navigator.clipboard = { writeText: () => Promise.resolve() };
        if (win.Element && !win.Element.prototype.scrollIntoView) {
          win.Element.prototype.scrollIntoView = function () {};
        }
      },
    });
    const w = dom.window;
    const wait = (ms) => new Promise((r) => setTimeout(r, ms));
    for (let i = 0; i < 40 && typeof w.navGo !== 'function'; i++) await wait(100);
    // v0.31.34：代理页已并入插件，侧导航不再有 egress 项 ——
    // 必须走 openPluginView 进入（这也顺带验证了「插件能打开独立界面」）。
    if (typeof w.openPluginView === 'function') {
      w.openPluginView({ id: 'host-egress', name: '本地出口代理', kind: 'egress', desc: '桩' });
    }
    await wait(700);
    const box = w.document.getElementById('egressHomeBox');
    const txt = box ? String(box.textContent) : '';
    // 页面说明段（pane 顶部）也要看：安装引导有一半写在那里，
    // 只查 #egressHomeBox 会漏掉"在目标电脑上双击运行"这句关键动作。
    const page = w.document.body ? String(w.document.body.textContent) : '';
    if (!box || !box.children.length) {
      egrSetupBad.push('未接入时代理页应渲染出安装引导，实为空');
    } else {
      if (txt.indexOf('下载本机代理安装包') < 0) {
        egrSetupBad.push('未接入 agent 时应出现「下载本机代理安装包」按钮（自动化入口）');
      }
      if (page.indexOf('双击') < 0) {
        egrSetupBad.push('应说明"在目标电脑上双击运行"（这是唯一的用户动作）');
      }
      if (txt.indexOf('无需手动') < 0 && page.indexOf('不需要复制粘贴') < 0) {
        egrSetupBad.push('应说明"无需复制粘贴任何参数"（这是本版要解决的问题）');
      }
      // ⚠️ 刻意**不**断言"没有复制命令"：bat 只适用于 Windows，
      // macOS / Linux 用户仍需要那行命令作为 fallback（页面同时给两条路是对的）。
    }
    try { w.close(); } catch (e) {}
  }
  egrSetupBad.forEach((m) => console.log('  [FAIL] egress-setup: ' + m));
  if (!egrSetupBad.length) {
    console.log('  [ok] egress-setup: 未接入时有「下载安装包」入口（无需复制任何参数）');
  }

  const c = await run('admin-login.html', null, []);
  const d = await run('index.html', null, []);
  const allOk = a && b && b2 && c && d && dupBad.length === 0 && probeBad.length === 0
    && delBad.length === 0 && routeBad.length === 0 && switchBad.length === 0
    && egrBad.length === 0 && egrSelfBad.length === 0 && egrSetupBad.length === 0;
  console.log('\nOVERALL: ' + (allOk ? 'PASS' : 'FAIL'));
  process.exit(allOk ? 0 : 1);
})();