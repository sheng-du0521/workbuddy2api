// app.js —— 全站共享的前端工具（无依赖，原生 DOM）。
// 页面结构约定：
//   侧边栏导航 → .nav-item[data-nav][data-on]；内容区 → .pane[data-pane]
//   路由/切换由 setupNav() 统一接管；登录页仍用 setupTabs()（顶部 tab）。

const $ = (id) => document.getElementById(id);

// toast 的实现见文件末尾「共享 UI 套件」段（v0.26.6 改为**堆叠卡片**：
// 图标 / 可关闭 / 悬停暂停 / aria-live）。旧实现是单元素、2.8 秒自动消失，
// 错误提示切个页就找不到了 —— 而"刚才失败过"恰恰是最需要留下的信息。
// 签名保持 toast(msg, kind) 兼容，第三参可选 { duration, sticky }。

async function api(path, opts = {}) {
  const res = await fetch(path, Object.assign({ credentials: 'same-origin' }, opts, {
    headers: Object.assign({ 'Content-Type': 'application/json' }, opts.headers || {}),
  }));
  const text = await res.text();
  let data;
  try { data = JSON.parse(text); } catch (e) { data = { _raw: text }; }
  if (!res.ok) {
    // 错误信息三级回落：error 字符串 → error.message（对象型）→ 原文/状态码。
    // 修复点：旧写法 `data.error || data.error.message` 在 error 为对象时
    // 会直接命中第一个操作数，把对象交给 new Error() → 界面只显示 "[object Object]"，
    // 完全看不出服务端说了什么。
    const errField = data && data.error;
    let msg = '';
    if (typeof errField === 'string') msg = errField;
    else if (errField && typeof errField === 'object') msg = errField.message || safeJSON(errField);
    if (!msg) msg = (data && data._raw) || ('HTTP ' + res.status);
    const err = new Error(msg);
    err.status = res.status;
    // err.data 携带**完整的错误响应体**（v0.25.0）。有些接口的拒绝不是一句话能说清的：
    // 导入防关联门禁被拒时要回传 guard（当前 IP / 上次 IP / 该怎么做），前端得据此
    // 渲染出可操作的引导，而不是把 "出口 IP 与上次导入相同" 这句干巴巴地丢给用户。
    err.data = data;
    throw err;
  }
  return data;
}

// safeJSON 序列化失败时不抛（错误提示本身不该再引发异常）。
function safeJSON(v) {
  try { return JSON.stringify(v); } catch (e) { return String(v); }
}

async function post(path, body) {
  return api(path, { method: 'POST', body: JSON.stringify(body || {}) });
}

async function currentUser() {
  try {
    const d = await api('/api/auth/me');
    return d.user;
  } catch (e) {
    return null;
  }
}

async function requireLogin() {
  const u = await currentUser();
  if (!u) { location.href = '/'; return null; }
  return u;
}

// requireAdmin 管理后台守卫：未登录 / 非管理员一律跳独立「管理登录页」。
// 与用户控制台（requireLogin → '/'）分属两条入口，互不复用。
async function requireAdmin() {
  const u = await currentUser();
  if (!u || !u.is_admin) { location.href = '/admin-login.html'; return null; }
  return u;
}

async function logout() {
  try { await post('/api/auth/logout'); } catch (e) { /* 忽略 */ }
  location.href = '/';
}

// ---------------- 格式化 ----------------
//
// ============================================================================
// 三套数值口径的**唯一**格式化入口（v0.17.7）——改动前先读 cmd/webapp/money.go
// ============================================================================
// 系统里同时存在三种数，此前混着叫，极易误读。统一规则如下：
//
//   ① 点（平台点数）—— 余额、消费、日/月限额、提现申请额。
//      fmtPoints：≤3 位小数 + 千分位，数字后写「点」。
//   ② 钱（金额，元）—— 「点 × 折算比例」。返点**一律**用它。
//      fmtMoney：¥ + 固定 2 位小数 + 千分位。
//   ③ 号池积分（上游账号 credits）—— 与「点」是两种货币，只出现在账号维度。
//      fmtCredit：≤2 位小数 + 千分位，数字后写「号池积分」。
//
// 禁止再新增第四种写法：需要格式化时一律从这三个里选，不要就地 toLocaleString。
// ============================================================================

// fmtPoints 平台点数（点）：≤3 位小数 + 千分位。
function fmtPoints(v) {
  const n = Number(v || 0);
  if (!isFinite(n)) return '—';
  return n.toLocaleString('zh-CN', { maximumFractionDigits: 3 });
}

// fmtPointsU 点数带单位（「1,234 点」）。
function fmtPointsU(v) { return fmtPoints(v) + ' 点'; }

// fmtMoney 金额（元）：¥ + 固定 2 位小数 + 千分位。
// 实现与 fmtCNY 同为一份；fmtCNY 保留为别名（历史调用点较多，且语义等价）。
function fmtMoney(v) {
  const n = Number(v || 0);
  if (!isFinite(n)) return '—';
  return '¥' + n.toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}
function fmtCNY(v) { return fmtMoney(v); }

// fmtMoneySigned 金额带正负号（返点入账用「+¥1.20」）。
function fmtMoneySigned(v, sign) {
  const n = Number(v || 0);
  if (!isFinite(n)) return '—';
  const s = (sign === undefined ? (n > 0 ? '+' : '') : sign);
  return s + fmtMoney(Math.abs(n));
}

// fmtCredit 号池积分（上游账号 credits）：≤2 位小数 + 千分位。
// 与 fmtAmt 同为一份数值格式，但语义命名独立 —— 用它才能在调用点自证单位。
function fmtCredit(v) { return fmtAmt(v); }

// fmtCreditU 号池积分带单位（「1,234.56 号池积分」）。
function fmtCreditU(v) { return fmtCredit(v) + ' 号池积分'; }

// cnyOf 点数 → 金额（元）。rate 为「元 / 点」（后端各接口均返回该值）。
// 只在后端未提供 *_cny 字段时使用；能用后端字段就用（口径唯一，避免前端二次换算）。
function cnyOf(points, rate) { return Number(points || 0) * Number(rate || 0); }

// fmtRate 折算比例展示（元 / 点）。同时给出「1 元 ≈ N 点」的反向口径，
// 因为用户更常需要「这些钱等于多少点」，而配置里填的是元/点。
function fmtRate(rate) {
  const r = Number(rate || 0);
  if (!isFinite(r) || r <= 0) return '折算比例 —';
  const inverse = 1 / r;
  const invTxt = inverse >= 100 ? String(Math.round(inverse)) : inverse.toFixed(inverse >= 10 ? 0 : 2);
  return '折算 ' + r + ' 元/点（1 元 ≈ ' + invTxt + ' 点）';
}

function fmtTime(ts) {
  if (!ts) return '—';
  const d = new Date(ts * 1000);
  const p = (x) => String(x).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

function fmtClock(ts) {
  if (!ts) return '—';
  const d = new Date(ts * 1000);
  const p = (x) => String(x).padStart(2, '0');
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function fmtDate(ts) {
  if (!ts) return '—';
  const d = new Date(ts * 1000);
  const p = (x) => String(x).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

function fmtNum(n) { return Number(n || 0).toLocaleString('zh-CN'); }

// fmtUptime 秒 → 人类可读（1d 3h / 2h 5m / 45s）。
function fmtUptime(sec) {
  sec = Math.max(0, Math.floor(Number(sec) || 0));
  const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
  if (d > 0) return d + ' 天 ' + h + ' 小时';
  if (h > 0) return h + ' 小时 ' + m + ' 分';
  if (m > 0) return m + ' 分 ' + (sec % 60) + ' 秒';
  return sec + ' 秒';
}

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast('已复制到剪贴板', 'ok');
  } catch (e) {
    toast('复制失败，请手动选择', 'err');
  }
}

// ---------------- DOM 小工具 ----------------

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function setTxt(id, v) { const e = $(id); if (e) e.textContent = v; }

function escapeHTML(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
  ));
}

function emptyRow(tbody, cols, text) {
  const tr = el('tr');
  const td = el('td', 'muted', text);
  td.colSpan = cols;
  tr.appendChild(td);
  tbody.appendChild(tr);
}

function emptyBlock(box, text) {
  box.textContent = '';
  box.appendChild(el('div', 'feed-empty', text));
}

// avatarTone 由名字派生一个稳定的色号（a1..a6），让卡片头像有辨识度。
function avatarTone(name) {
  name = String(name || '?');
  let h = 0;
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) >>> 0;
  return 'a' + ((h % 6) + 1);
}

function maskUID(uid) {
  if (!uid) return '—';
  if (uid.length <= 10) return uid;
  return uid.slice(0, 4) + '···' + uid.slice(-4);
}

// ---------------- 代挂账号：两页共享的状态与任务模型（v0.24.2，P3-13） ----------------
//
// 为什么下沉到这里：这三个函数此前在 admin.html 与 dashboard.html 里**各存一份**，
// 而且是「同源分叉」——两份实现只差一行（pending 的标签一个写「待审」、一个写「待审核」），
// 谁都不知道该以哪份为准。这类重复不会立刻报错，只会在某天「只改了一边」时
// 表现为「管理端显示对了、用户端还是旧的」——最难查的一类缺陷。
// 现在只有一份，主色/文案/模型形状都从这里走。

// hostStateTag 代挂账号的入池状态标签（in / pending / rejected / 其它=未入池）。
// 文案统一取「待审核」（此前管理端写「待审」、用户端写「待审核」，本次对齐为后者）。
function hostStateTag(ps) {
  if (ps === 'in') return el('span', 'tag ok', '已入池');
  if (ps === 'pending') return el('span', 'tag warn', '待审核');
  if (ps === 'rejected') return el('span', 'tag err', '已驳回');
  return el('span', 'tag', '未入池');
}

// taskModelOf 把后端状态收敛成本地模型（保留逐项「意图」，总开关关闭也不丢）。
function taskModelOf(t) {
  t = t || {};
  return { enabled: t.enabled !== false, disabled: new Set(t.disabled || []) };
}

// taskModelToJSON 本地模型 → 落库形状（Set 必须转回数组，否则 JSON.stringify 得到 {}）。
function taskModelToJSON(m) {
  return { enabled: m.enabled, disabled: Array.from(m.disabled) };
}

// ---------------- 导航 ----------------

// setupTabs 顶部 tab（登录页用）。
function setupTabs(root) {
  const tabs = Array.from(root.querySelectorAll('.tab'));
  const panes = Array.from(root.querySelectorAll('.pane'));
  tabs.forEach((t) => {
    t.onclick = () => {
      tabs.forEach((x) => x.classList.remove('active'));
      panes.forEach((x) => x.classList.remove('active'));
      t.classList.add('active');
      const pane = root.querySelector('.pane[data-pane="' + t.dataset.tab + '"]');
      if (pane) pane.classList.add('active');
      if (typeof t.dataset.on === 'string' && window[t.dataset.on]) window[t.dataset.on]();
    };
  });
}

// setupNav 侧边栏导航：绑定 .nav-item ↔ .pane，切换时调用 data-on 指定的加载函数。
// 同时把题目/副标题切到 data-title/data-sub（缺省则不动），并记住当前页（hash）。
function setupNav(root) {
  const items = Array.from(root.querySelectorAll('.nav-item'));
  const panes = Array.from(root.querySelectorAll('.pane'));
  const go = (name, silent) => {
    const item = items.find((x) => x.dataset.nav === name);
    if (!item) return false;
    items.forEach((x) => x.classList.remove('active'));
    panes.forEach((x) => x.classList.remove('active'));
    item.classList.add('active');
    const pane = root.querySelector('.pane[data-pane="' + name + '"]');
    if (pane) pane.classList.add('active');
    if (item.dataset.title) setTxt('pageTitle', item.dataset.title);
    if (item.dataset.sub !== undefined) setTxt('pageSub', item.dataset.sub);
    if (!silent) { try { location.hash = name; } catch (e) { /* 忽略 */ } }
    const fn = item.dataset.on;
    if (typeof fn === 'string' && window[fn]) window[fn]();
    // 全局切页钩子：页面用它启停轮询 / 释放资源（如离开实时页就停掉定时器）。
    if (typeof window.onNavChange === 'function') window.onNavChange(name);
    return true;
  };
  items.forEach((it) => { it.onclick = () => go(it.dataset.nav); });
  const initial = (location.hash || '').replace('#', '');
  if (!initial || !go(initial, true)) {
    const first = items.find((x) => x.classList.contains('active')) || items[0];
    if (first) go(first.dataset.nav, true);
  } else {
    // go() 已触发加载
  }
  window.navGo = go;
  return go;
}

// ---------------- 迷你柱状图 ----------------

// sparkbars 把一组 {label, req, fail, tokens} 画成柱状图。
// 柱高按本组最大值归一；有失败的分钟标红；零请求画一条极矮的灰柱占位。
function sparkbars(box, series, opts) {
  opts = opts || {};
  box.textContent = '';
  const max = Math.max(1, ...series.map((p) => Number(p.req) || 0));
  for (const p of series) {
    const req = Number(p.req) || 0;
    const fail = Number(p.fail) || 0;
    const b = el('div', 'bar' + (fail > 0 ? ' bad' : '') + (req === 0 ? ' empty' : ''));
    b.style.height = (req === 0 ? 3 : Math.max(7, Math.round((req / max) * 100))) + '%';
    const parts = [p.label ? p.label + ' · ' : '', req + ' 次'];
    if (fail > 0) parts.push(fail + ' 失败');
    if (p.tokens) parts.push(fmtNum(p.tokens) + ' tokens');
    b.title = parts.join(' · ');
    box.appendChild(b);
  }
  if (opts.axis) {
    box._axis = series;
  }
}

// progressPct 计算进度百分比（上限 100，返回 0-100 整数）。
function progressPct(used, limit) {
  used = Number(used) || 0;
  limit = Number(limit) || 0;
  if (limit <= 0) return 0;
  return Math.min(100, Math.round((used / limit) * 100));
}

// progressEl 生成一个进度条 + 说明标签的组合。
function progressEl(used, limit, label) {
  const wrap = el('div');
  const pct = progressPct(used, limit);
  const cls = pct >= 90 ? ' err' : (pct >= 70 ? ' warn' : '');
  const bar = el('div', 'progress' + cls);
  const fill = el('i');
  fill.style.width = pct + '%';
  bar.appendChild(fill);
  wrap.appendChild(bar);
  const lab = el('div', 'pg-label');
  lab.appendChild(el('span', '', label));
  lab.appendChild(el('span', '', pct + '%'));
  wrap.appendChild(lab);
  return wrap;
}

// ---------------- 积分卡片（对齐参考项目：大总额 + N 个积分包 + 逐包进度条/到期） ----------------

// fmtAmt 积分数值展示（2 位小数上限，与参考项目一致）。
function fmtAmt(v) {
  const n = Number(v || 0);
  if (!isFinite(n)) return '—';
  return n.toLocaleString('zh-CN', { maximumFractionDigits: 2 });
}

// creditExpiryText 到期文案：无到期 = 长期有效，否则 MM/DD 到期。
function creditExpiryText(ts) {
  if (!ts) return '长期有效';
  const d = new Date(ts * 1000);
  if (isNaN(d.getTime())) return '长期有效';
  const p = (x) => String(x).padStart(2, '0');
  return p(d.getMonth() + 1) + '/' + p(d.getDate()) + ' 到期';
}

// creditExpiryClass 到期色标：已过期=err，7 天内=warn，其余=muted。
function creditExpiryClass(ts) {
  if (!ts) return '';
  const ms = ts * 1000 - Date.now();
  if (ms < 0) return 'err';
  if (ms < 7 * 86400 * 1000) return 'warn';
  return '';
}

// creditPackRow 单个积分包：剩余/总量 + 名称 + 到期 + 进度条。
function creditPackRow(r) {
  const wrap = el('div', 'pack');
  const size = Number(r.size) || 0;
  const remain = Number(r.remain) || 0;
  const pct = size > 0 ? Math.min(100, Math.max(0, Math.round((remain / size) * 100))) : 0;
  const top = el('div', 'pack-top');
  top.appendChild(el('span', 'pack-amt', fmtAmt(remain) + ' 积分'));
  top.appendChild(el('span', 'pack-name', r.name || '积分包'));
  const exp = el('span', 'pack-exp ' + creditExpiryClass(r.expire_at), creditExpiryText(r.expire_at));
  top.appendChild(exp);
  wrap.appendChild(top);
  const bar = el('div', 'pack-bar' + (creditExpiryClass(r.expire_at) === 'err' ? ' err' : (creditExpiryClass(r.expire_at) === 'warn' ? ' warn' : '')));
  const fill = el('i');
  fill.style.width = pct + '%';
  bar.appendChild(fill);
  wrap.appendChild(bar);
  wrap.title = (r.name || '积分包') + ' · 剩余 ' + fmtAmt(remain) + ' / ' + fmtAmt(size) + ' · ' + creditExpiryText(r.expire_at);
  return wrap;
}

// creditModal 全部积分包弹窗。
function creditModal(title, d, rows) {
  const mask = el('div', 'modal-mask');
  const box = el('div', 'modal');
  const h = el('div', 'modal-head');
  h.appendChild(el('b', '', '全部积分包'));
  const close = el('button', 'modal-x', '×');
  close.onclick = () => mask.remove();
  h.appendChild(close);
  box.appendChild(h);
  box.appendChild(el('div', 'modal-sub', (title || '') + ' · 共 ' + rows.length + ' 个积分包'));
  const body = el('div', 'modal-body');
  for (const r of rows) body.appendChild(creditPackRow(r));
  box.appendChild(body);
  mask.appendChild(box);
  mask.onclick = (e) => { if (e.target === mask) mask.remove(); };
  document.body.appendChild(mask);
}

// creditRowsTotal 明细行的可用合计（排除已过期包）。
//
// ⚠️ 这是**服务端 cachedRemain 的镜像口径**，只用于「服务端字段缺失」时的兜底。
// 改这里的判断时，务必同步改 Go 侧的 cachedRemain（cmd/webapp/creditcache.go）——
// 两处口径不一致会让"卡片总额"与"接口给的 remain"在两个页面上打架。
function creditRowsTotal(rows) {
  const now = Math.floor(Date.now() / 1000);
  return (rows || []).reduce((s, r) => {
    if (r && r.expire_at && r.expire_at < now) return s;
    return s + (Number(r && r.remain) || 0);
  }, 0);
}

// creditSnapshotLine 账号卡片的积分区块（v0.24.3 起；v0.24.9 恢复明细展示）：
// **默认用「明细缓存」渲染（零出站），点按钮才真实出站刷新。**
//
// ⚠️ 两件事必须同时成立，改这里之前先读完这段：
//
//  1. **绝不能渲染即出站**（2026-10-13 封号事故的根因）：
//     此前这里是 `creditPanel(url, title)`，一渲染就发 GET
//     `/api/{admin,host}/accounts/<uid>/resources`，那个接口会**真实出站**查上游积分。
//     账号池页有几十张卡片 → 几十个**并发**请求、同一个出口 IP、几秒内打完 →
//     上游风控当场判「请求非法(11140)」→ 30 个健康账号被误判停用。
//     也就是说：**管理员打开页面看一眼，本身就是一次风控事件。**
//
//  2. **但也不能只剩一个总数**（v0.24.3~v0.24.8 的实际后果）：
//     卡片上只剩总额快照，看不到「哪些包、哪天到期、哪个快过期」—— 而这正是运维
//     判断「这个号还能顶多久」的依据。于是 v0.24.9 把上一次**成功查询**的明细
//     由服务端落库、随列表接口一起下发（a.credit_packs / a.credit_packs_at），
//     这里直接画出来：**零出站拿到明细**，点「刷新明细」才出站（仍受探活总闸约束）。
//
// 快照数字（a.credits）与明细缓存（a.credit_packs）都来自列表接口，零额外出站。
// 详见 docs/封号事件诊断与账号隔离核对-20261013.md 与 creditcache.go。
function creditSnapshotLine(a, btnLabel, url, title) {
  const box = el('div', 'credit');
  // 只有明确 false 才视为「未获取」：字段缺失时按已知处理，避免把有值的号显示成「—」。
  const known = (a && a.credits_known) !== false;
  const cached = (a && Array.isArray(a.credit_packs)) ? a.credit_packs : [];
  const at = Number((a && a.credit_packs_at) || 0);
  // 过滤/排序与 loadCreditPanel 逐字一致：「缓存态」与「刚查完态」看起来必须没有差别，
  // 否则运维会以为缓存数据是另一套口径（同一件事两套呈现＝本项目反复踩过的坑）。
  const rows = cached.filter((r) => Number(r.remain) > 0)
    .sort((x, y) => (x.expire_at || Infinity) - (y.expire_at || Infinity));
  // 总额取值（v0.25.0）：**有缓存就用缓存明细的合计**，没有才回落池内快照。
  //
  // 为什么不一律用池内快照（a.credits）：池内那个数是「逐次请求扣减出来的内存快照」，
  // 会与下面列出的明细行对不上（大字说 4437、三行加起来 3800）。缓存明细是上一次
  // 真实查询的逐包结果，与明细行**同一份数据** —— 用它就永远自洽。
  const cachedAt = Number((a && a.credit_packs_at) || 0);
  const cachedRemain = Number((a && a.credit_cached_remain));
  const totalKnown = cachedAt > 0 ? true : known;
  const totalVal = cachedAt > 0
    // 服务端字段优先；**取不到时按明细行自算兜底**。
    //
    // 为什么要兜底（v0.25.0 真机踩到）：补新器就地重画卡片时漏传了这个字段，
    // 于是卡片总额显示成 0 —— 一个"字段名漏传就静默变 0"的接口是脆的。
    // 明细行反正就在手里，兜底一次的成本极低，却能把这类脆弱性一次性消掉。
    // ⚠️ 兜底口径必须与服务端 cachedRemain 一致：排除已过期包。
    ? (Number.isFinite(cachedRemain) ? cachedRemain : creditRowsTotal(rows))
    : ((a && a.credits) || 0);

  // render 用**缓存**画区块。正常渲染与「刷新失败后回退」共用它 —— 这样限流或上游抖动
  // 时页面不会变空（旧行为是整块被错误信息替换，明细消失，看起来就像功能坏了）。
  const render = () => {
    const frag = document.createDocumentFragment();
    const head = el('div', 'credit-head');
    const big = el('div', 'credit-total');
    big.appendChild(el('b', '', totalKnown ? fmtAmt(totalVal) : '—'));
    big.appendChild(el('span', 'u', '积分'));
    head.appendChild(big);
    head.appendChild(el('span', 'credit-packs',
      rows.length ? (cached.length + ' 个积分包') : (known ? '快照值 · 点开查明细' : '尚未获取权威积分')));
    if (at) head.appendChild(el('span', 'credit-upd', fmtClock(at) + ' 更新'));
    frag.appendChild(head);

    if (rows.length) {
      frag.appendChild(el('div', 'credit-sub', '近期到期'));
      const visible = rows.slice(0, 3);
      for (const r of visible) frag.appendChild(creditPackRow(r));
      if (rows.length > visible.length) {
        const more = el('button', 'credit-more', '查看全部 ' + rows.length + ' 个积分包');
        // 弹窗直接用缓存行，**同样零出站**。
        more.onclick = () => creditModal(title, null, rows);
        frag.appendChild(more);
      }
    }
    const btn = el('button', 'credit-more', rows.length ? '刷新明细' : btnLabel);
    btn.onclick = () => {
      box.textContent = '';
      box.appendChild(el('div', 'muted', '积分查询中…'));
      loadCreditPanel(box, url, title, render);
    };
    frag.appendChild(btn);
    return frag;
  };
  box.appendChild(render());
  return box;
}

// creditPanel 生成账号卡片的积分区块（异步拉取）。
// url 返回 {ok, remain, used, size, packs, expiring_soon, resources, updated_at}。
function creditPanel(url, title) {
  const box = el('div', 'credit');
  box.appendChild(el('div', 'muted', '积分查询中…'));
  loadCreditPanel(box, url, title);
  return box;
}

// fallback（可选，v0.24.9）：查询失败时用**缓存**重画的渲染函数。
// 有它就不会「一点刷新，明细整块消失」——探活总闸限流（30s 内刚查过）与上游抖动
// 都属常态，页面必须留在「上次的好数据 + 一条错误提示」这个状态。
async function loadCreditPanel(box, url, title, fallback) {
  let d;
  try { d = await api(url); } catch (e) { d = { ok: false, error: e.message }; }
  box.textContent = '';
  if (!d.ok) {
    const err = el('div', 'credit-err');
    err.appendChild(el('span', '', '◈'));
    err.appendChild(el('span', '', d.error || '积分查询失败'));
    box.appendChild(err);
    if (fallback) box.appendChild(fallback());
    return;
  }
  const head = el('div', 'credit-head');
  const big = el('div', 'credit-total');
  big.appendChild(el('b', '', fmtAmt(d.remain)));
  big.appendChild(el('span', 'u', '积分'));
  head.appendChild(big);
  head.appendChild(el('span', 'credit-packs', (d.packs || 0) + ' 个积分包'));
  if (d.updated_at) head.appendChild(el('span', 'credit-upd', fmtClock(d.updated_at) + ' 更新'));
  box.appendChild(head);

  const rows = (d.resources || []).filter((r) => Number(r.remain) > 0)
    .sort((a, b) => (a.expire_at || Infinity) - (b.expire_at || Infinity));
  if (!rows.length) { box.appendChild(el('div', 'muted', '暂无可用积分')); return; }
  box.appendChild(el('div', 'credit-sub', '近期到期'));
  const visible = rows.slice(0, 3);
  for (const r of visible) box.appendChild(creditPackRow(r));
  if (rows.length > visible.length) {
    const more = el('button', 'credit-more', '查看全部 ' + rows.length + ' 个积分包');
    more.onclick = () => creditModal(title, d, rows);
    box.appendChild(more);
  }
}

// ---------------- 账号任务完成情况 ----------------

// taskStatusPanel 账号卡片上的「任务完成情况」（今日签到 + 猫猫旅行），懒加载。
//
// 为什么要有它：自动任务此前只有「开关」，卡片上完全看不出今天到底跑没跑成，
// 管理员只能等下一轮跑完翻日志。参考实现（switch2api 的 account-card）是在卡片上
// 直接打「已签到 / 未签到」+ 旅行状态徽标，这里按同一口径做。
//
// 数据源 GET → { checkin:{supported,checked_in,ok,error},
//                travel:{supported,ok,state,daily_limit_reached,reward_credit,error},
//                cached }；后端有 30s 缓存，重复渲染不会反复打上游。
function taskStatusPanel(url) {
  const box = el('div', 'tstatus');
  box.appendChild(el('span', 'muted', '任务状态查询中…'));
  loadTaskStatus(box, url);
  return box;
}

async function loadTaskStatus(box, url) {
  let d;
  try { d = await api(url); } catch (e) { d = { error: e.message }; }
  box.textContent = '';
  if (d.error) {
    box.appendChild(el('span', 'tag err', '任务状态：' + d.error));
    return;
  }
  const c = d.checkin || {};
  const t = d.travel || {};
  // 签到
  if (c.supported === false) box.appendChild(el('span', 'tag', '签到：不支持'));
  else if (!c.ok) box.appendChild(chipWithTitle('签到：查询失败', 'err', c.error));
  else if (c.checked_in) box.appendChild(el('span', 'tag ok', '✓ 今日已签到'));
  else box.appendChild(el('span', 'tag warn', '今日未签到'));
  // 猫猫旅行
  if (t.supported === false) box.appendChild(el('span', 'tag', '旅行：不支持'));
  else if (!t.ok) box.appendChild(chipWithTitle('旅行：查询失败', 'err', t.error));
  else if (t.state === 'arrived') box.appendChild(el('span', 'tag ok', '到站待领 ' + fmtAmt(t.reward_credit)));
  else if (t.state === 'traveling') box.appendChild(el('span', 'tag accent', '猫猫旅行中'));
  else if (t.state === 'no-buddy') box.appendChild(el('span', 'tag', '无猫 · 待领养'));
  else if (t.daily_limit_reached) box.appendChild(el('span', 'tag', '今日已派出'));
  else box.appendChild(el('span', 'tag warn', '今日未派出'));
}

// chipWithTitle 带 title 提示的标签（失败原因放 title，避免卡片被长文撑开）。
function chipWithTitle(text, cls, title) {
  const span = el('span', 'tag ' + (cls || ''), text);
  if (title) span.title = title;
  return span;
}

// ---------------- 轮询 ----------------

// poll 每 ms 毫秒跑一次 fn；页面隐藏时跳过（省流量），返回停止函数。
//
// 停止函数必须**同时**解绑 visibilitychange 监听器（v0.17.6 修复）：
// 此前只 clearInterval，监听器留在 document 上永不回收 —— 每次「切到实时页 → 切走」
// 都累积一个。累积 N 个后，每次把标签页切回前台会**同时触发 N 次 tick**，
// 瞬时打出 N 份 /api/admin/live + /api/admin/pool-dashboard（实测 3 次往返后
// 切回前台 = 6 个并发请求）。旧监听器还会在新一轮 poll 已经停掉之后继续调 fn。
function poll(ms, fn) {
  let busy = false;
  const tick = async () => {
    if (document.hidden || busy) return;
    busy = true;
    try { await fn(); } finally { busy = false; }
  };
  const t = setInterval(tick, ms);
  const onVisible = () => { if (!document.hidden) tick(); };
  document.addEventListener('visibilitychange', onVisible);
  return () => {
    clearInterval(t);
    document.removeEventListener('visibilitychange', onVisible);
  };
}

// ======================================================================
// v0.11.0 · 参考项目（WorkBuddy-Switch2api / shdcn）对齐的组件基元
//   · icon 线性图标（内联 SVG，跟随 currentColor）
//   · iconBtn 图标按钮（刷新类：执行期间自动旋转）
//   · switchEl 药丸开关
//   · segmented 分段控件
//   · svg 图表：折线/柱、环形占比、横向条
// ======================================================================

// ICONS 线性图标路径（24×24 viewBox，stroke 风格，与 lucide 同构）。
const ICONS = {
  refresh: 'M21 12a9 9 0 1 1-2.6-6.4M21 4v5h-5',
  plus: 'M12 5v14M5 12h14',
  check: 'M20 6 9 17l-5-5',
  copy: 'M9 9h10a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2h-9a2 2 0 0 1-2-2v-9a2 2 0 0 1 2-2zM5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1',
  trash: 'M3 6h18M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6',
  save: 'M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11l5 5v11a2 2 0 0 1-2 2zM17 21v-8H7v8M7 3v5h8',
  users: 'M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2M9 3a4 4 0 1 1 0 8 4 4 0 0 1 0-8zM22 21v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75',
  list: 'M11 12H3M16 6H3M16 18H3M18 9v6M21 12h-6',
  external: 'M15 3h6v6M10 14 21 3M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6',
  wallet: 'M21 12V7H5a2 2 0 0 1 0-4h14v4M3 5v14a2 2 0 0 0 2 2h16v-5M18 12a2 2 0 0 0 0 4h4v-4z',
  chart: 'M12 20V10M18 20V4M6 20v-4',
  x: 'M18 6 6 18M6 6l12 12',
  ban: 'M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20zM4.9 4.9l14.2 14.2',
  checkCircle: 'M22 11.1V12a10 10 0 1 1-5.9-9.1M9 11l3 3L22 4',
  clock: 'M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20zM12 6v6l4 2',
  power: 'M12 2v10M18.4 6.6a9 9 0 1 1-12.8 0',
  gift: 'M20 12v10H4V12M2 7h20v5H2zM12 22V7M12 7H7.5a2.5 2.5 0 0 1 0-5C11 2 12 7 12 7zM12 7h4.5a2.5 2.5 0 0 0 0-5C13 2 12 7 12 7z',
  down: 'M12 5v14M19 12l-7 7-7-7',
  up: 'M12 19V5M5 12l7-7 7 7',
  // 密钥显隐（v0.17.8）：eye = 查看完整密钥，eyeOff = 收起回掩码。
  eye: 'M1 12s4-7 11-7 11 7 11 7-4 7-11 7-11-7-11-7zM12 9a3 3 0 1 0 0 6 3 3 0 0 0 0-6z',
  eyeOff: 'M17.9 17.9A10.4 10.4 0 0 1 12 19C5 19 1 12 1 12a18.9 18.9 0 0 1 5.1-5.9M9.9 4.2A10.6 10.6 0 0 1 12 4c7 0 11 7 11 7a18.6 18.6 0 0 1-2.2 3.2M1 1l22 22M9.9 9.9a3 3 0 0 0 4.2 4.2',
};

// icon 生成一个线性图标元素（跟随 currentColor，尺寸默认 14）。
function icon(name, size) {
  const s = size || 14;
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('width', s);
  svg.setAttribute('height', s);
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', '2');
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  svg.setAttribute('class', 'ic');
  const p = document.createElementNS('http://www.w3.org/2000/svg', 'path');
  p.setAttribute('d', ICONS[name] || ICONS.check);
  svg.appendChild(p);
  return svg;
}

// iconBtn 图标按钮。opts={icon,label,title,cls,onClick}
// 若 onClick 返回 Promise，按钮在等待期间自动进入 spin（刷新类按钮用）。
function iconBtn(opts) {
  const b = el('button', 'icon-btn ' + (opts.cls || ''));
  if (opts.title) b.title = opts.title;
  const ic = icon(opts.icon || 'refresh');
  b.appendChild(ic);
  if (opts.label) b.appendChild(el('span', '', opts.label));
  b.onclick = async () => {
    if (typeof opts.onClick !== 'function') return;
    try {
      const r = opts.onClick();
      if (r && typeof r.then === 'function') {
        b.classList.add('spin');
        b.disabled = true;
        await r;
      }
    } finally {
      b.classList.remove('spin');
      b.disabled = false;
    }
  };
  return b;
}

// switchEl 药丸开关。返回 <label class="switch"><input><i></i></label>
// onChange(checked) 可返回 Promise（等待期间禁用，防连点）。
function switchEl(checked, onChange, ariaLabel) {
  const lab = el('label', 'row-switch');
  const wrap = el('span', 'switch');
  const inp = el('input');
  inp.type = 'checkbox';
  inp.checked = !!checked;
  if (ariaLabel) inp.setAttribute('aria-label', ariaLabel);
  wrap.appendChild(inp);
  wrap.appendChild(el('i'));
  lab.appendChild(wrap);
  inp.onchange = async () => {
    if (typeof onChange !== 'function') return;
    try {
      const r = onChange(inp.checked);
      if (r && typeof r.then === 'function') { inp.disabled = true; await r; }
    } catch (e) {
      inp.checked = !inp.checked; // 失败回滚
      toast(e.message, 'err');
    } finally {
      inp.disabled = false;
    }
  };
  return lab;
}

// segmented 分段控件。opts=[{value,label}]，onPick(value)。
function segmented(options, value, onPick) {
  const box = el('div', 'segmented');
  for (const o of options) {
    const b = el('button', 'seg' + (o.value === value ? ' active' : ''), o.label);
    b.onclick = () => onPick(o.value);
    box.appendChild(b);
  }
  return box;
}

// ---------------- SVG 图表（零依赖） ----------------

// SVG_NS 命名空间常量。
const SVG_NS = 'http://www.w3.org/2000/svg';

function svgEl(tag, attrs) {
  const e = document.createElementNS(SVG_NS, tag);
  for (const k in (attrs || {})) e.setAttribute(k, attrs[k]);
  return e;
}

// svgColumnChart 柱状/面积图。series=[{label,value}]，opts={height,color,unit}
// 返回一个 SVG 元素；柱高按最大值归一，零值画极矮灰柱。
function svgColumnChart(series, opts) {
  opts = opts || {};
  const W = 100, H = opts.height || 34; // 用 viewBox 单位，外层 CSS 控制像素
  const n = Math.max(1, series.length);
  const max = Math.max(1, ...series.map((p) => Number(p.value) || 0));
  const svg = svgEl('svg', { viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: 'none', class: 'chart-svg' });
  const bw = W / n;
  series.forEach((p, i) => {
    const v = Number(p.value) || 0;
    const h = v === 0 ? 1 : Math.max(3, (v / max) * (H - 2));
    const r = svgEl('rect', {
      x: (i * bw + bw * 0.18).toFixed(2), y: (H - h).toFixed(2),
      width: (bw * 0.64).toFixed(2), height: h.toFixed(2), rx: Math.min(1.2, bw * 0.2),
      fill: v === 0 ? 'var(--border)' : (opts.color || 'var(--accent)'),
    });
    const t = svgEl('title'); t.textContent = (p.label || '') + ' · ' + v;
    r.appendChild(t);
    svg.appendChild(r);
  });
  return svg;
}

// svgLineChart 折线图。series=[{label,value}]，opts={height,color}
function svgLineChart(series, opts) {
  opts = opts || {};
  const W = 100, H = opts.height || 34, pad = 2;
  const svg = svgEl('svg', { viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: 'none', class: 'chart-svg' });
  if (!series.length) return svg;
  const max = Math.max(1, ...series.map((p) => Number(p.value) || 0));
  const step = series.length > 1 ? (W - pad * 2) / (series.length - 1) : 0;
  const pts = series.map((p, i) => {
    const x = pad + i * step;
    const y = H - pad - ((Number(p.value) || 0) / max) * (H - pad * 2);
    return [x, y];
  });
  const d = pts.map((p, i) => (i === 0 ? 'M' : 'L') + p[0].toFixed(2) + ' ' + p[1].toFixed(2)).join(' ');
  const color = opts.color || 'var(--accent)';
  svg.appendChild(svgEl('path', { d: d + ` L${(pad + (series.length - 1) * step).toFixed(2)} ${H} L${pad} ${H} Z`, fill: color, opacity: '0.12', stroke: 'none' }));
  svg.appendChild(svgEl('path', { d: d, fill: 'none', stroke: color, 'stroke-width': '1.4', 'stroke-linejoin': 'round', 'stroke-linecap': 'round', 'vector-effect': 'non-scaling-stroke' }));
  series.forEach((p, i) => {
    const c = svgEl('circle', { cx: pts[i][0].toFixed(2), cy: pts[i][1].toFixed(2), r: '1.5', fill: color });
    const t = svgEl('title'); t.textContent = (p.label || '') + ' · ' + (Number(p.value) || 0);
    c.appendChild(t);
    svg.appendChild(c);
  });
  return svg;
}

// svgDonut 环形占比图。items=[{label,value,color}]，居中显示 centerLabel。
function svgDonut(items, centerLabel, opts) {
  opts = opts || {};
  const size = 120, r = 46, sw = 14, cx = size / 2, cy = size / 2;
  const C = 2 * Math.PI * r;
  const total = Math.max(1, items.reduce((s, x) => s + (Number(x.value) || 0), 0));
  const svg = svgEl('svg', { viewBox: `0 0 ${size} ${size}`, class: 'donut-svg' });
  svg.appendChild(svgEl('circle', { cx, cy, r, fill: 'none', stroke: 'var(--bg)', 'stroke-width': sw }));
  let off = 0;
  const palette = ['var(--accent)', 'var(--ok)', 'var(--warn)', 'var(--accent-2)', 'var(--err)'];
  items.forEach((it, i) => {
    const v = Number(it.value) || 0;
    if (v <= 0) return;
    const len = (v / total) * C;
    const arc = svgEl('circle', {
      cx, cy, r, fill: 'none', stroke: it.color || palette[i % palette.length],
      'stroke-width': sw, 'stroke-dasharray': `${len.toFixed(2)} ${(C - len).toFixed(2)}`,
      'stroke-dashoffset': (-off).toFixed(2), 'stroke-linecap': 'butt',
      transform: `rotate(-90 ${cx} ${cy})`,
    });
    const t = svgEl('title'); t.textContent = it.label + ' · ' + v;
    arc.appendChild(t);
    svg.appendChild(arc);
    off += len;
  });
  if (centerLabel) {
    const tx = svgEl('text', { x: cx, y: cy + 5, 'text-anchor': 'middle', 'font-size': '18', 'font-weight': '650', fill: 'var(--text)' });
    tx.textContent = centerLabel;
    svg.appendChild(tx);
  }
  return svg;
}

// svgHBars 横向条形（按模型/账号排行的可视化）。
// items=[{label,value}]，opts={unit,color,fmt}
function svgHBars(items, opts) {
  opts = opts || {};
  const fmt = opts.fmt || ((v) => fmtNum(v));
  const box = el('div', 'hbars');
  if (!items.length) { box.appendChild(el('div', 'muted', '暂无数据')); return box; }
  const max = Math.max(1, ...items.map((x) => Number(x.value) || 0));
  for (const it of items) {
    const v = Number(it.value) || 0;
    const row = el('div', 'hbar-row');
    const top = el('div', 'hbar-top');
    top.appendChild(el('span', 'hbar-label', it.label));
    top.appendChild(el('span', 'hbar-val', fmt(v) + (opts.unit || '')));
    row.appendChild(top);
    const track = el('div', 'hbar-track');
    const fill = el('i');
    fill.style.width = Math.max(0, Math.round((v / max) * 100)) + '%';
    if (opts.color) fill.style.background = opts.color;
    track.appendChild(fill);
    row.appendChild(track);
    box.appendChild(row);
  }
  return box;
}


// ---------------- 复制反馈 / 弹窗 / 小块 ----------------

// copyBtn 带 ✓ 反馈的复制按钮（对齐参考项目：点一下图标变绿勾、文案变「已复制」）。
// getText 可传字符串或返回字符串的函数。cls 默认 icon-btn。
function copyBtn(getText, label, cls) {
  const b = el('button', cls || 'icon-btn');
  b.type = 'button';
  const ic = el('span', 'cb-ic');
  ic.appendChild(icon('copy'));
  b.appendChild(ic);
  const txt = el('span', '', label || '复制');
  b.appendChild(txt);
  const reset = () => {
    b.classList.remove('copied');
    ic.textContent = '';
    ic.appendChild(icon('copy'));
    txt.textContent = label || '复制';
  };
  b.onclick = async () => {
    const val = typeof getText === 'function' ? getText() : getText;
    try {
      await navigator.clipboard.writeText(String(val == null ? '' : val));
    } catch (e) {
      toast('复制失败，请手动选择', 'err');
      return;
    }
    b.classList.add('copied');
    ic.textContent = '';
    ic.appendChild(icon('check'));
    txt.textContent = '已复制';
    clearTimeout(b._t);
    b._t = setTimeout(reset, 1500);
  };
  return b;
}

// openModal 通用弹窗。返回 {mask, body, close}（点击遮罩 / × 关闭）。
function openModal(title, subtitle) {
  const mask = el('div', 'modal-mask');
  const box = el('div', 'modal');
  const h = el('div', 'modal-head');
  h.appendChild(el('b', '', title));
  const close = el('button', 'modal-x', '×');
  const closeFn = () => mask.remove();
  close.onclick = closeFn;
  h.appendChild(close);
  box.appendChild(h);
  if (subtitle) box.appendChild(el('div', 'modal-sub', subtitle));
  const body = el('div', 'modal-body');
  box.appendChild(body);
  mask.appendChild(box);
  mask.onclick = (e) => { if (e.target === mask) closeFn(); };
  document.body.appendChild(mask);
  return { mask, body, close: closeFn };
}

// statCard 数值卡。opts={unit,hint,cls,icon}
function statCard(k, v, opts) {
  opts = opts || {};
  const c = el('div', 'stat-card' + (opts.cls ? ' ' + opts.cls : ''));
  const kk = el('div', 'k');
  if (opts.icon) kk.appendChild(icon(opts.icon, 13));
  kk.appendChild(el('span', '', k));
  c.appendChild(kk);
  const vv = el('div', 'v', v);
  if (opts.unit) vv.appendChild(el('span', 'u', opts.unit));
  c.appendChild(vv);
  if (opts.hint) c.appendChild(el('div', 'hint', opts.hint));
  return c;
}

// legend 图例（items=[{label,color}]）。
function legend(items) {
  const box = el('div', 'chart-legend');
  for (const it of items) {
    const s = el('span');
    const i = el('i');
    i.style.background = it.color;
    s.appendChild(i);
    s.appendChild(document.createTextNode(it.label));
    box.appendChild(s);
  }
  return box;
}

// cfgRow 配置行（左文案 + 右控件）。
function cfgRow(title, hint, ctl) {
  const row = el('div', 'cfg-row');
  const txt = el('div', 'cfg-txt');
  txt.appendChild(el('b', '', title));
  if (hint) txt.appendChild(el('div', 'hint', hint));
  row.appendChild(txt);
  const c = el('div', 'cfg-ctl');
  if (ctl) c.appendChild(ctl);
  row.appendChild(c);
  return row;
}

// ---------------- 设备授权扫码登录弹窗（v0.12.0，需求 4 / 5） ----------------
// 对齐参考项目 WorkBuddy-Switch2api 的 OAuthLoginDialog：
//   发起扫码 → 打开验证页 → 轮询采集结果 → 账号入库 → 提示 + 回调刷新。
// 国内版 / 国际版是两套独立登录态，弹窗提供分段切换（也可由调用方预置 edition）。

// OAUTH_EDITIONS 两个档位（与后端 realm 取值对应）。
const OAUTH_EDITIONS = [
  { value: 'cn', label: '国内版 CN' },
  { value: 'global', label: '国际版 Global' },
];

// oauthLoginDialog 打开扫码登录弹窗。
// opts = { startUrl, pollUrl, edition, title, subtitle, onDone }
//   startUrl: POST {realm} → {session_id, auth_url}
//   pollUrl : GET ?sid=… → {status:'pending'|'done', account?, message?}
function oauthLoginDialog(opts) {
  opts = opts || {};
  const startUrl = opts.startUrl || '/api/admin/oauth/start';
  const pollUrl = opts.pollUrl || '/api/admin/oauth/poll';
  let edition = opts.edition || 'cn';
  let timer = null;
  let finished = false;
  // authWin 点「开始扫码登录」时在用户手势栈内预开的空白窗口。
  //
  // 为什么要预开：真正要跳转的授权地址要等 POST 返回才知道，而在 await 之后调
  // window.open 会被浏览器判为「非用户手势」直接拦截 —— 这正是「点了扫码却没有
  // 自动跳转」的根因。先在点击栈内开一个空白页占位，拿到地址后改写它的 location，
  // 就能稳定自动跳转；被拦时下面的链接/复制按钮仍是兜底。
  let authWin = null;

  const m = openModal(opts.title || '扫码添加账号',
    opts.subtitle || '选好版本后点「开始扫码登录」，在浏览器中完成授权；本页会自动检测并把账号入库。两个版本登录态不通用。');
  const body = m.body;

  // 档位分段控件
  const segBox = el('div');
  const renderSeg = () => {
    segBox.textContent = '';
    segBox.appendChild(segmented(OAUTH_EDITIONS, edition, (v) => { edition = v; renderSeg(); }));
  };
  renderSeg();
  body.appendChild(cfgRow('目标版本', '国内版走 codebuddy.cn，国际版走 workbuddy.ai', segBox));

  // 动作区
  const actRow = el('div', 'row');
  const startBtn = el('button', '', '开始扫码登录');
  const hint = el('span', 'muted', '');
  actRow.appendChild(startBtn);
  actRow.appendChild(hint);
  body.appendChild(actRow);

  const authBox = el('div', 'oauth-auth hidden');
  body.appendChild(authBox);

  const stop = () => {
    if (timer) { clearInterval(timer); timer = null; }
    try { if (authWin && !authWin.closed) authWin.close(); } catch (e) { /* 忽略 */ }
    authWin = null;
  };
  const closed = () => !document.body.contains(m.mask);
  const tick = async (fn) => { if (closed()) { stop(); return; } await fn(); };

  const poll = async (sid) => {
    await tick(async () => {
      let d;
      try { d = await api(pollUrl + '?sid=' + encodeURIComponent(sid)); }
      catch (e) {
        // 会话过期等致命错误 → 停轮询并提示。
        stop();
        hint.className = 'muted err';
        hint.textContent = e.message;
        return;
      }
      if (d.status === 'done') {
        finished = true;
        stop();
        renderDone(d.account);
        return;
      }
      hint.textContent = d.message || '等待浏览器完成登录…';
    });
  };

  const renderDone = (account) => {
    authBox.textContent = '';
    const ok = el('div', 'notice ok');
    const name = account && (account.nickname || account.uid);
    ok.appendChild(el('b', '', '已采集账号：' + (name || '（匿名）')));
    if (account && account.realm) ok.appendChild(el('span', 'muted', ' · ' + (account.realm === 'global' ? '国际版' : '国内版')));
    authBox.appendChild(ok);
    const row = el('div', 'row');
    const fin = el('button', '', '完成');
    fin.onclick = () => { m.close(); if (typeof opts.onDone === 'function') opts.onDone(account); };
    row.appendChild(fin);
    authBox.appendChild(row);
    authBox.classList.remove('hidden');
  };

  startBtn.onclick = async () => {
    if (finished) return;
    stop();
    // 预开占位窗口：必须在 await 之前，否则会被弹窗拦截器拦掉（见 authWin 注释）。
    try { authWin = window.open('', '_blank'); } catch (e) { authWin = null; }
    startBtn.disabled = true;
    startBtn.classList.add('spin');
    hint.className = 'muted';
    hint.textContent = '正在发起登录…';
    authBox.textContent = '';
    authBox.classList.add('hidden');
    let res;
    try {
      res = await post(startUrl, { realm: edition });
    } catch (e) {
      try { if (authWin && !authWin.closed) authWin.close(); } catch (_) { /* 忽略 */ }
      authWin = null;
      hint.className = 'muted err';
      hint.textContent = '发起失败：' + e.message;
      startBtn.disabled = false;
      startBtn.classList.remove('spin');
      return;
    }
    startBtn.disabled = false;
    startBtn.classList.remove('spin');
    hint.textContent = '等待浏览器完成登录…';
    renderAuth(res.auth_url);
    // 立即尝试一次，随后每 1.5 秒轮询（对齐参考项目节奏）。
    await poll(res.session_id);
    timer = setInterval(() => poll(res.session_id), 1500);
  };

  // renderAuth 渲染授权链接（并自动跳转）。
  const renderAuth = (url) => {
    authBox.textContent = '';
    const box = el('div', 'notice');
    const line = el('div', 'row');
    const open = el('a', 'oauth-open', '① 打开授权页');
    open.href = url;
    open.target = '_blank';
    open.rel = 'noreferrer';
    line.appendChild(open);
    line.appendChild(copyBtn(() => url, '复制链接', 'icon-btn'));
    box.appendChild(line);
    const l = el('div', 'oauth-url');
    const a = el('a', '', url);
    a.href = url; a.target = '_blank'; a.rel = 'noreferrer';
    l.appendChild(a);
    box.appendChild(l);
    box.appendChild(el('div', 'hint', '已自动打开授权页；若没跳转（被浏览器拦截），点上面的链接或复制到浏览器打开。本页会自动检测账号并入库。'));
    authBox.appendChild(box);
    authBox.classList.remove('hidden');
    // 自动跳转：复用预开的占位窗口；占位被拦时再尝试直接新开一个。
    if (authWin && !authWin.closed) {
      try { authWin.location.href = url; return; } catch (e) { /* 跨域/被拦 → 走下面的兜底 */ }
    }
    try { window.open(url, '_blank', 'noopener,noreferrer'); } catch (e) { /* 交给链接兜底 */ }
  };

  return m;
}

// oauthScanButtons 生成「扫码添加（国内版）/（国际版）」两个按钮（参考项目同款）。
// opts = { startUrl, pollUrl, onDone }；返回可直接 append 的容器元素。
function oauthScanButtons(opts) {
  const box = el('div', 'row oauth-btns');
  for (const e of OAUTH_EDITIONS) {
    const b = el('button', e.value === 'cn' ? '' : 'ghost');
    b.appendChild(icon('plus', 13));
    b.appendChild(el('span', '', '扫码添加（' + e.label + '）'));
    b.onclick = () => oauthLoginDialog(Object.assign({}, opts, { edition: e.value }));
    box.appendChild(b);
  }
  return box;
}


// mountRefreshSlots 把页面里 [data-refresh="fnName"] 占位换成带旋转反馈的图标按钮。
// fnName 必须是对应页面脚本里的全局函数名。
function mountRefreshSlots() {
  for (const s of Array.from(document.querySelectorAll('[data-refresh]'))) {
    const fn = window[s.dataset.refresh];
    if (typeof fn !== 'function') continue;
    if (s.querySelector('.icon-btn')) continue; // 已挂载过，幂等
    s.appendChild(iconBtn({
      icon: 'refresh',
      label: s.dataset.refreshLabel || '刷新',
      title: '刷新',
      onClick: () => fn(),
    }));
  }
}

// ======================================================================
// 主题（深色 / 浅色，v0.17.8）
//
// 设计：
//   1) 落点是 <html data-theme="dark|light">，样式全部由 CSS 变量驱动
//      （见 style.css 顶部三块规则），因此切换是**实时**的，无需刷新；
//   2) 偏好写 localStorage（键 wb_theme），刷新与再次登录后保持 ——
//      它是浏览器维度偏好，不跟账号走（同一台电脑换号登录也该保持视觉一致）；
//   3) 首屏防闪：两个页面 <head> 里各有一段内联脚本，在样式表解析前就把
//      data-theme 打上，避免「先亮一下再变暗」的白闪；
//   4) 未手选过时不写 data-theme，交回给系统 prefers-color-scheme。
// ======================================================================

const THEME_KEY = 'wb_theme';

// themeStored 已手选的主题（'' = 未手选，跟随系统）。
function themeStored() {
  try { return localStorage.getItem(THEME_KEY) || ''; } catch (e) { return ''; }
}

// themeEffective 当前实际生效的主题（含系统偏好兜底）。
function themeEffective() {
  const set = document.documentElement.getAttribute('data-theme');
  if (set) return set;
  try {
    return window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  } catch (e) { return 'light'; }
}

// applyTheme 写入主题并同步按钮图标。
function applyTheme(next) {
  if (next) document.documentElement.setAttribute('data-theme', next);
  else document.documentElement.removeAttribute('data-theme');
  syncThemeBtn();
}

// syncThemeBtn 让按钮图标与当前主题一致（深色时显示太阳=切回浅色）。
function syncThemeBtn() {
  const b = document.getElementById('btnTheme');
  if (!b) return;
  const dark = themeEffective() === 'dark';
  b.title = dark ? '切换到浅色模式' : '切换到深色模式';
  b.setAttribute('aria-label', b.title);
  b.replaceChildren(icon(dark ? 'up' : 'down'), el('span', '', dark ? '浅色' : '深色'));
}

// toggleTheme 手选并持久化；随后（若系统偏好变化）也不再影响已手选的主题。
function toggleTheme() {
  const next = themeEffective() === 'dark' ? 'light' : 'dark';
  try { localStorage.setItem(THEME_KEY, next); } catch (e) { /* 隐私模式下写不了，仍生效于本次会话 */ }
  applyTheme(next);
  toast('已切换到' + (next === 'dark' ? '深色' : '浅色') + '模式', 'ok');
}

// setTheme 显式设置主题（v0.26.6）。命令面板里"切换到深色/浅色"直接用它 ——
// 面板是**另一个入口**，不该逼用户先去侧边栏底部找那个小按钮。
// 与 toggleTheme 共用 applyTheme + localStorage，口径只有一份。
function setTheme(next) {
  if (next !== 'dark' && next !== 'light') return;
  try { localStorage.setItem(THEME_KEY, next); } catch (e) { /* 隐私模式：仅本次会话生效 */ }
  applyTheme(next);
  toast('已切换到' + (next === 'dark' ? '深色' : '浅色') + '模式', 'ok');
}
window.setTheme = setTheme;

// mountThemeToggle 挂载侧边栏的主题切换按钮（由各页 init 调用一次；幂等）。
function mountThemeToggle() {
  if (document.getElementById('btnTheme')) { syncThemeBtn(); return; }
  const foot = document.querySelector('.sidebar-foot .foot-actions');
  if (!foot) return;
  const b = el('button', 'ghost sm');
  b.id = 'btnTheme';
  b.onclick = toggleTheme;
  foot.appendChild(b);
  syncThemeBtn();
  // 未手选主题时，跟随系统偏好的实时变化（同一会话内系统切暗色立即生效）。
  try {
    window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
      if (!themeStored()) syncThemeBtn();
    });
  } catch (e) { /* 老浏览器忽略 */ }
}

// ======================================================================
// 修改密码（v0.17.8）
//
// 前后端分工（避免「两边各校验一半」导致的口径漂移）：
//   · 前端：即时反馈 —— 空值、两次输入不一致、长度与字符类别的**同一条规则**
//     文案（与后端 users.go:passwordPolicyHint 保持一致）；
//   · 后端：权威校验 —— 原密码是否正确、强度是否达标，都由服务端说了算。
//     前端校验只是体验优化，**不是安全边界**（任何人都能直接打接口）。
//
// 失败提示一律用服务端返回的 message（如「原密码不正确」），不在这里造新文案，
// 避免出现「界面说 A、日志记 B」。
// ======================================================================

// PASSWORD_POLICY_HINT 与后端 users.go 的 passwordPolicyHint 逐字一致。
const PASSWORD_POLICY_HINT = '至少 8 位、至多 128 位，且需包含字母与数字（或其它符号）中的至少两类';

// openChangePasswordModal 打开改密码弹窗。
function openChangePasswordModal() {
  const m = openModal('修改密码', '原密码用于确认是你本人；新密码需满足强度要求。');
  const form = el('div');
  const mk = (label, id, ph) => {
    const w = el('div', 'setting-card');
    w.appendChild(el('label', '', label));
    const i = el('input');
    i.type = 'password';
    i.id = id;
    i.autocomplete = 'new-password';
    i.placeholder = ph || '';
    w.appendChild(i);
    form.appendChild(w);
    return i;
  };
  mk('原密码', 'pwOld', '当前使用的密码');
  mk('新密码', 'pwNew', '满足下方强度要求');
  mk('确认新密码', 'pwNew2', '再输入一次');
  const hint = el('div', 'hint', '强度要求：' + PASSWORD_POLICY_HINT + '；新密码不能与原密码或用户名相同。');
  form.appendChild(hint);
  m.body.appendChild(form);

  const row = el('div', 'row');
  row.style.marginTop = '12px';
  const ok = el('button', '', '确认修改');
  const cancel = el('button', 'ghost', '取消');
  cancel.onclick = m.close;
  ok.onclick = async () => {
    const oldPw = $('pwOld').value;
    const newPw = $('pwNew').value;
    const newPw2 = $('pwNew2').value;
    // 前端即时校验（顺序与后端一致：先完整性，再一致性，最后强度）。
    if (!oldPw) { toast('请输入原密码', 'err'); $('pwOld').focus(); return; }
    if (!newPw) { toast('请输入新密码', 'err'); $('pwNew').focus(); return; }
    if (newPw !== newPw2) { toast('两次输入的新密码不一致', 'err'); $('pwNew2').focus(); return; }
    if (newPw.length < 8) { toast('新密码至少 8 位', 'err'); $('pwNew').focus(); return; }
    if (newPw.length > 128) { toast('新密码过长（最多 128 位）', 'err'); $('pwNew').focus(); return; }
    ok.disabled = true;
    try {
      const d = await post('/api/auth/password', { old_password: oldPw, new_password: newPw });
      toast(d.message || '密码已更新', 'ok');
      m.close();
    } catch (e) {
      // 服务端是权威：原密码错 / 强度不达标都直接展示它的原话。
      toast(e.message, 'err');
    } finally { ok.disabled = false; }
  };
  row.append(ok, cancel);
  m.body.appendChild(row);
  const first = $('pwOld');
  if (first) first.focus();
}

// mountAccountTools 挂载「改密码」入口（两个后台都挂，幂等）。
function mountAccountTools() {
  if (document.getElementById('btnChangePw')) return;
  const foot = document.querySelector('.sidebar-foot .foot-actions');
  if (!foot) return;
  const b = el('button', 'ghost sm');
  b.id = 'btnChangePw';
  b.textContent = '改密码';
  b.title = '修改登录密码';
  b.onclick = openChangePasswordModal;
  foot.appendChild(b);
}

// ======================================================================
// 站内公告（v0.17.8）
//
// 展示时机（与后端 announce.go 的口径一一对应）：
//   · 概览卡片：**置顶在概览页最前**，始终展示启用中的公告（与已读无关），
//     置顶在前，至多 20 条；**无公告时整块隐藏**（否则顶部留一大片空白）；
//   · 弹窗：只弹「本次浏览器没读过」的公告；已读 id 记在 localStorage。
//     因此同一条公告只打扰一次，刷新 / 重新登录都不会重复弹。
//
// 渲染目标是页面里已存在的容器：
//   · #annBox       概览页公告**外框**（没有它就只走弹窗，不渲染卡片）
//   · #annCard      外框内的卡片网格
//   · #annChip      侧边栏/顶部的未读角标（可选）
// ======================================================================

const ANN_SEEN_KEY = 'wb_ann_seen';
const ANN_SEEN_MAX = 200; // 只保留最近若干条 id，避免 localStorage 无界增长

function annSeen() {
  try {
    const raw = JSON.parse(localStorage.getItem(ANN_SEEN_KEY) || '[]');
    return Array.isArray(raw) ? raw.map(Number).filter((x) => x > 0) : [];
  } catch (e) { return []; }
}

function annMarkSeen(id) {
  const ids = annSeen().filter((x) => x !== id);
  ids.push(id);
  try { localStorage.setItem(ANN_SEEN_KEY, JSON.stringify(ids.slice(-ANN_SEEN_MAX))); } catch (e) { /* 忽略 */ }
}

// annLevelMeta 级别 → 展示元信息（配色 + 中文名）。
function annLevelMeta(level) {
  switch (level) {
    case 'important': return { cls: 'err', label: '重要' };
    case 'warn': return { cls: 'warn', label: '提醒' };
    default: return { cls: 'accent', label: '公告' };
  }
}

// annCardEl 渲染单条公告（概览卡片用）。
//
// v0.22.0：接口只回「生效中」的公告，故这里不必再判可见性；但要**显式标出**
// ① 置顶（📌，让用户理解为什么它排在最前）② 有效期（让用户知道它什么时候会消失）。
function annCardEl(a) {
  const meta = annLevelMeta(a.level);
  const card = el('article', 'ecard');
  const head = el('header');
  const who = el('div', 'who');
  who.appendChild(el('div', 'name', a.title || '（无标题）'));
  // ident = 发布时间（+ 有效期至…，仅在有结束时间时追加）。
  who.appendChild(el('div', 'ident', fmtTime(a.created_at) +
    (a.end_at ? ' · 有效期至 ' + fmtTime(a.end_at) : '')));
  head.appendChild(who);
  head.appendChild(el('span', 'tag ' + meta.cls, meta.label));
  if (a.pinned) head.appendChild(el('span', 'tag', '📌 置顶'));
  card.appendChild(head);
  const body = el('div', 'body');
  const p = el('div');
  // 用 textContent：公告正文是纯文本，不解析 HTML（管理员/被注入口都不该能注入脚本）。
  p.style.whiteSpace = 'pre-wrap';
  p.textContent = a.content || '';
  body.appendChild(p);
  card.appendChild(body);
  return card;
}

// renderAnnCard 渲染概览页公告卡（位于概览页**最前**）。
//
// 整块显隐由这里控制：有公告才显示外框，没有就藏起来。
// 不这么做的话，没公告的用户会在概览页最上方看到一片写着「暂无公告」的空卡 ——
// 占掉首屏最贵的位置，却什么也没说。
function renderAnnCard(list) {
  const box = document.getElementById('annCard');
  const frame = document.getElementById('annBox');
  if (!box) return;
  box.textContent = '';
  if (!list.length) {
    if (frame) frame.style.display = 'none';
    return;
  }
  for (const a of list) box.appendChild(annCardEl(a));
  if (frame) frame.style.display = '';
}

// popupAnnouncements 逐条弹未读公告（一次登录会话最多弹这几条）。
function popupAnnouncements(list) {
  const unread = list.filter((a) => !annSeen().includes(Number(a.id)));
  if (!unread.length) return;
  // 只弹最新的一条：一次弹 5 个窗是打扰而不是通知；其余留在卡片里。
  const a = unread[unread.length - 1];
  const meta = annLevelMeta(a.level);
  const m = openModal('【' + meta.label + '】' + (a.title || '站内公告'), fmtTime(a.created_at));
  const body = el('div');
  body.style.whiteSpace = 'pre-wrap';
  body.style.maxHeight = '50vh';
  body.style.overflow = 'auto';
  body.textContent = a.content || '';
  m.body.appendChild(body);
  const row = el('div', 'row');
  row.style.marginTop = '12px';
  const ok = el('button', '', '我知道了');
  const close = () => { annMarkSeen(Number(a.id)); m.close(); };
  ok.onclick = close;
  // 关掉遮罩 / × 也视为已读（否则每次刷新都会再弹同一条）。
  m.mask.addEventListener('click', (ev) => { if (ev.target === m.mask) annMarkSeen(Number(a.id)); });
  row.appendChild(ok);
  m.body.appendChild(row);
}

// maybeShowAnnouncements 拉公告 → 渲染卡片 + 弹未读。失败静默（公告不该打断使用）。
async function maybeShowAnnouncements() {
  let d;
  try { d = await api('/api/announcements'); } catch (e) { return; }
  const list = (d && d.announcements) || [];
  renderAnnCard(list);
  popupAnnouncements(list);
  const chip = document.getElementById('annChip');
  if (chip) {
    const n = list.filter((a) => !annSeen().includes(Number(a.id))).length;
    chip.textContent = n > 0 ? String(n) : '';
    chip.classList.toggle('hidden', n === 0);
  }
}

// ======================================================================
// v0.26.6 · 共享 UI 套件（管理后台 + 用户控制台共用，原生 DOM 无依赖）
//
// 这一批的取舍完全来自对 workbuddy-manager（Next.js + shadcn 面板）的对照调研，
// 但**只吸收交互与信息结构，不引入任何框架/构建**：
//
//   1. 通知：单元素自动消失 → **语义化堆叠卡片**（图标 / 可关闭 / 悬停暂停 /
//      aria-live）。旧实现的问题很具体：错误弹 2.8 秒就没了，用户切回来时
//      根本不知道刚才失败过（而页面上的数字看起来一切正常）。
//   2. 确认：原生 `confirm()`（不可样式化、阻塞、无 busy 态）→ **自定义对话框**。
//      危险操作要能一眼看出"这是破坏性的"，且点击后要有"正在执行"的反馈。
//   3. 导航：13 个菜单项靠眼睛找 → **⌘K / Ctrl+K 命令面板**（可发现性优先：
//      右上角有一个能点的按钮，快捷键只是第二条路径）。
//   4. 取数：失败只弹 toast → **常驻失败横幅 + 重试**（不自动消失）。
//   5. 首屏：文字"加载中…" → **骨架屏**（形状先出现，内容后到位，避免跳动）。
//   6. KPI：数字直接跳变 → **数字滚动**（从上一帧的数字滚到新值）。
// ======================================================================

// prefersReduced 尊重系统「减少动效」偏好。动画是锦上添花，不该对前庭敏感的人
// 造成困扰 —— 这是无障碍要求，不是可选项。
function prefersReduced() {
  try { return window.matchMedia('(prefers-reduced-motion: reduce)').matches; } catch (e) { return false; }
}

// ---------------------------------------------------------------- ① 通知
//
// 兼容旧签名：toast(msg, 'ok' | 'err')。第三参可选：{ duration, sticky, action }。
// sticky = 不自动消失（失败类提示默认更久，但不是永久 —— 永久提示请用 loadBanner）。
const TOAST_ICON = { ok: '✓', err: '!', warn: '!', info: 'i' };

function toast(msg, kind, opts) {
  opts = opts || {};
  let host = $('toastHost');
  if (!host) {
    host = el('div', 'toast-host');
    host.id = 'toastHost';
    // 礼貌播报：成功/信息不打断屏幕阅读器，错误用 assertive 由卡片自身声明。
    host.setAttribute('aria-live', 'polite');
    host.setAttribute('aria-atomic', 'false');
    document.body.appendChild(host);
  }
  const card = el('div', 'toast-card ' + (kind || 'info'));
  card.setAttribute('role', kind === 'err' ? 'alert' : 'status');
  card.appendChild(el('span', 'toast-ico', TOAST_ICON[kind] || TOAST_ICON.info));
  const body = el('div', 'toast-msg');
  body.textContent = msg == null ? '' : String(msg);
  card.appendChild(body);
  const x = el('button', 'toast-x', '×');
  x.type = 'button';
  x.setAttribute('aria-label', '关闭通知');
  card.appendChild(x);
  host.appendChild(card);

  // 上限 5 条：再多会把右上角糊住，而且旧的那几条已经没人在读。
  while (host.children.length > 5) host.removeChild(host.firstChild);

  const ttl = Number(opts.duration) || (kind === 'err' ? 6000 : 3200);
  const bar = el('i', 'toast-bar');
  card.appendChild(bar);
  if (!opts.sticky) {
    bar.style.animationDuration = ttl + 'ms';
    // 悬停暂停：正在读的时候被抽走是最让人恼火的一件事。
    let left = ttl, start = Date.now(), timer = null;
    const stop = () => { clearTimeout(timer); left -= Date.now() - start; bar.style.animationPlayState = 'paused'; };
    const go = () => { start = Date.now(); bar.style.animationPlayState = 'running'; timer = setTimeout(dismiss, Math.max(400, left)); };
    const dismiss = () => {
      clearTimeout(timer);
      if (card._gone) return;
      card._gone = true;
      card.classList.add('out');
      setTimeout(() => { if (card.parentNode) card.parentNode.removeChild(card); }, 220);
    };
    card.addEventListener('mouseenter', stop);
    card.addEventListener('mouseleave', go);
    x.onclick = dismiss;
    go();
  } else {
    x.onclick = () => { if (card.parentNode) card.parentNode.removeChild(card); };
  }
  return card;
}

// ---------------------------------------------------------------- ② 确认对话框
//
// confirmDialog({...}) → Promise<boolean>。
//
// 为什么是 Promise 而不是回调：调用点几乎全是
//   `if (!confirm(...)) return;`
// 的形态，返回布尔判断最自然；Promise 让它保持"看起来一样"（只是多个 await）。
//
// ⚠️ 调用点必须位于 async 函数内（或 .then 里）。这由 jscheck 与回归用例兜住。
function confirmDialog(opts) {
  opts = opts || {};
  // 测试通道：回归框架可置 window.__autoConfirm = true 让它直接通过，
  // 这样既有的大量流程用例不必逐个去点确认键（专用用例会显式测真实交互）。
  if (window.__autoConfirm) return Promise.resolve(true);

  return new Promise((resolve) => {
    const back = el('div', 'dlg-back');
    back.setAttribute('role', 'presentation');
    const dlg = el('div', 'dlg' + (opts.danger ? ' danger' : ''));
    dlg.setAttribute('role', 'alertdialog');
    dlg.setAttribute('aria-modal', 'true');
    const titleId = 'dlgTitle_' + Date.now();
    dlg.setAttribute('aria-labelledby', titleId);

    const head = el('div', 'dlg-head');
    const h = el('div', 'dlg-title', opts.title || '确认操作');
    h.id = titleId;
    head.appendChild(h);
    if (opts.danger) head.appendChild(el('span', 'tag err', '不可撤销'));
    dlg.appendChild(head);

    if (opts.message) dlg.appendChild(el('div', 'dlg-msg', opts.message));
    if (opts.detail) dlg.appendChild(el('div', 'dlg-detail', opts.detail));

    const foot = el('div', 'dlg-foot');
    // hideCancel：纯提示型对话框（如"更新产物已就绪，请按此操作"）只需要一个
    // 关闭键。给两个语义相同的按钮会让人以为"取消"会撤销什么 —— 而它什么都不做。
    // 注意不能靠 cancelText:'' 来隐藏：那时 `|| '取消'` 会把它回落成"取消"。
    let cancel = null;
    if (!opts.hideCancel) {
      cancel = el('button', 'ghost', opts.cancelText || '取消');
      cancel.type = 'button';
      cancel.setAttribute('data-dlg-cancel', '1');
      foot.appendChild(cancel);
    }
    const ok = el('button', opts.danger ? 'danger' : '', opts.confirmText || '确认');
    ok.type = 'button';
    ok.setAttribute('data-dlg-ok', '1');
    foot.appendChild(ok);
    dlg.appendChild(foot);
    back.appendChild(dlg);

    let done = false;
    const close = (val) => {
      if (done) return;
      done = true;
      document.removeEventListener('keydown', onKey, true);
      if (back.parentNode) back.parentNode.removeChild(back);
      resolve(val);
    };
    // Esc 关闭：必须用捕获阶段并在关闭时移除，否则页面上其它 Esc 监听（弹窗、
    // 命令面板）会一起响应，出现"一次 Esc 关掉两层"。
    const onKey = (ev) => {
      if (ev.key === 'Escape') { ev.stopPropagation(); close(false); }
      if (ev.key === 'Enter' && ev.target && ev.target.tagName !== 'TEXTAREA') {
        ev.preventDefault();
        ok.click();
      }
    };
    if (cancel) cancel.onclick = () => close(false);
    // 遮罩点击关闭：但**只**在点遮罩本身时（点对话框内部不该关）。
    back.onclick = (ev) => { if (ev.target === back) close(false); };
    ok.onclick = async () => {
      if (opts.onConfirm) {
        ok.disabled = true;
        if (cancel) cancel.disabled = true;
        ok.textContent = opts.busyText || '执行中…';
        try {
          await opts.onConfirm();
        } catch (e) {
          // 回调自己负责报错（用 toast）；这里只把按钮恢复可用，
          // 让用户能再试一次或取消 —— 卡在 disabled 上是最糟的结局。
          ok.disabled = false;
          if (cancel) cancel.disabled = false;
          ok.textContent = opts.confirmText || '确认';
          toast(e && e.message ? e.message : String(e), 'err');
          return;
        }
      }
      close(true);
    };

    document.body.appendChild(back);
    document.addEventListener('keydown', onKey, true);
    setTimeout(() => { try { ok.focus(); } catch (e) {} }, 0);
  });
}

// confirmAsk 是 confirmDialog 的**语法糖**：直接把原来写给原生 confirm() 的那段
// 中文提示塞进来即可 —— 首段当标题、其余当说明（既有文案本来就是这么写的：
// 第一句是问题、空行后是代价与后果），于是替换时**一个字都不用改文案**。
//
// danger 只给真正不可逆的动作（删除 / 清空 / 封禁 / 打款）；"上架 / 分配 / 设为"
// 这类可回退的操作不该被涂成红色 —— 到处都是红色等于没有红色。
function confirmAsk(text, opts) {
  opts = opts || {};
  const parts = String(text == null ? '' : text).split(/\n{2,}/);
  const title = (parts.shift() || '确认操作').trim();
  return confirmDialog(Object.assign({
    title: title,
    message: parts.join('\n\n').trim(),
  }, opts));
}

// promptDialog(opts) —— 自定义输入对话框，**替代原生 prompt()**。
//
// 为什么必须替换（v0.27.0，用户报「管理后台用户余额增减功能失效」）：
// 原生 prompt() 在若干环境里（内嵌 WebView、被浏览器弹窗策略拦截、页面失焦时）
// 会**直接返回 null 且不显示任何界面** —— 调用方一律按"用户取消"处理，用户看到的
// 就是"点了按钮没反应"。旧的余额调整正是这样：`const v = prompt(...);
// if (v === null) return;` 一旦 prompt 不出界面，整条链路静默中止，且没有任何错误。
//
// 另外原生 prompt 还有两个绕不开的问题：同步阻塞（与我们的异步 UI 体系冲突）、
// 一次只能收一个字段（而"加余额"天然要"数值 + 原因"两个），以及完全不可样式化。
//
// opts:
//   title       标题
//   message     说明段
//   fields      [{name, label, value, placeholder, type, required, multiline, hint}]
//   confirmText 确认按钮文案
//   danger      确认键用危险色
//   successMsg  提交成功后的提示（可选，由本函数负责 toast）
//
// 返回 Promise<Record<string,string> | null>：null = 用户取消。
// 取消与"提交失败"必须可区分 —— 前者什么都不做，后者要报错。
function promptDialog(opts) {
  opts = opts || {};
  const fields = (opts.fields || []).slice();
  // 测试通道：回归框架把 window.__autoPrompt 设成 {字段: 值} 即直接返回该对象，
  // 设成 null 即视为"取消"。默认（未设置）走真实渲染 —— 这样专用用例能测真交互，
  // 而其余流程用例也不会因为一个弹窗卡住。
  if (window.__autoPrompt !== undefined) return Promise.resolve(window.__autoPrompt);

  return new Promise((resolve) => {
    const back = el('div', 'dlg-back');
    const dlg = el('div', 'dlg' + (opts.danger ? ' danger' : ''));
    dlg.setAttribute('role', 'dialog');
    dlg.setAttribute('aria-modal', 'true');

    const head = el('div', 'dlg-head');
    head.appendChild(el('div', 'dlg-title', opts.title || '请输入'));
    dlg.appendChild(head);
    if (opts.message) dlg.appendChild(el('div', 'dlg-msg', opts.message));

    const body = el('div', 'dlg-body');
    const inputs = {};
    for (const f of fields) {
      const row = el('div', 'dlg-field');
      if (f.label) {
        const lab = el('label', 'dlg-label', f.label);
        lab.setAttribute('for', 'dlgIn_' + f.name);
        row.appendChild(lab);
      }
      let inp;
      if (f.multiline) {
        inp = document.createElement('textarea');
        inp.rows = f.rows || 3;
      } else {
        inp = document.createElement('input');
        inp.type = f.type || 'text';
      }
      inp.className = 'dlg-input';
      inp.id = 'dlgIn_' + f.name;
      // data-dlg-field 供自动化用例直接定位（也便于将来做表单重置）。
      inp.setAttribute('data-dlg-field', f.name);
      if (f.placeholder) inp.placeholder = f.placeholder;
      if (f.value !== undefined && f.value !== null) inp.value = String(f.value);
      row.appendChild(inp);
      if (f.hint) row.appendChild(el('div', 'dlg-hint', f.hint));
      inputs[f.name] = inp;
      body.appendChild(row);
    }
    dlg.appendChild(body);

    const foot = el('div', 'dlg-foot');
    const cancel = el('button', 'ghost', '取消');
    cancel.type = 'button';
    cancel.setAttribute('data-dlg-cancel', '1');
    const ok = el('button', opts.danger ? 'danger' : '', opts.confirmText || '确定');
    ok.type = 'button';
    ok.setAttribute('data-dlg-ok', '1');
    foot.append(cancel, ok);
    dlg.appendChild(foot);
    back.appendChild(dlg);

    let done = false;
    const close = (val) => {
      if (done) return;
      done = true;
      document.removeEventListener('keydown', onKey, true);
      if (back.parentNode) back.parentNode.removeChild(back);
      resolve(val);
    };
    const collect = () => {
      const out = {};
      for (const f of fields) out[f.name] = String(inputs[f.name].value);
      return out;
    };
    const submit = () => {
      const vals = collect();
      // 必填校验：**在这里拦**，而不是让接口回一个 400 让用户重新填一遍
      //（重填时前面已输入的内容就丢了）。
      for (const f of fields) {
        if (f.required && !vals[f.name].trim()) {
          toast('请填写「' + (f.label || f.name) + '」', 'err');
          try { inputs[f.name].focus(); } catch (e) {}
          return;
        }
      }
      close(vals);
    };
    // Esc 关闭（捕获阶段，关闭时移除 —— 否则会和页面其它 Esc 监听叠加，
    // 出现"一次 Esc 关掉两层"）。
    const onKey = (ev) => {
      if (ev.key === 'Escape') { ev.stopPropagation(); close(null); }
      // Enter 提交；多行文本框里的 Enter 是换行，不提交（用户是在写备注）。
      if (ev.key === 'Enter' && !ev.shiftKey) {
        const t = ev.target;
        if (t && t.tagName === 'TEXTAREA') return;
        ev.preventDefault();
        submit();
      }
    };
    cancel.onclick = () => close(null);
    back.onclick = (ev) => { if (ev.target === back) close(null); };
    ok.onclick = submit;

    document.body.appendChild(back);
    document.addEventListener('keydown', onKey, true);
    setTimeout(() => {
      const first = fields.length ? inputs[fields[0].name] : ok;
      try { first.focus(); if (first.select) first.select(); } catch (e) {}
    }, 0);
  });
}

// ---------------------------------------------------------------- ③ 命令面板（⌘K）
//
// 只做「跳转」，不放任何破坏性动作 —— ⌘K 的用法是"敲几个字 + 回车"，而回车前
// 那个词往往只打了一半；把「退出登录 / 删除账号」放进来等于把最容易误触的交互
// 接到最危险的按钮上。
//
// 排序**不按使用历史**：同一句查询在不同机器上给出不同结果会让"第一项是什么"
// 变得不可预测，方向键的肌肉记忆就废了。省下那一次输入不值得。

// navCommands 从侧边栏实时派生页面清单（单一事实源：菜单即清单）。
function navCommands() {
  const out = [];
  let group = '';
  for (const node of document.querySelectorAll('.nav .nav-group, .nav .nav-item')) {
    if (node.classList.contains('nav-group')) { group = node.textContent.trim(); continue; }
    const label = (node.textContent || '').replace(/\s+/g, '').replace(/[0-9]+$/, '').trim();
    const nav = node.dataset.nav;
    if (!nav) continue;
    out.push({
      id: 'nav:' + nav, label: label, group: group || '页面',
      hint: (node.dataset.title || '') + ' ' + (node.dataset.sub || ''),
      run: () => { if (typeof window.navGo === 'function') window.navGo(nav); },
    });
  }
  return out;
}

// paletteSources 允许页面注册**动态**数据源（账号 / 用户 / 密钥 …）。
// 契约：{ name, search(q) -> Promise<[{id,label,group,hint,run}]> }
// 只在查询非空时调用，且带最短长度门槛 —— 每敲一个字就打一次接口是很容易犯的错。
window.paletteSources = window.paletteSources || [];

function openCommandPalette() {
  if ($('paletteBack')) return;
  const back = el('div', 'pal-bk');
  back.id = 'paletteBack';
  const box = el('div', 'pal');
  box.setAttribute('role', 'dialog');
  box.setAttribute('aria-modal', 'true');
  box.setAttribute('aria-label', '命令面板');

  const row = el('div', 'pal-row');
  row.appendChild(el('span', 'pal-ico', '⌕'));
  const input = el('input', 'pal-input');
  input.id = 'palInput'; // 便于自动化用例直接定位（也是"输入框要有 id"的常规要求）
  input.type = 'text';
  input.placeholder = '搜索页面、账号、用户…';
  input.setAttribute('role', 'combobox');
  input.setAttribute('aria-expanded', 'true');
  input.setAttribute('aria-controls', 'palList');
  input.setAttribute('autocomplete', 'off');
  row.appendChild(input);
  const kbd = el('kbd', 'pal-kbd', 'esc');
  row.appendChild(kbd);
  box.appendChild(row);

  const list = el('div', 'pal-list');
  list.id = 'palList';
  list.setAttribute('role', 'listbox');
  box.appendChild(list);

  const foot = el('div', 'pal-foot');
  foot.appendChild(el('span', '', '↑↓ 选择 · ↵ 打开 · esc 关闭'));
  const count = el('span', 'pal-count', '');
  foot.appendChild(count);
  box.appendChild(foot);
  back.appendChild(box);
  document.body.appendChild(back);

  let items = navCommands();
  let active = 0;
  const dynamic = [];      // 动态结果（来自 paletteSources）
  let dynSeq = 0;

  const render = () => {
    const all = items.concat(dynamic);
    list.textContent = '';
    if (!all.length) {
      const e = el('div', 'pal-empty');
      e.appendChild(el('div', 'pal-empty-t', '没有匹配项'));
      e.appendChild(el('div', 'pal-empty-d', '换一个关键词，或清空后按 ↵ 展开全部页面'));
      list.appendChild(e);
      count.textContent = '共 0 项';
      return;
    }
    if (active >= all.length) active = all.length - 1;
    all.forEach((it, i) => {
      const n = el('div', 'pal-item' + (i === active ? ' on' : ''));
      n.setAttribute('role', 'option');
      n.setAttribute('aria-selected', i === active ? 'true' : 'false');
      n.dataset.href = it.id;
      n.appendChild(el('span', 'pal-lb', it.label));
      n.appendChild(el('span', 'pal-gp', it.group || ''));
      n.onclick = () => pick(all, i);
      // 鼠标移过就跟着高亮：否则鼠标与键盘各指一处，回车打开的是"另一条"。
      n.onmousemove = () => { if (active !== i) { active = i; paint(); } };
      list.appendChild(n);
    });
    count.textContent = '共 ' + all.length + ' 项';
    const cur = list.querySelector('.pal-item.on');
    // block:'nearest' 而不是 center：一次只挪一格，居中会让整个列表跟着跳。
    scrollIntoViewNearest(cur);
  };

  // paint 只切高亮类，不重建 DOM —— 键盘连续按 ↓ 时重建会让滚动条抖动。
  const paint = () => {
    const nodes = list.querySelectorAll('.pal-item');
    nodes.forEach((n, i) => {
      n.classList.toggle('on', i === active);
      n.setAttribute('aria-selected', i === active ? 'true' : 'false');
    });
    scrollIntoViewNearest(nodes[active]);
  };

  const pick = (all, i) => {
    const it = all[i];
    if (!it) return;
    close();
    if (typeof it.run === 'function') it.run();
  };

  const close = () => {
    document.removeEventListener('keydown', onKey, true);
    if (back.parentNode) back.parentNode.removeChild(back);
  };

  const allNow = () => items.concat(dynamic);

  const onKey = (ev) => {
    // 中文/日文输入法：回车是"确认候选词"而不是"打开这一项"。
    // 不拦掉的话，拼音打 zhanghao 再回车会直接跳走，跳到哪一条取决于当时高亮是谁。
    if (ev.isComposing || ev.keyCode === 229) return;
    const n = allNow().length;
    if (ev.key === 'Escape') { ev.preventDefault(); ev.stopPropagation(); close(); }
    else if (ev.key === 'ArrowDown') { ev.preventDefault(); if (n) { active = (active + 1) % n; paint(); } }
    else if (ev.key === 'ArrowUp') { ev.preventDefault(); if (n) { active = (active - 1 + n) % n; paint(); } }
    else if (ev.key === 'Home') { ev.preventDefault(); active = 0; paint(); }
    else if (ev.key === 'End') { ev.preventDefault(); active = Math.max(0, n - 1); paint(); }
    else if (ev.key === 'Enter') { ev.preventDefault(); pick(allNow(), active); }
  };

  // 输入：同步过滤页面；动态源异步追加（带序号防乱序覆盖）。
  const onInput = async () => {
    const q = input.value.trim().toLowerCase();
    // ⚠️ 必须**按得分排序**，不能只拿 matchScore 当过滤器用。
    // 踩过的坑：搜「代理」时，候选有「代理」（标签命中，80 分）与「账号池」
    //（只有说明里出现过"代理"，10 分）；而菜单顺序里账号池在前 —— 只过滤不排序，
    // 回车打开的就是**账号池**，用户看到的却是自己搜的词指向另一个页面。
    // 同分时按菜单原序（稳定），避免键盘操作的目标在两次输入间跳来跳去。
    items = q ? navCommands()
      .map((it, idx) => ({ it: it, score: matchScore(it, q), idx: idx }))
      .filter((x) => x.score >= 0)
      .sort((a, b) => (b.score - a.score) || (a.idx - b.idx))
      .map((x) => x.it)
      : navCommands();
    active = 0;
    if (q.length < 2) { dynamic.length = 0; render(); return; }
    const seq = ++dynSeq;
    const results = await Promise.all(window.paletteSources.map((s) => {
      try { return Promise.resolve(s.search(q)).catch(() => []); } catch (e) { return Promise.resolve([]); }
    }));
    if (seq !== dynSeq) return; // 已有更新的查询在跑，丢弃这次的迟到结果
    dynamic.length = 0;
    for (const arr of results) for (const it of (arr || [])) dynamic.push(it);
    dynamic.splice(60); // 动态结果本身也要有上限
    render();
  };

  input.addEventListener('input', onInput);
  document.addEventListener('keydown', onKey, true);
  back.onclick = (ev) => { if (ev.target === back) close(); };
  render();
  try { input.focus(); } catch (e) {}
}

// scrollIntoViewNearest 把元素滚进视野，并做**能力检测**。
//
// 为什么要检：jsdom 没有实现 scrollIntoView，直接调会抛
// "cur.scrollIntoView is not a function"，而它出现在键盘移动高亮这种
// **高频路径**上 —— 一抛就把整个渲染打断（表现为"打开面板打字没反应"）。
// 真实浏览器都有这个方法，所以这是纯粹的防御，不影响正常功能。
function scrollIntoViewNearest(node) {
  if (!node || typeof node.scrollIntoView !== 'function') return;
  try { node.scrollIntoView({ block: 'nearest' }); } catch (e) { /* 老浏览器忽略 */ }
}

// matchScore 命中得分（越大越靠前）；-1 = 不命中。
// 只认"子串命中"，且**标签命中优先于提示命中** —— 搜"账"时「账号池」应当排在
// 提示里恰好也有"账"字的其它页前面。
function matchScore(it, q) {
  const label = String(it.label || '').toLowerCase();
  if (label === q) return 100;
  if (label.indexOf(q) === 0) return 80;
  if (label.indexOf(q) >= 0) return 60;
  if (String(it.group || '').toLowerCase().indexOf(q) >= 0) return 20;
  if (String(it.hint || '').toLowerCase().indexOf(q) >= 0) return 10;
  return -1;
}

// mountPaletteTrigger 在顶栏插一个**可见的**入口。
// 快捷键只对已经知道它的人有用；这一批的目标是"可发现性"，所以必须有个能点的按钮。
function mountPaletteTrigger() {
  const bar = document.querySelector('.topbar .actions');
  if (!bar || $('btnPalette')) return;
  const b = el('button', 'ghost sm pal-trigger');
  b.id = 'btnPalette';
  b.type = 'button';
  b.title = '搜索页面与数据（Ctrl/⌘ + K）';
  b.appendChild(el('span', 'pal-trigger-ico', '⌕'));
  b.appendChild(el('span', 'pal-trigger-t', '搜索'));
  const k = el('kbd', 'pal-kbd', isMac() ? '⌘K' : 'Ctrl K');
  b.appendChild(k);
  b.onclick = openCommandPalette;
  bar.insertBefore(b, bar.firstChild);
}

function isMac() {
  try { return /mac|iphone|ipad|ipod/i.test(navigator.userAgent); } catch (e) { return false; }
}

// mountShortcuts 全局快捷键。
//
// 刻意只装三条，且都避开输入态：
//   Ctrl/⌘+K  命令面板（浏览器自带的 Ctrl+K 会去聚焦地址栏，必须拦掉默认行为）
//   /         聚焦当前页的搜索框（不是输入框里时才生效）
//   Esc       关掉最上层的浮层（命令面板 / 对话框各自处理，这里兜住"焦点在输入框"的情况）
function mountShortcuts() {
  document.addEventListener('keydown', (ev) => {
    const t = ev.target || {};
    const typing = t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.tagName === 'SELECT' || t.isContentEditable;
    if ((ev.metaKey || ev.ctrlKey) && !ev.altKey && String(ev.key).toLowerCase() === 'k') {
      ev.preventDefault();
      openCommandPalette();
      return;
    }
    if (typing) return;
    if (ev.key === '/') {
      const box = document.querySelector('.pane.active input[type="text"], .pane.active input:not([type])');
      if (box) { ev.preventDefault(); box.focus(); box.select && box.select(); }
    }
  });
}

// ---------------------------------------------------------------- ④ 常驻失败横幅
//
// 失败**不能只弹 toast**：toast 几秒后消失，用户切回来时只看到"一切正常"的数字，
// 根本不知道刚才刷新失败过 —— 那些数字可能已经不是最新的了。
// 横幅常驻到下一次刷新成功为止。
// key 是**必须**传的（本轮踩过的坑）：实时概览页里「实时态势」与「号池仪表盘」
// 两块共用同一个 .pane 作为宿主，各自成功时都会清横幅 —— 用一个共享的 banner
// 节点，就会出现"另一块失败刷出来的横幅被这一块的成功清掉"，用户什么也看不到。
// 按 key 隔离后，谁的横幅归谁清。
function loadBanner(host, msg, retry, key) {
  if (!host) return null;
  const k = key || 'default';
  clearLoadBanner(host, k);
  const bar = el('div', 'load-banner');
  bar.dataset.banner = k;
  bar.setAttribute('role', 'status');
  bar.appendChild(el('span', 'load-banner-ico', '!'));
  bar.appendChild(el('span', 'load-banner-msg', msg || '本次刷新未成功，页面显示的可能不是最新数据。'));
  if (retry) {
    const b = el('button', 'ghost sm', '重试');
    b.type = 'button';
    b.onclick = () => { clearLoadBanner(host); retry(); };
    bar.appendChild(b);
  }
  const x = el('button', 'toast-x', '×');
  x.type = 'button';
  x.setAttribute('aria-label', '关闭提示');
  x.onclick = () => clearLoadBanner(host);
  bar.appendChild(x);
  host.insertBefore(bar, host.firstChild);
  return bar;
}

function clearLoadBanner(host, key) {
  if (!host) return;
  const k = key || 'default';
  for (const old of Array.from(host.querySelectorAll('[data-banner="' + k + '"]'))) {
    if (old.parentNode) old.parentNode.removeChild(old);
  }
}

// ---------------------------------------------------------------- ⑤ 骨架屏
//
// 为什么值得做：首屏从"文字 加载中…"变成"形状先出现、内容后到位"，
// 感知速度明显更快，而且不会在数据到达时整体跳动（布局抖动比慢更难受）。
function skeletonRows(tbody, rows, cols) {
  if (!tbody) return;
  tbody.textContent = '';
  for (let i = 0; i < (rows || 3); i++) {
    const tr = el('tr', 'skel-row');
    for (let c = 0; c < (cols || 3); c++) {
      const td = el('td');
      const s = el('i', 'skel');
      s.style.width = (35 + ((i * 7 + c * 13) % 45)) + '%';
      td.appendChild(s);
      tr.appendChild(td);
    }
    tbody.appendChild(tr);
  }
}

function skeletonCards(box, n, kind) {
  if (!box) return;
  box.textContent = '';
  for (let i = 0; i < (n || 3); i++) {
    const c = el('div', 'skel-card ' + (kind || ''));
    c.appendChild(el('i', 'skel skel-line', ''));
    c.appendChild(el('i', 'skel skel-line short', ''));
    c.appendChild(el('i', 'skel skel-line', ''));
    box.appendChild(c);
  }
}

function skeletonBlock(box, lines) {
  if (!box) return;
  box.textContent = '';
  for (let i = 0; i < (lines || 3); i++) {
    const s = el('i', 'skel skel-line');
    s.style.width = (40 + ((i * 17) % 50)) + '%';
    box.appendChild(s);
  }
}

// ---------------------------------------------------------------- ⑥ KPI 数字滚动
//
// 增强层：**不改各页已有的赋值逻辑**，只在渲染完成后扫一遍 KPI 数字节点，
// 从"上一次的值"滚到"这一次的值"。这样接入成本接近零，也不会引入新的一致性问题
// （每个数字该显示什么，仍然由原来那段代码决定）。
function animateKpiNumbers(root) {
  const scope = root || document;
  const nodes = scope.querySelectorAll('.kpi-card .v, [data-count]');
  nodes.forEach((node) => {
    // 只取**直接文本节点**：`.v` 里可能还嵌着单位 <span class="u">点</span>，
    // 读 textContent 会得到 "12.5点"，被当成非数字而静默跳过（那就白做了）。
    let tn = null;
    for (const n of node.childNodes) {
      if (n.nodeType === 3 && String(n.textContent).trim() !== '') { tn = n; break; }
    }
    if (!tn) return;
    const raw = String(tn.textContent).trim();
    if (!/^-?[\d,]+(\.\d+)?$/.test(raw)) return;
    const to = Number(raw.replace(/,/g, ''));
    if (!isFinite(to)) return;
    const prev = Number(node.dataset.countPrev);
    node.dataset.countPrev = String(to);
    if (!isFinite(prev) || prev === to || prefersReduced()) return;
    const dec = (raw.split('.')[1] || '').length;
    const t0 = performance.now();
    const dur = 420;
    const step = (t) => {
      const p = Math.min(1, (t - t0) / dur);
      const e = 1 - Math.pow(1 - p, 3); // easeOutCubic
      const v = prev + (to - prev) * e;
      tn.textContent = v.toLocaleString('zh-CN', { minimumFractionDigits: dec, maximumFractionDigits: dec });
      if (p < 1) requestAnimationFrame(step);
      else tn.textContent = to.toLocaleString('zh-CN', { minimumFractionDigits: dec, maximumFractionDigits: dec });
    };
    requestAnimationFrame(step);
  });
}

// ---------------------------------------------------------------- ⑦ 侧边栏过滤
//
// 菜单涨到十几个之后，"用眼睛在列表里找"开始变慢。加一个过滤框把它变成"打两个字"。
// 分组标题在整组被过滤空时一并隐藏 —— 留着空标题会让人以为下面还有内容。
function mountNavFilter() {
  const nav = document.querySelector('.sidebar .nav');
  if (!nav || $('navFilter')) return;
  const wrap = el('div', 'nav-filter-wrap');
  const input = el('input', 'nav-filter');
  input.id = 'navFilter';
  input.type = 'text';
  input.placeholder = '过滤页面…';
  input.setAttribute('aria-label', '过滤侧边栏页面');
  const clear = el('button', 'nav-filter-x', '×');
  clear.type = 'button';
  clear.setAttribute('aria-label', '清空过滤');
  wrap.appendChild(el('span', 'nav-filter-ico', '⌕'));
  wrap.appendChild(input);
  wrap.appendChild(clear);
  nav.parentNode.insertBefore(wrap, nav);

  const apply = () => {
    const q = input.value.trim().toLowerCase();
    clear.style.display = q ? '' : 'none';
    let shown = 0;
    const groupNodes = Array.from(nav.querySelectorAll('.nav-group'));
    for (const it of Array.from(nav.querySelectorAll('.nav-item'))) {
      const hay = ((it.textContent || '') + ' ' + (it.dataset.title || '') + ' ' + (it.dataset.sub || '')).toLowerCase();
      const hit = !q || hay.indexOf(q) >= 0;
      it.style.display = hit ? '' : 'none';
      if (hit) shown++;
    }
    // 分组标题：整组都被过滤掉就隐藏。
    for (const g of groupNodes) {
      let n = 0;
      for (let s = g.nextElementSibling; s && s.classList.contains('nav-item'); s = s.nextElementSibling) {
        if (s.style.display !== 'none') n++;
      }
      g.style.display = n ? '' : 'none';
    }
    let hint = nav.parentNode.querySelector('.nav-filter-empty');
    if (!shown && q) {
      if (!hint) { hint = el('div', 'nav-filter-empty', '没有匹配的页面'); nav.parentNode.insertBefore(hint, nav.nextSibling); }
    } else if (hint) hint.parentNode.removeChild(hint);
  };
  input.addEventListener('input', apply);
  clear.onclick = () => { input.value = ''; apply(); input.focus(); };
  clear.style.display = 'none';
}

// ---------------------------------------------------------------- ⑧ 初始化
//
// 两个控制台共用：顶栏按钮 + 快捷键 + 侧边栏过滤。各页只需在 init 里调一次。
function mountUiShell() {
  mountPaletteTrigger();
  mountShortcuts();
  mountNavFilter();
}
