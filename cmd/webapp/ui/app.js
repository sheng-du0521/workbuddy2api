// app.js —— 全站共享的前端工具（无依赖，原生 DOM）。
// 页面结构约定：
//   侧边栏导航 → .nav-item[data-nav][data-on]；内容区 → .pane[data-pane]
//   路由/切换由 setupNav() 统一接管；登录页仍用 setupTabs()（顶部 tab）。

const $ = (id) => document.getElementById(id);

// toast 的实现见文件末尾「共享 UI 套件」段（v0.26.6 改为**堆叠卡片**：
// 图标 / 可关闭 / 悬停暂停 / aria-live）。旧实现是单元素、2.8 秒自动消失，
// 错误提示切个页就找不到了 —— 而"刚才失败过"恰恰是最需要留下的信息。
// 签名保持 toast(msg, kind) 兼容，第三参可选 { duration, sticky }。

// downloadJSONFrom 从**需要鉴权**的接口下载 JSON 文件（v0.31.29）。
//
// 为什么不用 `<a href>` 直接点：接口出错时浏览器会跳到一屏 JSON 错误页，
// 用户看到的是乱码而不是"导出失败的原因"。用 fetch 取 blob 才能把后端原文抛出来。
// 为什么先确认再下载：导出的文件里是 access_token / refresh_token **明文**，
// 拿到它就等于拿到账号 —— 这一句话必须由调用方在 confirm 里说清。
async function downloadJSONFrom(url, fallbackName) {
  const r = await fetch(url, { credentials: 'same-origin' });
  if (!r.ok) {
    let t = '';
    try { t = (await r.text()).slice(0, 200); } catch (e) { /* 忽略读失败 */ }
    throw new Error(t || ('HTTP ' + r.status));
  }
  // 文件名优先用服务端 Content-Disposition（带时间戳，避免多次导出同名覆盖）。
  const disp = r.headers.get('Content-Disposition') || '';
  const m = /filename="?([^";]+)"?/.exec(disp);
  const name = (m && m[1]) || fallbackName || 'export.json';
  const blob = await r.blob();
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = name;
  document.body.appendChild(a);
  a.click();
  setTimeout(() => {
    try { URL.revokeObjectURL(a.href); a.remove(); } catch (e) { /* 忽略 */ }
  }, 1000);
}

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

// writeClipboard 把文本写进系统剪贴板；返回是否成功。
//
// 为什么要显式降级（v0.31.18，用户现场驱动）：navigator.clipboard 只在
// **安全上下文**（HTTPS / localhost）里存在。本服务常部署为
// http://<公网IP>:8780 —— 那里它是 undefined，直接调用抛错，用户只看到
// 「复制失败，请手动选择」；而密钥卡片默认**只渲染掩码**，
// “手动选择”选到的就是掩码 → 粘到客户端里报 key 无效。
// 复制按钮坏掉 = 用户拿不到密钥，这是整条链路上最贵的一个小故障。
// 降级路径（隐藏 textarea + execCommand('copy')）在 http 下可用。
function writeClipboard(text) {
  const s = String(text == null ? '' : text);
  if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
    return navigator.clipboard.writeText(s).then(() => true, () => legacyCopy(s));
  }
  return Promise.resolve(legacyCopy(s));
}

// legacyCopy 非安全上下文下的复制兜底（execCommand 已废弃，但仍是 http 下唯一可用手段）。
function legacyCopy(s) {
  try {
    const ta = document.createElement('textarea');
    ta.value = s;
    ta.setAttribute('readonly', '');
    ta.style.cssText = 'position:fixed;top:-1000px;left:-1000px;opacity:0;';
    document.body.appendChild(ta);
    ta.select();
    ta.setSelectionRange(0, s.length);
    const ok = document.execCommand('copy');
    document.body.removeChild(ta);
    return !!ok;
  } catch (e) {
    return false;
  }
}

async function copyText(text) {
  if (await writeClipboard(text)) toast('已复制到剪贴板', 'ok');
  else toast('复制失败，请手动选择', 'err');
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
// hostStateTag 代挂账号的入池状态标签。
// 五态（v0.31.18 起）：in 已入池 / pending 已申请待审 / invited **收到邀请待你确认** /
// rejected 已驳回 / 其它 未入池。
// 拆出 invited 这一态是刻意的：它与 pending 的**行动方**相反**
// （pending = 等管理员 ； invited = 等你自己），合成一个“待审”会让号主
// 一直等一个永远不会到的审核。
function hostStateTag(ps) {
  if (ps === 'in') return el('span', 'tag ok', '已入池');
  if (ps === 'invited') return el('span', 'tag warn', '邀请你入池');
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
  let initial = (location.hash || '').replace('#', '');
  // 兼容老链接（v0.31.34）：「代理」曾是一级导航，现在并进插件。
  // 直接落到第一个导航项会让人以为"页面没了"，所以转成插件页 + 稍后自动打开该插件。
  if (initial === 'egress') {
    window.__openPluginAfterLoad = 'host-egress';
    initial = 'plugins';
  }
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
    const b = el('div', 'bar' + (req === 0 ? ' empty' : ''));
    b.style.height = (req === 0 ? 3 : Math.max(7, Math.round((req / max) * 100))) + '%';
    // 失败段（v0.30.0）：把"这根柱子里有多少失败"画成柱内的红色段。
    //
    // 此前只有"整根柱子染红"这一种表达，它在两种情况下看不出任何信息：
    //   · 1 成功 + 9 失败 与 10 成功 —— 柱子**一样高**，只有颜色不同，
    //     而颜色没有比例含义，运维读不出"这一分钟基本全挂了"；
    //   · 全失败的分钟（req>0 且 ok=0）与"含少量失败"的分钟是同一种红。
    // 柱内分段让"失败占比"直接可见，且与"空闲"（req=0 的灰柱）互不混淆。
    if (req > 0 && fail > 0) {
      const seg = el('i');
      // 最小 6%：占比极低时也要留一条可见的红线，否则"有失败"会看不出来。
      seg.style.height = Math.max(6, Math.round((fail / req) * 100)) + '%';
      b.appendChild(seg);
    }
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
  //
  // ⚠️ 判断顺序是**先 ok、后 supported**（v0.31.28 修正）。
  // 反过来的话，一次查询失败（supported 是 Go 零值 false）会被渲染成「签到：不支持」——
  // 2026-10-18 全池 token 被上游作废时，用户看到的就是"账号签到不支持"，
  // 而真实原因是凭证失效。**"没查成"与"不支持"是两件事，不能共用一句话。**
  if (!c.ok) box.appendChild(chipWithTitle('签到：查询失败', 'err', c.error));
  else if (c.supported === false) box.appendChild(el('span', 'tag', '签到：不支持'));
  else if (c.checked_in) box.appendChild(el('span', 'tag ok', '✓ 今日已签到'));
  else box.appendChild(el('span', 'tag warn', '今日未签到'));
  // 猫猫旅行（同上：先 ok）
  if (!t.ok) box.appendChild(chipWithTitle('旅行：查询失败', 'err', t.error));
  else if (t.supported === false) box.appendChild(el('span', 'tag', '旅行：不支持'));
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
    if (!(await writeClipboard(val))) {
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

// ======================================================================
// 提醒中心（v0.31.20）
//
// 为什么要有它：toast 几秒就消失、侧栏 badge 只覆盖单一事项。于是用户遇到
// 「点了复制没反应」「点某个页面报错」这类事时，**过一会儿就再也拿不出痕迹**，
// 只能凭记忆描述 —— 我们也只能靠猜。提醒中心把三类信息留在右上角：
//
//   · 操作反馈（复制 / 保存 / 失败…）—— 从 `toast()` 自动落一份，可回溯；
//   · 待办（入池邀请等）—— 需要用户做决定的事；
//   · **前端错误**（window.onerror / unhandledrejection）—— 这是"点进去报错"
//     唯一能被复现的证据，可整段复制发出来。
//
// 存 localStorage（最近 50 条）：刷新后仍在，否则"关页面就没了"等于没有。
// ======================================================================
const ALERT_KEY = 'wb_alerts';
const ALERT_MAX = 50;
let _alerts = null;   // null = 尚未从盘上读过
let _alertsSeen = 0;  // 已读游标（unix 秒）

function alertLoad() {
  if (_alerts) return _alerts;
  _alerts = [];
  try {
    const d = JSON.parse(localStorage.getItem(ALERT_KEY) || '{}');
    if (d && Array.isArray(d.items)) _alerts = d.items;
    _alertsSeen = Number(d && d.seen) || 0;
  } catch (e) { /* 解析失败当空处理，绝不因此让页面挂掉 */ }
  return _alerts;
}

function alertSave() {
  try {
    localStorage.setItem(ALERT_KEY, JSON.stringify({ items: _alerts.slice(0, ALERT_MAX), seen: _alertsSeen }));
  } catch (e) { /* 隐私模式：仅本次会话可见 */ }
}

// alertPush 记一条提醒。**绝不能在这里调 toast**（自激循环）。
function alertPush(rec) {
  alertLoad();
  const text = String((rec && rec.text) || '').slice(0, 800);
  const detail = String((rec && rec.detail) || '').slice(0, 4000);
  if (!text && !detail) return;
  const at = Math.floor(Date.now() / 1000);
  // 同文去重（连点 5 次"复制失败"不该刷 5 条 —— 刷屏会让真正的那条被淹掉）。
  const dup = _alerts.find((x) => x.text === text && x.kind === rec.kind && at - x.at < 10);
  if (dup) {
    dup.at = at;
    if (detail) dup.detail = detail;
  } else {
    _alerts.unshift({ at, level: rec.level || 'info', kind: rec.kind || 'action', text, detail });
  }
  if (_alerts.length > ALERT_MAX) _alerts.length = ALERT_MAX;
  alertSave();
  alertRefreshBadge();
}

// alertRefreshBadge 刷新铃铛上的未读数。
function alertRefreshBadge() {
  const bell = document.getElementById('alertBell');
  if (!bell) return;
  alertLoad();
  const unread = _alerts.filter((x) => x.at > _alertsSeen).length;
  const badge = bell.querySelector('.alert-badge');
  if (badge) {
    badge.textContent = unread > 99 ? '99+' : String(unread);
    badge.style.display = unread ? '' : 'none';
  }
}

function alertLevelColor(level) {
  if (level === 'err') return 'var(--bad, #c62828)';
  if (level === 'warn') return '#e6a23c';
  return '#888';
}

// alertFill 渲染面板内容。
function alertFill(panel) {
  alertLoad();
  panel.textContent = '';
  const head = el('div');
  head.style.cssText = 'display:flex;align-items:center;gap:8px;margin-bottom:4px;';
  head.appendChild(el('b', null, '提醒中心'));
  head.appendChild(el('span', 'muted', _alerts.length ? '共 ' + _alerts.length + ' 条' : ''));
  const clr = el('button', 'ghost sm', '清空');
  clr.style.marginLeft = 'auto';
  clr.onclick = () => {
    _alerts = [];
    _alertsSeen = Math.floor(Date.now() / 1000);
    alertSave();
    alertFill(panel);
    alertRefreshBadge();
  };
  head.appendChild(clr);
  panel.appendChild(head);

  if (!_alerts.length) {
    panel.appendChild(el('div', 'sub',
      '暂无提醒。操作反馈（复制 / 保存等）、入池邀请、以及**页面脚本错误**都会记在这里 —— '
      + '遇到"点进去报错"时，把这里的原文发出来即可定位。'));
    return;
  }
  for (const it of _alerts) {
    const row = el('div');
    row.style.cssText = 'padding:6px 0;border-top:1px solid rgba(127,127,127,.15);';
    const top = el('div');
    top.style.cssText = 'display:flex;gap:6px;align-items:baseline;';
    const ico = el('span', null, it.kind === 'error' ? '⚠' : (it.kind === 'todo' ? '📌' : '·'));
    ico.style.color = alertLevelColor(it.level);
    top.appendChild(ico);
    const tx = el('span', null, it.text || '（无内容）');
    tx.style.flex = '1';
    tx.style.wordBreak = 'break-word';
    top.appendChild(tx);
    top.appendChild(el('span', 'muted', typeof fmtClock === 'function' ? fmtClock(it.at) : ''));
    row.appendChild(top);
    if (it.detail) {
      const pre = el('div', 'sub');
      pre.style.cssText = 'margin:4px 0 0;padding:6px 8px;background:rgba(127,127,127,.12);border-radius:6px;'
        + 'font-family:ui-monospace,Consolas,monospace;font-size:11px;white-space:pre-wrap;word-break:break-all;max-height:140px;overflow:auto;';
      pre.textContent = it.detail;
      row.appendChild(pre);
      const cp = el('button', 'ghost sm', '复制这条');
      cp.style.marginTop = '4px';
      cp.onclick = async () => {
        const ok = await writeClipboard(it.text + '\n' + it.detail);
        toast(ok ? '已复制错误详情' : '复制失败，请手动选择', ok ? 'ok' : 'err');
      };
      row.appendChild(cp);
    }
    panel.appendChild(row);
  }
}

// mountAlertCenter 在顶栏右侧挂上铃铛 + 面板（两端共用）。
function mountAlertCenter() {
  const bar = document.querySelector('.topbar .actions');
  if (!bar || document.getElementById('alertBell')) return;
  alertLoad();

  const bell = el('button', 'icon-btn');
  bell.id = 'alertBell';
  bell.type = 'button';
  bell.title = '提醒中心：操作反馈 / 待办 / 页面错误（可复制原文）';
  bell.style.position = 'relative';
  bell.appendChild(el('span', null, '🔔'));
  const badge = el('span', 'alert-badge');
  badge.style.cssText = 'position:absolute;top:-4px;right:-4px;min-width:16px;height:16px;border-radius:8px;'
    + 'background:var(--bad,#c62828);color:#fff;font-size:10px;line-height:16px;text-align:center;padding:0 4px;display:none;';
  bell.appendChild(badge);

  const panel = el('div');
  panel.id = 'alertPanel';
  panel.style.cssText = 'display:none;position:fixed;top:56px;right:12px;z-index:80;width:min(430px,88vw);'
    + 'max-height:62vh;overflow:auto;background:var(--bg,#fff);border:1px solid rgba(127,127,127,.3);'
    + 'border-radius:10px;box-shadow:0 8px 28px rgba(0,0,0,.18);padding:10px 12px;';

  bell.onclick = (e) => {
    e.stopPropagation();
    if (panel.style.display !== 'none') { panel.style.display = 'none'; return; }
    _alertsSeen = Math.floor(Date.now() / 1000);
    alertSave();
    alertFill(panel);
    panel.style.display = '';
    alertRefreshBadge();
  };
  document.addEventListener('click', () => { panel.style.display = 'none'; });
  panel.addEventListener('click', (e) => e.stopPropagation());

  bar.appendChild(bell);
  // panel 挂 body（`.topbar` 有 overflow 时会裁掉它）。
  document.body.appendChild(panel);
  alertRefreshBadge();

  // ① 脚本错误 / 资源加载失败 → 提醒中心（"点进去报错"的可复现证据）。
  window.addEventListener('error', (ev) => {
    const t = ev.target;
    if (t && t !== window && t.tagName && /^(SCRIPT|LINK|IMG)$/.test(t.tagName)) {
      alertPush({
        level: 'warn', kind: 'error',
        text: '资源加载失败：' + (t.src || t.href || ''),
        detail: '若反复出现，多半是浏览器缓存了旧版本前端 —— 强制刷新（Ctrl+F5）可恢复。',
      });
      return;
    }
    alertPush({
      level: 'err', kind: 'error',
      text: '页面脚本错误：' + (ev.message || '未知错误'),
      detail: (ev.filename || '') + ':' + (ev.lineno || 0) + ':' + (ev.colno || 0)
        + '\n' + ((ev.error && ev.error.stack) || '(无堆栈)'),
    });
  });
  // ② 未处理的 Promise 拒绝（fetch 忘了 catch 时走这条）。
  window.addEventListener('unhandledrejection', (ev) => {
    const r = ev.reason;
    alertPush({
      level: 'err', kind: 'error',
      text: '未处理的异步错误：' + ((r && r.message) || String(r)),
      detail: (r && r.stack) || '',
    });
  });
}

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

  // 落一份到提醒中心（v0.31.20）：toast 会消失、这里不会 —— 用户回头能查
  // "刚才那步到底成没成"，我们也能拿到"复制失败"这类现场。
  if (!opts.noAlert) {
    alertPush({
      level: (kind === 'err' || kind === 'bad') ? 'warn' : 'info',
      kind: 'action',
      text: String(msg == null ? '' : msg),
    });
  }

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

// ------------------------------------------------------- 开关兜底（v0.31.22）
//
// 本项目的 `.switch` 把真实 input 做成 0×0 透明（见 style.css 的
// `.switch input { position:absolute; opacity:0; width:0; height:0 }`），
// 于是**只有被 `<label>` 包裹时**点击才会落到 input 上。
//
// admin.html 里有三处是裸写的 ——「启用定时任务」「开放注册」「调试对话-流式」：
// 它们**看起来是开关，实际点不动**（jsdom 实证：点 <i> 前后 checked 不变）。
// 用户不会报"这个开关坏了"，只会觉得"设了没用"。
//
// 这里做一次事件委托，让"裸开关"与"label 包裹的开关"行为一致。
//
// ⚠️ 已被 label 包裹的直接 return：那种情况浏览器（label 激活行为）会处理，
// 我们再翻一次等于**抵消**（翻两下回到原状）。
document.addEventListener('click', (ev) => {
  const t = ev.target;
  if (!t || !t.closest) return;
  const sw = t.closest('.switch');
  if (!sw) return;
  if (sw.closest('label')) return; // label 会自己激活，别抢
  const cb = sw.querySelector('input[type="checkbox"]');
  if (!cb || cb.disabled) return;
  if (t === cb) return; // 直接点到 input：浏览器已处理
  cb.checked = !cb.checked;
  cb.dispatchEvent(new Event('change', { bubbles: true }));
});

// ---------------------------------------------------------------- ⑨ 插件中心（v0.31.16）
//
// 两端共用（admin / dashboard）：页面只提供
//   <section class="pane" data-pane="plugins" data-plugin-scope="admin|user">
// 其余（列表 / 启停 / 选号 / 执行 / 逐号结果）全部在这里。
//
// 这么分的好处：**加插件不用改前端**。后端注册表下发什么，这里就渲染什么；
// 页面只声明"我是哪一端"，由服务端决定该端能看到哪些插件、能碰哪些账号。
//
// 两条硬纪律（服务端各还有一道，这里是前端侧）：
//   1. **串行**：一次只发一个号的请求，号与号之间留间隔 —— 绝不并发轰上游。
//      2026-10-13 的事故形态就是"59 个并发请求、同一出口 IP、几秒内打完"，
//      直接换来上游风控。批量功能在这里必须比手动操作更保守，不能更激进。
//   2. **不用原生 confirm/alert**：WebView 下会静默返回 null（"点了没反应也没报错"），
//      统一走共享的 confirmAsk / toast。
const PLUGIN_GAP_MS = 350; // 号与号之间的间隔（服务端另有 30s/号 的闸门）

// pluginScope 当前页面属于哪一端（决定请求哪个前缀）。
function pluginScope() {
  const pane = document.querySelector('.pane[data-plugin-scope]');
  return pane && pane.dataset.pluginScope === 'admin' ? 'admin' : 'user';
}
function pluginBase() {
  return pluginScope() === 'admin' ? '/api/admin/plugins' : '/api/plugins';
}

// 每个插件的展开态与执行态（按插件 id 分开存，互不干扰）。
const pluginState = {};

// loadPlugins 插件页入口（nav 的 data-on 指向它）。
async function loadPlugins() {
  const box = document.getElementById('pluginCards');
  if (!box) return;
  box.textContent = '';
  box.appendChild(el('div', 'sub', '正在读取插件列表…'));
  let data;
  try {
    data = await api(pluginBase());
  } catch (e) {
    box.textContent = '';
    box.appendChild(el('div', 'sub', '读取插件列表失败：' + (e && e.message ? e.message : e)));
    return;
  }
  const list = (data && data.plugins) || [];
  box.textContent = '';
  if (!list.length) {
    box.appendChild(el('div', 'sub', '当前没有可用的插件。'));
    return;
  }
  for (const p of list) box.appendChild(pluginCard(p));
  // 老链接 #egress 的落地（见 setupNav）：列表就绪后自动进入插件。
  const want = window.__openPluginAfterLoad;
  if (want) {
    window.__openPluginAfterLoad = '';
    const one = list.find((x) => x.id === want);
    if (one) openPluginView(one);
  }
}

// pluginToggleSwitch 一张卡上的启停开关（v0.31.22）。
//
// 复用共享的 `switchEl`，不自己拼 checkbox：
//   · `switchEl` 是 **label 包裹**的 —— 本项目的 `.switch` 把 input 做成 0×0 透明，
//     只有 label 能给它点击目标（裸写出来的开关点不动，见文件上方「开关兜底」）；
//   · 它自带"异步期间禁用 + 失败回滚 + 报错 toast"，与全站其它开关行为一致。
//
// 路径由 `pluginBase()` 决定（管理端 /api/admin/plugins、用户端 /api/plugins），
// 所以同一份渲染代码在两端各自打自己的接口 —— 这也是两端开关能共用一套 UI 的前提。
function pluginToggleSwitch(p, on, disabled, text, title) {
  const lab = switchEl(on, async (checked) => {
    try {
      await api(pluginBase() + '/' + encodeURIComponent(p.id) + '/enabled', {
        method: 'POST', body: JSON.stringify({ enabled: checked }),
      });
    } catch (e) {
      // 交回 switchEl 统一处理（回滚 + toast）：这里只负责把话说清。
      throw new Error('切换失败：' + (e && e.message ? e.message : e));
    }
    toast(checked ? '已启用' : '已停用', 'ok');
    loadPlugins();
  });
  lab.title = title || '';
  if (disabled) {
    const inp = lab.querySelector('input');
    if (inp) inp.disabled = true;
  }
  lab.appendChild(el('span', 'muted', text));
  return lab;
}

// ======================================================================
// 插件独立视图（v0.31.34）
//
// 为什么不再用"卡片里展开的小面板"：
//   · 插件本来就是一个个**独立功能**（领取、体检、出口代理），而卡片只有
//     一列宽度、还夹在列表里 —— 逐号表格、隧道状态、机器列表塞进去根本读不清；
//   · 「本地出口代理」这类插件是**持续操作的控制台**，不是"点一次就跑"，
//     必须占满内容区并带自己的返回栏，否则用户会不知道该页属于谁、怎么退出去。
//
// 结构：
//   · 有专属 pane 的插件（egress 复用现有代理页 DOM）→ `.pane[data-plugin-view="<id>"]`；
//   · 其余插件 → 动态建一个通用 pane（返回栏 + 标题 + 说明 + [data-plugin-body]）。
//
// ⚠️ 不写 location.hash：插件视图是"插件"这个导航项的子页，
// 深链回退到 hash 会与 setupNav 的初始解析打架（刷新后会落到第一个导航项）。
// ======================================================================

// pluginViewPane 取得/创建某插件的独立视图容器（幂等）。
function pluginViewPane(p) {
  let pane = document.querySelector('.pane[data-plugin-view="' + p.id + '"]');
  if (pane) return pane;

  pane = el('section', 'pane');
  pane.setAttribute('data-plugin-view', p.id);
  pane.setAttribute('data-pane', 'plugin-' + p.id);

  const bar = el('div', 'row');
  bar.style.cssText = 'align-items:center;gap:10px;margin:0 0 10px;';
  const back = el('button', 'ghost sm', '← 插件');
  back.setAttribute('data-plugin-back', '1');
  bar.appendChild(back);
  bar.appendChild(el('span', 'muted', '插件 · ' + p.name));
  pane.appendChild(bar);

  const head = el('div', 'section-head');
  head.appendChild(el('h2', null, p.name));
  pane.appendChild(head);
  // 卡片上的 desc 由服务端给（可能含 <b>），用 innerHTML 保留强调；
  // 内容是注册表里的常量，不来自用户输入。
  const desc = el('div', 'sub');
  desc.style.margin = '0 0 14px';
  desc.innerHTML = p.desc || '';
  pane.appendChild(desc);

  const body = el('div');
  body.setAttribute('data-plugin-body', '1');
  pane.appendChild(body);

  // 插到「插件」页后面（同级），这样它天然是插件的子页位置。
  const host = document.querySelector('.pane[data-pane="plugins"]');
  if (host && host.parentNode) host.parentNode.insertBefore(pane, host.nextSibling);
  else document.body.appendChild(pane);
  return pane;
}

// openPluginView 打开某插件的独立界面（隐藏其它 pane，侧栏仍停在「插件」）。
function openPluginView(p) {
  const pane = pluginViewPane(p);
  const items = Array.from(document.querySelectorAll('.nav-item'));
  document.querySelectorAll('.pane').forEach((x) => x.classList.remove('active'));
  pane.classList.add('active');
  // 侧栏保持「插件」高亮：用户的位置感是"我在插件里"，而不是"我掉到了别的页"。
  items.forEach((x) => x.classList.toggle('active', x.dataset.nav === 'plugins'));
  setTxt('pageTitle', p.name);
  setTxt('pageSub', '插件 · ' + (p.kind === 'egress' ? '独立控制台' : '批量操作'));
  window.__pluginViewOpen = p.id;
  if (typeof window.onNavChange === 'function') window.onNavChange('plugin:' + p.id);
  // 回到顶部：从列表点进来时页面可能已经滚到很下面，不重置会"打开后一片空白"。
  try { window.scrollTo({ top: 0, behavior: 'auto' }); } catch (e) { /* 忽略 */ }

  if (p.kind === 'egress') {
    // 复用现有代理页加载链（含机器状态 + 我的账号出口两张卡）。
    if (typeof window.loadEgressHome === 'function') window.loadEgressHome();
    return;
  }
  const body = pane.querySelector('[data-plugin-body="1"]');
  if (body) pluginFillPanel(p, pane, body);
}

// closePluginView 回到插件列表。
function closePluginView() {
  window.__pluginViewOpen = '';
  const back = document.querySelector('.nav-item[data-nav="plugins"]');
  if (back) {
    // 复用导航逻辑：它负责恢复标题、隐藏其它 pane、重跑 loadPlugins（刷新状态）。
    back.click();
    return;
  }
  // 兜底（理论上不会走到）：手工恢复。
  document.querySelectorAll('.pane').forEach((x) => x.classList.remove('active'));
  const pane = document.querySelector('.pane[data-pane="plugins"]');
  if (pane) pane.classList.add('active');
  setTxt('pageTitle', '插件');
  if (typeof window.loadPlugins === 'function') window.loadPlugins();
}

// 返回按钮用**事件委托**：pane 可能是动态创建的，逐次绑定容易漏。
if (typeof document !== 'undefined' && !window.__pluginBackBound) {
  window.__pluginBackBound = true;
  document.addEventListener('click', (e) => {
    const t = e.target;
    const btn = t && t.closest ? t.closest('[data-plugin-back]') : null;
    if (!btn) return;
    e.preventDefault();
    closePluginView();
  });
}

// pluginCard 一张插件卡：标题 + 状态 + 启用开关 + 展开的执行面板。
// pluginKindLabel 插件形态的中文标签（v0.31.34）。
//
// 为什么在卡片上标出来：用户看到三张卡却不知道"打开之后会是什么"，
// 而三种形态的交互差别很大（执行一次 / 读一次状态 / 持续控制台）。
// 标签让人在点之前就有预期。
function pluginKindLabel(kind) {
  switch (kind) {
    case 'health': return { text: '检测', title: '只读检查每个号的凭证/余额/对话/出口' };
    case 'egress': return { text: '控制台', title: '持续运行的出口代理：拉起隧道、逐号分配出口' };
    case 'claim': return { text: '批量执行', title: '对选中的号逐个串行执行一次写操作' };
    default: return { text: '批量执行', title: '' };
  }
}

function pluginCard(p) {
  // 状态容器：独立视图里的面板会复用（picked / targets / rows 跨开关保留）。
  const st = (pluginState[p.id] = pluginState[p.id] || {
    targets: null, picked: null, running: false, rows: {},
  });
  const card = el('div', 'card');
  card.style.marginBottom = '14px';

  const isAdminScope = pluginScope() === 'admin';
  // 生效口径由服务端算好（`active = 平台启用 && 用户启用`），前端只渲染 ——
  // 两边各拼一遍 `a && b` 迟早有一边漏。
  const active = isAdminScope ? !!p.enabled : !!p.active;

  const head = el('div', 'section-head');
  head.appendChild(el('h2', null, p.name));
  const badge = el('span', 'tag' + (active ? ' ok' : ''), active ? '已启用' : '已停用');
  badge.style.fontSize = '12px';
  badge.style.marginLeft = '8px';
  head.appendChild(badge);
  // 形态标签：让「打开」之前就知道里面是什么。
  const kl = pluginKindLabel(p.kind);
  const kindTag = el('span', 'tag', kl.text);
  kindTag.style.fontSize = '12px';
  kindTag.style.marginLeft = '6px';
  if (kl.title) kindTag.title = kl.title;
  head.appendChild(kindTag);
  card.appendChild(head);

  card.appendChild(el('div', 'sub', p.desc || ''));
  // 停用时把卡片压暗：一眼能看出"这条现在不生效"，不必去读徽标。
  if (!active) card.style.opacity = '0.72';

  const row = el('div', 'row');
  row.style.flexWrap = 'wrap';
  row.style.alignItems = 'center';
  row.style.gap = '8px';

  if (isAdminScope) {
    // 平台级开关：动的是"要不要把这项能力提供给用户"，影响所有人。
    row.appendChild(pluginToggleSwitch(p, !!p.enabled, false, '全局启用',
      '平台级开关：影响所有用户。用户各自的启用偏好不受它影响（但平台停用时一律不可用）'));
  } else {
    // 用户级开关（v0.31.22）：只影响"我自己"，动不了平台开关。
    const globallyOff = p.enabled === false;
    row.appendChild(pluginToggleSwitch(p, active, globallyOff,
      globallyOff ? '平台已停用' : '启用',
      globallyOff
        ? '该插件已被管理员停用，无法在此启用'
        : '只影响你自己的入口与执行，不影响平台其他用户'));
  }

  // 打开 = 进入**独立界面**（v0.31.34）。不再在卡片里展开小面板。
  //
  // 停用时禁用按钮：点进去也只会拿到 403，不如在入口就把话说清
  // （"停用"是个明确状态，不该用一个失败弹窗来表达）。
  const btnOpen = el('button', '', '打开');
  btnOpen.disabled = !active;
  btnOpen.title = active
    ? '进入独立界面（占满内容区，带返回栏）'
    : '插件已停用：先启用再打开';
  btnOpen.addEventListener('click', () => openPluginView(p));
  row.appendChild(btnOpen);

  // 领取类插件额外给一个"批量"提示：用户最关心"会不会一次打一堆请求"。
  if (p.kind !== 'health' && p.kind !== 'egress') {
    const hint = el('span', 'muted', '逐个串行');
    hint.style.fontSize = '11px';
    hint.title = '该插件对选中的号依次执行，每次只发一个上游请求';
    row.appendChild(hint);
  }
  card.appendChild(row);
  return card;
}

// pluginHealthPanel 健康检测面板（v0.31.31）。
//
// 语义与"领取"完全不同，所以是另一套渲染：
//   · 检测是**读**操作（不会改账号状态），所以没有"执行中逐行返回"；
//   · 结果是**异步**产出的（服务端逐号串行探活，几十秒到几分钟），
//     所以给的是"发起 → 轮询刷新"，而不是同步等。
//   · 用户视角**只读**：不提供停用/删除/复活按钮（那是管理员的权力）。
async function pluginHealthPanel(p, card, panel) {
  const st = (pluginState[p.id] = pluginState[p.id] || { healthOpen: true });
  panel.textContent = '';
  panel.appendChild(el('div', 'sub', '正在读取检测结果…'));

  const isAdmin = pluginScope() === 'admin';
  const base = isAdmin ? '/api/admin/accounts/health' : '/api/host/accounts/health';
  let d;
  try {
    d = await api(base + '/last');
  } catch (e) {
    panel.textContent = '';
    panel.appendChild(el('div', 'sub', '读取检测结果失败：' + (e && e.message ? e.message : e)));
    return;
  }

  // 两端返回形状不同（后台 results[]，用户端 accounts[]），在这里归一。
  // 统一成 {uid, nickname, tokenDead, checked, result}，后面只按这个渲染。
  let rows = [];
  if (Array.isArray(d.accounts)) {
    rows = d.accounts.map((a) => ({
      uid: a.uid, nickname: a.nickname, tokenDead: !!a.token_dead,
      checked: !!a.checked, result: a.result || null,
    }));
  } else if (Array.isArray(d.results)) {
    rows = d.results.map((r) => ({
      uid: r.uid, nickname: r.nickname, tokenDead: false, checked: true, result: r,
    }));
  }
  if (!st.picked) st.picked = new Set(rows.map((r) => r.uid));
  // 刷新后收敛勾选集合（只保留还存在的号）。
  const alive = new Set(rows.map((r) => r.uid));
  st.picked = new Set(Array.from(st.picked).filter((u) => alive.has(u)));

  panel.textContent = '';
  if (!rows.length) {
    panel.appendChild(el('div', 'sub', isAdmin
      ? '还没有可检测的账号。先到「账号池」导入账号。'
      : '你还没有代挂账号。先到「账号代挂」页添加一个，再回来体检。'));
    return;
  }

  const sum = d.summary || {};
  const head = el('div', 'sub');
  head.style.margin = '8px 0';
  head.appendChild(document.createTextNode(
    '检测内容：凭证是否还有效 · 余额能否读到 · 对话通道是否通 · 出口是否正常。' +
    '每号 1~3 秒、逐个串行，**只读**，不会停用或删除任何账号。'));
  if (isAdmin) {
    head.appendChild(document.createElement('br'));
    head.appendChild(document.createTextNode(
      '需要按策略处置（停用/复活/删除）请到左侧「健康检测」页 —— 那里才有写操作。'));
  }
  panel.appendChild(head);

  // 汇总（只算本次能看到的号）
  const kpi = el('div', 'sub');
  kpi.style.margin = '6px 0 10px';
  const stamp = sum.ended_at ? (' · 最近一次 ' + fmtClock(sum.ended_at)) : '';
  kpi.textContent = '共 ' + (sum.total || rows.length) + ' 个号 · 已检测 ' + (sum.checked || 0)
    + ' · 确定性异常 ' + (sum.definite || 0) + stamp;
  panel.appendChild(kpi);

  const row = el('div', 'row');
  row.style.cssText = 'flex-wrap:wrap;align-items:center;gap:8px;';
  const all = el('label', 'pick-all');
  const cbAll = document.createElement('input');
  cbAll.type = 'checkbox';
  cbAll.checked = st.picked.size === rows.length;
  cbAll.onchange = () => {
    st.picked = cbAll.checked ? new Set(rows.map((r) => r.uid)) : new Set();
    pluginHealthPanel(p, card, panel);
  };
  all.appendChild(cbAll);
  all.appendChild(document.createTextNode(' 全选'));
  row.appendChild(all);

  const cnt = el('span', 'muted', '已选 ' + st.picked.size + ' / ' + rows.length);
  cnt.style.fontSize = '12px';
  row.appendChild(cnt);

  const runBtn = el('button', null, '开始检测');
  runBtn.disabled = st.picked.size === 0;
  runBtn.onclick = async () => {
    const uids = Array.from(st.picked);
    runBtn.disabled = true;
    runBtn.textContent = '已发起…';
    try {
      const r = await post(base, { uids });
      toast('已开始检测 ' + (r.count || uids.length) + ' 个号（逐个串行，约几十秒）', 'ok');
      // 轮询刷新：检测是后台跑的，页面要自己把结果接回来。
      // 15 秒一次、最多 20 次（5 分钟）—— 超过就停下并提示手动刷新，
      // 不做无限轮询（用户切走标签页时那只会白烧请求）。
      let n = 0;
      const tick = async () => {
        n++;
        try {
          const nd = await api(base + '/last');
          const s2 = nd.summary || {};
          if ((s2.checked || 0) >= (r.count || uids.length) || n >= 20) {
            pluginHealthPanel(p, card, panel);
            return;
          }
        } catch (e) { /* 轮询失败不终止 */ }
        if (n < 20) setTimeout(tick, 15000);
        else pluginHealthPanel(p, card, panel);
      };
      setTimeout(tick, 8000);
    } catch (e) {
      toast('发起检测失败：' + (e && e.message ? e.message : e), 'err');
      runBtn.disabled = false;
      runBtn.textContent = '开始检测';
    }
  };
  row.appendChild(runBtn);

  const refresh = el('button', 'ghost', '刷新结果');
  refresh.onclick = () => pluginHealthPanel(p, card, panel);
  row.appendChild(refresh);
  panel.appendChild(row);

  // 逐号结果
  const list = el('div', 'cfg-list');
  list.style.marginTop = '10px';
  for (const r of rows) {
    const line = el('div');
    line.style.cssText = 'display:flex;gap:8px;align-items:flex-start;padding:6px 0;'
      + 'border-top:1px solid rgba(127,127,127,.15);';
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = st.picked.has(r.uid);
    cb.onchange = () => {
      if (cb.checked) st.picked.add(r.uid); else st.picked.delete(r.uid);
      pluginHealthPanel(p, card, panel);
    };
    line.appendChild(cb);

    const body = el('div');
    body.style.flex = '1';
    const title = el('div');
    title.appendChild(el('b', null, r.nickname || maskUID(r.uid) + ' '));
    const res = r.result;
    if (r.tokenDead) {
      const t = el('span', 'tag err', '登录失效');
      t.title = 'refresh token 已被上游作废（12153）：只能重新登录导入，等下去不会好';
      title.appendChild(t);
      title.appendChild(document.createTextNode(' '));
    }
    if (!r.checked) {
      title.appendChild(el('span', 'tag', '未检测'));
    } else {
      const okState = res && (res.state === 'ok' || res.state === 'healthy');
      title.appendChild(el('span', 'tag ' + (okState ? 'ok' : 'warn'), (res && res.state_label) || (res && res.state) || '—'));
      if (res && res.chat_probed) {
        title.appendChild(document.createTextNode(' '));
        const ct = el('span', 'tag ' + (res.chat_ok ? 'ok' : 'err'),
          '对话' + (res.chat_ok ? '通' : '不通'));
        if (res.chat_note) ct.title = res.chat_note;
        title.appendChild(ct);
      }
    }
    body.appendChild(title);

    if (res) {
      const detail = el('div', 'muted');
      detail.style.fontSize = '11px';
      const bits = [];
      if (res.reason) bits.push(res.reason);
      if (res.credits_known) bits.push('余额 ' + res.credits);
      if (res.egress) bits.push('出口 ' + res.egress);
      if (res.latency_ms) bits.push(res.latency_ms + 'ms');
      detail.textContent = bits.join(' · ');
      if (bits.length) body.appendChild(detail);
    }
    line.appendChild(body);
    list.appendChild(line);
  }
  panel.appendChild(list);
}

// pluginFillPanel 填面板主体：按插件形态分流。
//
// `kind` 由**服务端**下发（pluginDef.Kind），前端不按 id 猜 ——
// 否则加第二个同类插件时又得回来改前端，插件层就白抽了。
async function pluginFillPanel(p, card, panel) {
  if (p.kind === 'health') {
    return pluginHealthPanel(p, card, panel);
  }
  // 状态兜底（v0.31.35）：`pluginCard` 会初始化它，但 `openPluginView` 也可能
  // **不经过卡片**直接进面板（深链 / 将来的"最近使用"入口）。少这行就是
  // `Cannot read properties of undefined`，整个面板空白且不报错给用户。
  const st = (pluginState[p.id] = pluginState[p.id] || {
    targets: null, picked: null, running: false, rows: {},
  });
  panel.textContent = '';
  panel.appendChild(el('div', 'sub', '正在读取可操作的账号…'));

  let targets = st.targets;
  if (!targets) {
    try {
      // dry_run 复用服务端的目标解析：**用户端会自动收窄到自己的号**，
      // 前端因此不必关心"我的号从哪来"（admin 看全部、user 看自己的）。
      const d = await api(pluginBase() + '/' + encodeURIComponent(p.id) + '/claim',
        { method: 'POST', body: JSON.stringify({ all: true, dry_run: true }) });
      targets = (d && d.targets) || [];
      st.targets = targets;
    } catch (e) {
      panel.textContent = '';
      panel.appendChild(el('div', 'sub', '读取账号失败：' + (e && e.message ? e.message : e)));
      return;
    }
  }
  if (!st.picked) st.picked = new Set(targets.map((t) => t.uid));

  panel.textContent = '';
  if (!targets.length) {
    panel.appendChild(el('div', 'sub', '没有可操作的账号。'));
    return;
  }

  const head = el('div', 'sub');
  head.style.margin = '10px 0 8px';
  head.appendChild(document.createTextNode('出站纪律：'));
  const strong = el('b', null, '逐个串行');
  head.appendChild(strong);
  head.appendChild(document.createTextNode(
    '执行（每次只发一个账号，间隔 ' + PLUGIN_GAP_MS + 'ms，服务端另有 30 秒/账号 的重复保护）。'
    + '上游对「没有可领」的答复属正常态，不等于失败。'));
  panel.appendChild(head);

  const row = el('div', 'row');
  row.style.flexWrap = 'wrap';
  row.style.alignItems = 'center';
  row.style.gap = '8px';

  const all = el('label', 'pick-all');
  const cbAll = document.createElement('input');
  cbAll.type = 'checkbox';
  cbAll.checked = st.picked.size === targets.length;
  cbAll.addEventListener('change', () => {
    st.picked = cbAll.checked ? new Set(targets.map((t) => t.uid)) : new Set();
    pluginFillPanel(p, card, panel);
  });
  all.appendChild(cbAll);
  all.appendChild(document.createTextNode(' 全选'));
  row.appendChild(all);

  const cnt = el('span', 'muted', '已选 ' + st.picked.size + ' / ' + targets.length);
  cnt.style.fontSize = '12px';
  row.appendChild(cnt);

  const run = el('button', null, st.running ? '执行中…' : '开始领取');
  run.disabled = st.running || st.picked.size === 0;
  run.addEventListener('click', () => pluginRun(p, card, panel));
  row.appendChild(run);

  const refresh = el('button', 'ghost', '重新读取账号');
  refresh.disabled = st.running;
  refresh.addEventListener('click', () => {
    st.targets = null; st.picked = null; st.rows = {};
    pluginFillPanel(p, card, panel);
  });
  row.appendChild(refresh);
  panel.appendChild(row);

  const listBox = el('div');
  listBox.style.marginTop = '10px';
  for (const t of targets) {
    const line = el('label');
    line.style.display = 'flex';
    line.style.alignItems = 'center';
    line.style.gap = '8px';
    line.style.padding = '4px 0';
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = st.picked.has(t.uid);
    cb.disabled = st.running;
    cb.addEventListener('change', () => {
      if (cb.checked) st.picked.add(t.uid); else st.picked.delete(t.uid);
      pluginFillPanel(p, card, panel);
    });
    line.appendChild(cb);
    line.appendChild(el('span', null, t.nickname || t.uid));
    const r = st.rows[t.uid];
    if (r) {
      // 上游原文挂在 title 上（悬停可见）：这是区分「真的无可领」与「字段名猜错」
      // 的唯一事实，但直接铺在行里会把布局撑坏。
      // 上游答复（code + msg）与返回原文都挂在 title 上：悬停可见、不撑坏布局。
      // v0.31.19 起 "已领取过" 有独立状态，但**证据仍然只有上游原文**——
      // 把 code 一起摆出来，用户就不必来问"到底是已领过还是没活动"。
      const tips = [];
      if (r.upstream_msg) tips.push('上游答复：' + r.upstream_msg + (r.code ? '（code=' + r.code + '）' : ''));
      if (r.detail) tips.push('上游返回原文：' + r.detail);
      if (tips.length) line.title = tips.join('\n');
      line.appendChild(pluginResultBadge(r));
    }
    listBox.appendChild(line);
  }
  panel.appendChild(listBox);
}

// pluginResultBadge 一行结果徽标（颜色 + 文字，逐号可见）。
function pluginResultBadge(r) {
  const map = {
    ok: ['已到账 +' + (r.credit || 0), 'var(--ok, #2e7d32)'],
    // already（v0.31.19）：与 none 分开 —— "已经领过"是**结论**（不用再试），
    // "无可领"是**现状**（等有活动再试）。上游原文见行 title。
    already: ['已领取过', '#888'],
    none: ['无可领', '#888'],
    skip: ['已跳过' + (r.message ? '（' + r.message + '）' : ''), '#888'],
    error: ['失败：' + (r.message || '未知错误'), 'var(--bad, #c62828)'],
  };
  const [text, color] = map[r.status] || [r.status, '#888'];
  const b = el('span', null, text);
  b.style.fontSize = '12px';
  b.style.color = color;
  b.style.marginLeft = 'auto';
  return b;
}

// pluginRun **串行**执行：一个号一个请求，号与号之间留间隔；逐号刷新结果。
async function pluginRun(p, card, panel) {
  const st = (pluginState[p.id] = pluginState[p.id] || { rows: {} });
  if (st.running) return;
  const uids = Array.from(st.picked);
  if (!uids.length) return;

  st.running = true;
  st.rows = {};
  pluginFillPanel(p, card, panel);

  let ok = 0, already = 0, none = 0, err = 0, credit = 0;
  for (let i = 0; i < uids.length; i++) {
    const uid = uids[i];
    try {
      const d = await api(pluginBase() + '/' + encodeURIComponent(p.id) + '/claim',
        { method: 'POST', body: JSON.stringify({ uids: [uid] }) });
      st.rows[uid] = d || { status: 'error', message: '空响应' };
    } catch (e) {
      st.rows[uid] = { status: 'error', message: (e && e.message ? e.message : String(e)) };
    }
    const r = st.rows[uid];
    if (r.status === 'ok') { ok++; credit += (r.credit || 0); }
    else if (r.status === 'already') already++;
    else if (r.status === 'none' || r.status === 'skip') none++;
    else err++;
    // 每完成一个就刷新这一行（而不是等全部跑完）——长任务里"看着它走"比结果重要。
    pluginFillPanel(p, card, panel);
    if (i < uids.length - 1) await new Promise((res) => setTimeout(res, PLUGIN_GAP_MS));
  }

  st.running = false;
  pluginFillPanel(p, card, panel);
  const msg = '完成：' + ok + ' 个到账（共 +' + credit + ' 积分），'
    + (already ? already + ' 个已领取过，' : '')
    + none + ' 个无可领，' + err + ' 个失败';
  toast(msg, err ? 'bad' : 'ok');
  // 汇总也写进面板（toast 会消失，汇总要留在页面上）。
  const sum = el('div', 'sub', msg);
  sum.style.marginTop = '8px';
  panel.appendChild(sum);
}


// ---------------------------------------------------------------- ⑩ 本地出口代理（v0.31.17）
//
// 控制台「代理」页：把「本机的网络出口」接进平台的出站链路。
//
// 为什么把它放控制台而不是只有管理后台：出口是**用户自己能感知**的东西
// （"我的号从哪出去"），把它藏起来只会让人去猜。
//
// v0.31.24：**执行权也下沉到用户**（此前只有管理员能做，普通用户点「一键拉起」
// 只会拿到 403）。服务端把"能碰谁"编码成 `manage_scope`：
//   · platform（管理员 / 运维）→ 全部机器；应用时设全局默认出口 + 绑全部自挂号；
//   · self（普通用户）→ 只有自己的机器；应用时只绑自己名下的号，
//     **不设全局默认出口**。
// 本页的所有文案与概览行都跟着它切 —— 对一个没有"全局默认出口"这个开关的人
// 讲它，只会让他以为页面坏了。
//
// 三步闭环（页面上一句话讲清，用户不必理解隧道）：
//   ① 本机跑起 agent（一次，之后常驻）→ ② 点「一键拉起」→ ③ 就绪后自动应用配置
const EGRESS_POLL_MS = 2000;   // 轮询状态间隔
const EGRESS_WAIT_MAX_MS = 90000; // 一键拉起后的最长等待
// EGRESS_INSTALL_WAIT_MS 下载安装包后等 agent 上线的最长时间（v0.31.25）。
// 5 分钟：够"下载 → 双击 → 装完 → 起进程"，又不至于让页面一直挂着。
const EGRESS_INSTALL_WAIT_MS = 300000;
let egressTimer = null;
let egressSince = 0;
// egressWatchTimer 「等 agent 上线」的轮询（与 egressTimer 分开：那个是"等拉起完成"）。
let egressWatchTimer = null;
let egressWatchSince = 0;

// egressErrHint 把失败原因翻译成"下一步该干什么"（v0.31.25）。
//
// 为什么值得单做一个映射：出口链路的失败几乎都不是用户能直接看懂的
// （`remote port forwarding failed` / `publickey` / HTTP 409 业务错）。
// 只把原文抛给用户，他唯一能做的就是来问；而这几条恰好都有明确的下一步。
// **原文一律保留**（拼接而不是替换）—— 映射错了也有据可查。
function egressErrHint(msg) {
  const m = String(msg || '');
  if (!m) return '';
  if (/invalid agent token|令牌/.test(m)) return '令牌无效或已作废：请在本页重新下载安装包（会重新签发）';
  if (/端口已用完|隧道端口已用完/.test(m)) return '本账号的机器数已达上限（10 台）：先停用不再使用的机器';
  if (/隧道尚未就绪|连不上本机代理端口/.test(m)) return 'agent 还没连上云端：请确认那台电脑上的窗口最后显示 Done / READY';
  if (/找不到代理程序/.test(m)) return '缺少 wbproxy.exe：请重新下载安装包（两个文件都要）';
  if (/Permission denied|publickey|密钥/.test(m)) return '密钥被服务器拒绝：请重新下载安装包（会重新签发一把）';
  if (/remote port forwarding failed/.test(m)) return '隧道端口被占用：等几秒重试；仍失败请换一台机器或联系管理员';
  if (/正在应用中|busy/.test(m)) return '上一次应用还在进行：等几秒再点';
  if (/尚未登记|不属于你/.test(m)) return '这台机器已不在列表中：请刷新页面重新选择';
  if (/下载失败|download failed/.test(m)) return '下载失败：检查目标电脑的网络能否访问服务地址';
  return '';
}

// egressMsg 组合"原文 + 下一步"（原文在前，便于报障时直接复制）。
function egressMsg(msg) {
  const h = egressErrHint(msg);
  return h ? (msg + '\n→ ' + h) : msg;
}
// egressSelMachine 当前选中的**目标机器**（v0.31.18 多机）。
// 指令按机器定向下发；这个变量就是“发给哪一台”的答案。
let egressSelMachine = '';

function egressDot(ok, text) {
  const s = el('span', null, (ok ? '● ' : '○ ') + text);
  s.style.fontSize = '12px';
  s.style.color = ok ? 'var(--ok, #2e7d32)' : '#888';
  s.style.marginRight = '12px';
  return s;
}

async function loadEgressHome() {
  const box = document.getElementById('egressHomeBox');
  if (!box) return;
  // 两张卡一起刷：出口状态（机器/隧道）+ 我的账号出口（逐号线路）。
  // 它们回答的是两个不同问题："隧道通不通" 与 "每个号到底从哪个 IP 出去"。
  loadHostEgressLines();
  try {
    const v = await api('/api/egress/home');
    renderEgressHome(box, v);
  } catch (e) {
    box.textContent = '';
    box.appendChild(el('div', 'sub', '读取出口状态失败：' + (e && e.message ? e.message : e)));
  }
}

// ======================================================================
// 重新登录 / 导入账号 JSON（v0.31.32）
//
// 背景：token 被判失效（12153）之后**只能重新登录**。用户端此前只有「扫码添加」
// 一条路，而且失败后卡片上只有一个"去找管理员"的三步提示 —— 失效号一多，
// 恢复就被卡在人工环节。这里补齐两条自助路径：
//   ① 扫码（复用现有 oauth 弹窗，登录成功即覆盖该号凭证）；
//   ② 导入 auth JSON（与「导出 JSON」严格闭环，粘回去即可）。
// ======================================================================

// hostImportJSONDialog 打开"粘贴/选择 auth JSON"弹窗。
//
// opts.onDone(count) 在导入成功后调用（用于刷新列表）。
function hostImportJSONDialog(opts) {
  opts = opts || {};
  const m = openModal(opts.title || '导入账号 JSON',
    '粘上 WorkBuddy 导出的 auth 文件内容（或直接选文件）。**同一个号**再导一次 = 重新登录，'
    + '会覆盖旧凭证并清掉「登录失效」标记；新号则直接加到你名下。');
  const body = m.body;

  const ta = el('textarea');
  ta.placeholder = '[{"uid":"…","access_token":"…","refresh_token":"…"}] 或单个对象 {…}';
  ta.rows = 8;
  ta.style.cssText = 'width:100%;font-family:ui-monospace,Consolas,monospace;font-size:12px;';
  body.appendChild(ta);

  const row = el('div', 'row');
  row.style.cssText = 'flex-wrap:wrap;align-items:center;gap:8px;margin-top:10px;';

  // 文件选择：读文本塞进 textarea —— 不直接把文件传给后端，用户能先看到内容再决定。
  const file = document.createElement('input');
  file.type = 'file';
  file.accept = '.json,application/json,text/plain';
  file.style.display = 'none';
  file.onchange = () => {
    const f = file.files && file.files[0];
    if (!f) return;
    const rd = new FileReader();
    rd.onload = () => { ta.value = String(rd.result || ''); };
    rd.onerror = () => toast('读取文件失败', 'err');
    rd.readAsText(f, 'utf-8');
  };
  body.appendChild(file);
  const pick = el('button', 'ghost', '选择文件…');
  pick.onclick = () => file.click();
  row.appendChild(pick);

  const run = el('button', '', '导入');
  run.onclick = async () => {
    const txt = String(ta.value || '').trim();
    if (!txt) { toast('先粘贴内容或选择文件', 'err'); return; }
    if (!(await confirmAsk('导入这段 JSON？\n\n· 同一个号会覆盖旧凭证（= 重新登录）；\n'
      + '· 新号直接加到你名下（未申请入池）；\n· 不属于你的号会被跳过并说明原因。'))) return;
    run.disabled = true;
    try {
      const d = await post('/api/host/accounts/import', { json: txt });
      toast(d.note || ('已导入 ' + d.imported + ' 个、更新 ' + d.updated + ' 个'), 'ok');
      for (const s of (d.skips || []).slice(0, 6)) {
        toast((s.uid ? maskUID(s.uid) + '：' : '') + s.reason, 'warn');
      }
      m.close();
      if (typeof opts.onDone === 'function') opts.onDone(d);
    } catch (e) {
      toast('导入失败：' + (e && e.message ? e.message : e), 'err');
      run.disabled = false;
    }
  };
  row.appendChild(run);

  const cancel = el('button', 'ghost', '取消');
  cancel.onclick = m.close;
  row.appendChild(cancel);
  body.appendChild(row);
}

// hostReloginDialog 重新登录某个代挂号：扫码 / 导入 JSON 两条路并列。
//
// 只对**自己的号**有意义（服务端也只允许改自己的号），所以这里不做权限判断 ——
// 按钮只出现在自己名下的卡片上。
function hostReloginDialog(acc) {
  const name = (acc && (acc.nickname || maskUID(acc.uid))) || '账号';
  const m = openModal('重新登录 · ' + name,
    '两种方式选一种即可。登录成功后这个号的凭证会被替换、红色「登录失效」标记自动消失，'
    + '原有的分组 / 出口 / 入池状态**不会被改动**。');
  const body = m.body;

  // ① 扫码
  const t1 = el('div', 'sub');
  t1.style.margin = '4px 0 8px';
  t1.appendChild(el('b', null, '① 扫码登录（推荐）'));
  t1.appendChild(document.createTextNode('：点下面按钮，在打开的页面里登录这个账号，本页会自动采集。'));
  body.appendChild(t1);
  body.appendChild(oauthScanButtons({
    startUrl: '/api/host/oauth/start',
    pollUrl: '/api/host/oauth/poll',
    title: '扫码重新登录 · ' + name,
    subtitle: '选版本后点「开始扫码登录」；在浏览器里完成登录，本页会自动把凭证写回这个账号。',
    onDone: (account) => {
      const got = (account && account.uid) || '';
      if (got && acc && acc.uid && got !== acc.uid) {
        // 扫的号与要修的不是同一个：如实说，不要假装成功。
        toast('登录的是另一个账号（' + (account.nickname || got) + '），已按其 uid 入库；'
          + '要修「' + name + '」请扫它自己的账号', 'warn');
      } else {
        toast('已重新登录：' + name, 'ok');
      }
      m.close();
      if (typeof loadHostAccounts === 'function') loadHostAccounts();
    },
  }));

  // ② 导入 JSON
  const t2 = el('div', 'sub');
  t2.style.margin = '16px 0 8px';
  t2.appendChild(el('b', null, '② 导入 auth JSON'));
  t2.appendChild(document.createTextNode(
    '：如果你有桌面端导出的 auth 文件（或之前在本站「导出 JSON」存下的），直接粘进来即可。'));
  body.appendChild(t2);

  const imp = el('button', 'ghost', '打开导入窗口');
  imp.onclick = () => {
    m.close();
    hostImportJSONDialog({
      title: '导入 auth JSON · ' + name,
      onDone: () => { if (typeof loadHostAccounts === 'function') loadHostAccounts(); },
    });
  };
  body.appendChild(imp);

  const tip = el('div', 'sub');
  tip.style.marginTop = '14px';
  tip.style.fontSize = '12px';
  tip.textContent = '提示：扫码与导入都必须用**这个号自己**的登录信息。'
    + '导入别人的号会被拒绝（归属仍是原号主），也不会把别人的号并到你名下。';
  body.appendChild(tip);
}

// ======================================================================
// 我的账号出口（v0.31.31）
//
// 用户端的"逐号指定出口"。在它之前，用户只有两个全量动作：
//   · 设置页「我的出口」填一个 URL → 应用到全部；
//   · 代理页「一键拉起」→ 应用 = 把我名下所有号绑到刚拉起的那条线路。
// 两台电脑接进来时，后者的效果是"在 B 机上点一下，A 机上的号全搬过来"，
// 于是两台机器最后只有一台在承重 —— 用户看到的就是"两个 IP 却是同一个出口"。
// ======================================================================

let heLines = [];       // 可用线路
let heAccounts = [];    // 我的号 + 当前出口
let hePicked = new Set();

async function loadHostEgressLines() {
  const box = document.getElementById('egrAccBox');
  if (!box) return;
  try {
    const d = await api('/api/host/egress/lines');
    heLines = d.lines || [];
    heAccounts = d.accounts || [];
    // 勾选集合随刷新收敛：只保留仍然存在的号，避免"删了号还勾着"。
    const alive = new Set(heAccounts.map((a) => a.uid));
    hePicked = new Set(Array.from(hePicked).filter((u) => alive.has(u)));
    renderHostEgress(d);
  } catch (e) {
    box.textContent = '';
    box.appendChild(el('div', 'sub', '读取账号出口失败：' + (e && e.message ? e.message : e)));
  }
}

// heLineOptions 可选线路（跳过"不可选"的：停用的、别人的）。
function heUsableLines() {
  return heLines.filter((l) => !l.note);
}

function renderHostEgress(d) {
  const box = document.getElementById('egrAccBox');
  if (!box) return;
  box.textContent = '';

  const usable = heUsableLines();
  const hint = document.getElementById('egrAccHint');
  if (hint) {
    // 口径（v0.31.34）：这里只统计**我能用的**线路（平台 + 我的）。
    // 写「共 N 条」会让人以为全平台只有 N 条，而别人的线路对他是不可见的 ——
    // 一个总数既不能用来判断"我够不够"，还会引出"为什么我看不到别的"这类疑问。
    hint.textContent = heAccounts.length
      ? ('可用 ' + usable.length + ' 条线路（平台 + 我的）· 实际 '
        + (d.distinct_exit_ips || 0) + ' 个不同出口 IP')
      : '';
  }

  if (!heAccounts.length) {
    box.appendChild(el('div', 'hintbar', '还没有代挂账号。先到「账号代挂」页添加一个，再回来配出口。'));
    return;
  }

  // ---- 顶部：口径说明 + 批量动作 ----
  const bar = el('div', 'row');
  bar.style.cssText = 'flex-wrap:wrap;align-items:center;gap:8px;margin-bottom:10px;';

  const rebal = el('button', null, '按出口 IP 均衡');
  rebal.title = '把（勾选的）号轮流分到不同出口 IP 的线路上；同一 IP 的多条线路只算一个出口';
  rebal.onclick = async () => {
    const uids = Array.from(hePicked);
    const scope = uids.length ? ('勾选的 ' + uids.length + ' 个号') : '全部代挂号';
    if (!(await confirmAsk('把 ' + scope + '按出口 IP 均衡分配？\n\n'
      + '· 我会按「不同出口 IP」分组，把号轮流分上去；\n'
      + '· 同一个 IP 上的多条线路只算一个出口（不会假装更分散）；\n'
      + '· 只会动你名下的号。'))) return;
    rebal.disabled = true;
    try {
      const r = await post('/api/host/egress/rebalance', uids.length ? { uids } : {});
      const desc = (r.exits || []).map((x) => (x.exit_ip || '未知') + ' × ' + x.count).join('、');
      toast('已分配 ' + r.assigned + ' 个号到 ' + r.distinct_exit_ips + ' 个出口：' + desc, 'ok');
      for (const w of (r.warnings || [])) toast(w, 'warn');
      loadHostEgressLines();
    } catch (e) { toast('均衡失败：' + e.message, 'err'); }
    finally { rebal.disabled = false; }
  };
  bar.appendChild(rebal);

  // 批量指定：把勾选的号统一指到某条线路（或直连 / 跟随默认）。
  const sel = el('select');
  sel.style.minWidth = '240px';
  const opt = (v, t) => { const o = el('option', null, t); o.value = v; sel.appendChild(o); };
  opt('', '— 批量设为… —');
  opt('__direct', '直连（不走任何代理）');
  opt('__clear', '跟随默认（清空本号出口配置）');
  for (const l of usable) {
    const who = l.mine ? '［我的］' : '［平台］';
    opt(l.url, who + '线路：' + (l.name || l.url) + (l.exit_ip ? '（' + l.exit_ip + '）' : '（出口未知）'));
  }
  bar.appendChild(sel);
  const apply = el('button', 'ghost', '应用到勾选');
  apply.onclick = async () => {
    const v = sel.value;
    if (!v) { toast('先选一个目标', 'err'); return; }
    const uids = Array.from(hePicked);
    if (!uids.length) { toast('先勾选账号', 'err'); return; }
    const proxy = v === '__direct' ? 'direct' : (v === '__clear' ? '' : v);
    apply.disabled = true;
    try {
      const r = await post('/api/host/egress/assign', { uids, proxy });
      toast('已设置 ' + r.updated + ' 个号' + (r.exit_ip ? '，该线路出口 ' + r.exit_ip : ''), 'ok');
      if (r.note) toast(r.note, 'warn');
      loadHostEgressLines();
    } catch (e) { toast('设置失败：' + e.message, 'err'); }
    finally { apply.disabled = false; }
  };
  bar.appendChild(apply);

  const cnt = el('span', 'muted', '已选 ' + hePicked.size + ' / ' + heAccounts.length);
  cnt.style.fontSize = '12px';
  bar.appendChild(cnt);
  box.appendChild(bar);

  // ---- 线路一览（每条线路一个出口 IP；同 IP 的会合并提示）----
  const byIP = {};
  for (const l of usable) (byIP[l.exit_ip || '未知'] = byIP[l.exit_ip || '未知'] || []).push(l);
  const lineRow = el('div', 'sub');
  lineRow.style.margin = '0 0 10px';
  for (const ip of Object.keys(byIP)) {
    const arr = byIP[ip];
    const txt = el('div');
    txt.style.padding = '2px 0';
    txt.appendChild(el('b', null, ip + '  '));
    // 归属只有两种（v0.31.34）：平台线路 / 我自己接的。
    // 他人的线路后端已不再下发，这里也就不需要"借的是谁的机器"那套标注。
    txt.appendChild(el('span', null, arr.map((l) => {
      const who = l.mine ? '我的・' : '平台・';
      return who + (l.name || l.url) + '（' + l.url + '）';
    }).join('、')));
    if (arr.length > 1) {
      txt.appendChild(el('span', null, '  ⚠ 这几条线路共用同一个出口，只会算 1 个 IP'));
      txt.lastChild.style.color = '#b9770e';
    }
    if (!arr[0].last_ok) {
      txt.appendChild(el('span', null, '  · 最近探活未成功'));
      txt.lastChild.style.color = '#b9770e';
    }
    lineRow.appendChild(txt);
  }
  if (!Object.keys(byIP).length) {
    lineRow.appendChild(el('div', 'sub', '还没有可用线路：先在上面「一键拉起」，或等管理员加线路。'));
  }
  // 诊断结论（v0.31.31）：把"几条线路 / 几个 IP / 号怎么分布的"一句话说清。
  // 用户问的"两台电脑两个 IP 却同一个出口"，答案就在这里。
  if (usable.length) {
    const distinct = d.distinct_exit_ips || 0;
    const diag = el('div', 'sub');
    diag.style.margin = '0 0 10px';
    const perIP = {};
    for (const a of heAccounts) {
      const key = a.exit_ip || '未知';
      perIP[key] = (perIP[key] || 0) + 1;
    }
    const dist = Object.keys(perIP).map((k) => k + ' × ' + perIP[k] + ' 个号').join('、');
    diag.textContent = '账号分布：' + (dist || '（没有号）');
    if (usable.length > 1 && distinct < usable.length) {
      const w = el('div', null, '⚠ 你有 ' + usable.length + ' 条可用线路，但**只有 ' + distinct
        + ' 个不同出口 IP**：同 IP 的线路再多也只算一个出口。'
        + '要真正多出 IP，需要另一台电脑（另一条宽带）接入，或换一条不同出口的线路。');
      w.style.cssText = 'margin-top:6px;color:#b9770e;font-size:12px;';
      diag.appendChild(w);
    } else if (usable.length && !usable.some((l) => l.mine)) {
      const w = el('div', null, '你现在只用了平台线路。想要自己的住宅 IP，点上面的'
        + '「① 下载本机代理安装包」在任意一台电脑上运行即可 —— 那条线路只承载你自己的号。');
      w.style.cssText = 'margin-top:6px;color:#888;font-size:12px;';
      diag.appendChild(w);
    } else if (distinct === 1 && usable.length === 1) {
      const w = el('div', null, '当前只有 1 条线路：所有号都从这一个 IP 出去。再接一台电脑（另一条宽带）就能分散。');
      w.style.cssText = 'margin-top:6px;color:#888;font-size:12px;';
      diag.appendChild(w);
    }
    lineRow.appendChild(diag);
  }
  box.appendChild(lineRow);

  // ---- 逐号表格 ----
  const tbl = el('div', 'cfg-list');
  const head = el('div', 'gr-sub');
  head.style.cssText = 'display:flex;gap:8px;align-items:center;padding:6px 0;font-size:12px;color:#888;';
  const cbAll = document.createElement('input');
  cbAll.type = 'checkbox';
  cbAll.checked = hePicked.size === heAccounts.length && heAccounts.length > 0;
  cbAll.onchange = () => {
    hePicked = cbAll.checked ? new Set(heAccounts.map((a) => a.uid)) : new Set();
    renderHostEgress(d);
  };
  head.appendChild(cbAll);
  head.appendChild(el('span', null, '账号'));
  head.appendChild(el('span', null, '当前出口'));
  tbl.appendChild(head);

  for (const a of heAccounts) {
    const row = el('div');
    row.style.cssText = 'display:flex;gap:8px;align-items:center;padding:6px 0;border-top:1px solid rgba(127,127,127,.15);';
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = hePicked.has(a.uid);
    cb.onchange = () => {
      if (cb.checked) hePicked.add(a.uid); else hePicked.delete(a.uid);
      const c = document.getElementById('egrAccBox');
      if (c) renderHostEgress(d);
    };
    row.appendChild(cb);

    const who = el('div');
    who.style.cssText = 'min-width:140px;flex:0 0 160px;';
    who.appendChild(el('div', null, a.nickname || maskUID(a.uid)));
    const sub = el('div', 'muted', a.configured ? ('已指定：' + (a.configured === 'direct' ? '直连' : a.configured)) : '未指定（跟随默认）');
    sub.style.fontSize = '11px';
    who.appendChild(sub);
    row.appendChild(who);

    const exitTxt = el('div');
    exitTxt.style.cssText = 'flex:1;min-width:160px;';
    const src = a.exit_source === 'direct'
      ? '平台本机 IP 直连'
      : (a.resolved_name || a.resolved || '（未知）');
    exitTxt.appendChild(el('div', null, src));
    const ipLine = el('div', 'muted', a.exit_ip ? ('出口 IP ' + a.exit_ip) : '出口 IP 未知（该线路还没探活）');
    ipLine.style.fontSize = '11px';
    if (!a.exit_ip) ipLine.style.color = '#b9770e';
    exitTxt.appendChild(ipLine);
    // 越界绑定（v0.31.34）：这个号当前走的是别人的线路。
    // 规则收紧后用户选不到那条线路，所以他要靠这行字才知道"我现在在借谁的机器"。
    if (a.foreign_bound) {
      const fb = el('div', null, '⚠ 当前走的是「' + (a.foreign_owner_name || '其他用户')
        + '」的线路：你的号经过对方的机器，对方一关机这个号就不可用。'
        + '建议用右侧下拉改成自己的或平台线路。');
      fb.style.fontSize = '11px';
      fb.style.color = '#c0392b';
      exitTxt.appendChild(fb);
    }
    if (a.token_dead) {
      const dead = el('div', null, '⚠ 登录已失效，改出口无效 —— 请先重新登录');
      dead.style.fontSize = '11px';
      dead.style.color = '#c0392b';
      exitTxt.appendChild(dead);
    }
    row.appendChild(exitTxt);

    // 逐号改：选线路即生效（不必再点保存）。
    const one = el('select');
    one.style.minWidth = '180px';
    const o0 = el('option', null, '— 改到… —'); o0.value = ''; one.appendChild(o0);
    const od = el('option', null, '直连'); od.value = 'direct'; one.appendChild(od);
    const oc = el('option', null, '跟随默认'); oc.value = '__clear'; one.appendChild(oc);
    for (const l of usable) {
      const who = l.mine ? '［我的］' : '［平台］';
      const o = el('option', null, who + (l.name || l.url) + (l.exit_ip ? '（' + l.exit_ip + '）' : ''));
      o.value = l.url;
      one.appendChild(o);
    }
    one.onchange = async () => {
      const v = one.value;
      if (!v) return;
      const proxy = v === '__clear' ? '' : v;
      one.disabled = true;
      try {
        const r = await post('/api/host/egress/assign', { uids: [a.uid], proxy });
        toast('已更新' + (r.exit_ip ? '，出口 ' + r.exit_ip : ''), 'ok');
        loadHostEgressLines();
      } catch (e) { toast('设置失败：' + e.message, 'err'); one.disabled = false; }
    };
    row.appendChild(one);

    tbl.appendChild(row);
  }
  box.appendChild(tbl);
}

// egressMachineLabel 一台机器的显示名（hostname 优先；没有就用 id）。
function egressMachineLabel(m) {
  const h = String((m && m.hostname) || '').trim();
  if (h) return h;
  return m && m.is_default ? '（未报主机名的 agent）' : String((m && m.id) || '?');
}

// egressLabels 机器 id → 显示名（v0.31.24）。
//
// 为什么需要：页面上的机器 id 现在是**机器表的键**（用户视角形如 `u12:abc123`，
// 因为它带了 owner 命名空间）。把这种串直接写进 toast 对用户毫无意义 ——
// 用户在页面上看到的是主机名，提示里就该是同一个词。
const egressLabels = {};

// egressCurMachine 从状态里取当前选中的机器（并保证选中项有效）。
function egressCurMachine(v) {
  const machines = (v && v.machines) || [];
  if (!egressSelMachine || !machines.some((m) => m.id === egressSelMachine)) {
    egressSelMachine = (v && v.active_machine) || (machines.length ? machines[0].id : '');
  }
  return { machines, cur: machines.find((m) => m.id === egressSelMachine) || null };
}

function renderEgressHome(box, v) {
  box.textContent = '';
  const { machines, cur } = egressCurMachine(v);
  // manage_scope（v0.31.24）：平台视角 = 管理员/运维（能设全局默认出口、看全部机器）；
  // 用户视角 = 普通用户（只有自己的机器，只绑自己名下的号）。**文案必须跟着切**：
  // 对一个没有"全局默认出口"这个开关的人讲它，只会让他以为页面坏了。
  const isPlatform = (v.manage_scope || 'platform') === 'platform';
  for (const m of machines) egressLabels[m.id] = egressMachineLabel(m);

  const card = el('div', 'card');
  const head = el('div', 'section-head');
  head.appendChild(el('h2', null, '出口状态'));
  card.appendChild(head);

  // ---- 目标机器选择（v0.31.18 多机）----
  // 为什么要有这一档：指令是**按机器定向**下发的。没有选择器时，
  // 用户在 A 机页面上点「拉起」可能发给了 B 机（"谁最近说过话就命令谁"），
  // 而页面上完全看不出这件事 —— 于是表现为"点了没反应"。
  if (machines.length) {
    const selRow = el('div', 'row');
    selRow.style.cssText = 'margin:4px 0 10px;gap:8px;align-items:center;flex-wrap:wrap;';
    selRow.appendChild(el('span', 'muted', '目标机器'));
    const sel = el('select');
    sel.id = 'egressMachineSel';
    sel.style.minWidth = '260px';
    for (const m of machines) {
      const o = el('option', null, egressMachineLabel(m) + (m.online ? ' · 在线' : ' · 离线')
        + ' · 隧道:' + m.tunnel_port + (m.id === v.active_machine ? ' · 最近上报' : ''));
      o.value = m.id;
      sel.appendChild(o);
    }
    sel.value = egressSelMachine;
    sel.onchange = () => { egressSelMachine = sel.value; renderEgressHome(box, v); };
    selRow.appendChild(sel);
    card.appendChild(selRow);
  }

  // ---- 状态行（针对**选中的那台**）----
  const online = cur ? cur.online : v.agent_online;
  const tunnel = cur ? cur.tunnel_reachable : v.tunnel_reachable;
  const applied = cur ? cur.applied : v.applied;
  const ready = cur ? cur.ready : v.ready;
  const ports = cur ? { proxy_port: cur.proxy_port, tunnel_port: cur.tunnel_port } : (v.ports || {});
  const bound = cur ? cur.host_bound : v.host_bound;
  const total = cur ? cur.host_total : v.host_total;
  const exitIP = (cur && cur.exit_ip) || (v.agent_state && v.agent_state.exit_ip) || '（未知 —— agent 尚未上报）';
  const curMsg = (cur && cur.message) || (v.agent_state && v.agent_state.message) || '';

  const status = el('div');
  status.style.margin = '6px 0 10px';
  status.appendChild(egressDot(online, 'agent ' + (online ? '在线' : '离线')));
  status.appendChild(egressDot(tunnel, '反向隧道 ' + (tunnel ? '已连通' : '未连通')));
  status.appendChild(egressDot(applied, '配置' + (applied ? '已应用' : '未应用')));
  card.appendChild(status);

  const rows = [
    ['目标机器', cur ? (egressMachineLabel(cur) + ' · ' + cur.id) : '（还没有任何 agent 连上来）'],
    ['出口 IP', exitIP],
  ];
  // 隧道实测出口（v0.31.31）：这是**平台从这条隧道出去时真正看到的 IP**，
  // 与上面那行（agent 自己直出时看到的 IP）是两条不同链路。
  // 两者不一致 = 这条隧道没落到这台机器上（端口被占 / 转发没建起来），
  // 而页面上会同时挂着两条"线路" —— 那正是"两个 IP 却同一个出口"的样子。
  if (cur) {
    rows.push(['隧道实测出口', cur.tunnel_exit_ip || '（还没探活）']);
  }
  // 用户视角**不显示全局默认出口**：它不属于这个用户（那是平台级开关），
  // 显示出来只会让他以为"我能不能改它"，而答案是不能。
  if (isPlatform) rows.push(['全局默认出口', v.default_egress || '（空 = 直连）']);
  rows.push([isPlatform ? '自挂号绑定' : '我的代挂号绑定',
    (bound || 0) + ' / ' + (total || 0) + ' 个']);
  rows.push(['隧道端口', '127.0.0.1:' + (ports.tunnel_port || '?')
    + '（本机代理端口 ' + (ports.proxy_port || '?') + '）']);
  if (curMsg) rows.push(['最近状态', curMsg]);
  const tbl = el('div', 'sub');
  for (const [k, val] of rows) {
    const line = el('div');
    line.style.padding = '2px 0';
    const kk = el('span', null, k + '：');
    kk.style.color = '#888';
    line.appendChild(kk);
    line.appendChild(el('span', null, String(val)));
    tbl.appendChild(line);
  }
  card.appendChild(tbl);

  // 诊断结论（v0.31.31）：把"为什么两条线路是同一个 IP"直接写在页面上，
  // 而不是让用户自己去猜。
  if (cur && cur.exit_mismatch) {
    const w = el('div', null, '⚠ 这条隧道的实测出口（' + (cur.tunnel_exit_ip || '?') +
      '）与这台机器自报的出口（' + (cur.exit_ip || '?') + '）不一致：' +
      '说明隧道没有落到这台电脑上（端口被另一台占用，或转发没建起来）。' +
      '点下面的「修复出口」会自动换一个端口并让它重建。');
    w.style.cssText = 'margin-top:8px;padding:8px 10px;border-left:3px solid #c0392b;' +
      'background:rgba(192,57,43,.08);font-size:12px;';
    card.appendChild(w);
  }
  if (cur && (cur.alt_ids || []).length) {
    const w = el('div', null, '⚠ 这台电脑上还跑着另一份 agent（' + cur.alt_ids.join('、') + '）：' +
      '两份会抢同一个隧道端口，出口 IP 不会因此多出来。请关掉多余的那份。');
    w.style.cssText = 'margin-top:8px;padding:8px 10px;border-left:3px solid #c0392b;' +
      'background:rgba(192,57,43,.08);font-size:12px;';
    card.appendChild(w);
  }

  // ---- 全部机器一览（多机时，或存在重复接入/同出口时）----
  //
  // 为什么要多一个显示条件（v0.31.26）：一台机器的场景也可能有问题 ——
  // 同一台电脑跑了两份 agent，或两台机器其实从同一条宽带/NAT 出去。
  // 这两种情况在页面上"看起来像多了一条线路"，但并不会多一个 IP；
  // 不点出来，用户会一直以为"我已经有两个出口了"。
  const anyDup = machines.some((m) => (m.same_machine_of || []).length || (m.same_exit_of || []).length);
  if (machines.length > 1 || anyDup) {
    const mt = el('div', 'sub');
    mt.style.marginTop = '10px';
    mt.appendChild(el('div', null, '已接入的机器（每台一条独立隧道，端口不同，可同时在线）：'));
    for (const m of machines) {
      const line = el('div');
      line.style.padding = '2px 0';
      line.appendChild(el('b', null, egressMachineLabel(m) + ' '));
      line.appendChild(el('span', null,
        (m.online ? '● 在线' : '○ 离线')
        + ' · 隧道' + (m.tunnel_reachable ? '通' : '不通')
        + ' · 端口 ' + m.tunnel_port
        + (m.exit_ip ? ' · 出口 ' + m.exit_ip : '')
        + (m.applied ? ' · 已应用' : '')));
      mt.appendChild(line);
      const lab = (k) => egressLabels[k] || k;
      for (const k of (m.same_machine_of || [])) {
        const w = el('div', null, '⚠ 与「' + lab(k) + '」是同一台机器（重复接入）：两条线路必然同一个出口 IP，'
          + '还会互相抢隧道端口 —— 请停掉多余的那份 agent。');
        w.style.color = '#c0392b';
        w.style.paddingLeft = '12px';
        mt.appendChild(w);
      }
      if (m.exit_mismatch) {
        const w = el('div', null, '⚠ 隧道实测出口 ' + (m.tunnel_exit_ip || '?') +
          ' ≠ 本机自报 ' + (m.exit_ip || '?') + '：这条隧道没落到这台机器上');
        w.style.color = '#c0392b';
        w.style.paddingLeft = '12px';
        mt.appendChild(w);
      }
      for (const k of (m.same_exit_of || [])) {
        const w = el('div', null, '同一出口 IP 的还有「' + lab(k) + '」：不是配置错误（同一条宽带 / 同一 NAT），'
          + '但不会带来新的 IP。要得到不同出口，需要改用另一条宽带或换个网络环境。');
        w.style.color = '#b9770e';
        w.style.paddingLeft = '12px';
        mt.appendChild(w);
      }
    }
    card.appendChild(mt);
  }

  // ---- 操作区 ----
  if (!v.can_manage) {
    const tip = el('div', 'sub', '当前账号没有执行出口操作的权限；上面是只读状态。');
    tip.style.marginTop = '10px';
    card.appendChild(tip);
    box.appendChild(card);
    return;
  }

  const row = el('div', 'row');
  row.style.cssText = 'flex-wrap:wrap;align-items:center;gap:8px;margin-top:10px;';

  const pulling = !!egressTimer;      // 等"拉起完成"
  const watching = !!egressWatchTimer; // 等"agent 上线"（安装后）
  const busy = pulling || watching;
  const hasMachine = !!egressSelMachine;

  // ⚠️ 三态按钮的语义（v0.31.25）：**没有机器时主按钮是"下载安装包"**。
  // 之前这里只给一句"请先在目标电脑上运行下面那行命令"—— 用户要自己复制命令、
  // 自己开终端、自己核对参数。现在整条链路是：点下载 → 双击 .bat → 页面自己
  // 检测到机器并自动拉起。用户只在两个地方动手：点一下、双击一下。
  if (!hasMachine) {
    const dl = el('button', null, '① 下载本机代理安装包');
    dl.title = '下载一个 setup 文件：在目标电脑上双击运行即可（无需复制任何参数或密钥）';
    dl.disabled = busy;
    dl.addEventListener('click', () => {
      // 必须用浏览器直接下载（fetch 拿不到附件流，且要触发"另存为"）。
      window.location.href = '/api/egress/home/install';
      egressWatchInstall();
    });
    row.appendChild(dl);

    const refresh = el('button', 'ghost', '② 装好后点这里刷新');
    refresh.disabled = busy;
    refresh.addEventListener('click', () => loadEgressHome());
    row.appendChild(refresh);

    if (watching) {
      const w = el('span', 'sub', '正在等待你的电脑接入…（装好后这里会自动继续）');
      row.appendChild(w);
    }
  } else {
    const pull = el('button', null,
      pulling ? '正在拉起…' : (ready ? '重新拉起并应用' : '一键拉起'));
    pull.disabled = busy;
    pull.title = pulling
      ? '已下发指令，正在等待 agent 完成（最长 90 秒）'
      : '自动完成：下发指令 → 等 agent 起代理与隧道 → 就绪后自动应用配置';
    pull.addEventListener('click', () => egressPull(v));
    row.appendChild(pull);

    // 应用（v0.31.31 改为**非破坏式**）：只接管"还没指定出口"的号。
    //
    // 旧行为是"把我名下所有号改绑到这条线路"，于是第二台电脑点一次拉起，
    // 第一台的号全被静默搬过来 —— 两台机器只有一个在承重。
    // 想整体改绑是另一个显式动作（下面的「全部改绑」），文案会说清代价。
    const applyBtn = el('button', 'ghost', '应用（只绑未指定的号）');
    applyBtn.disabled = busy || !tunnel || !hasMachine;
    applyBtn.title = '把这条线路登记进线路池，并把**还没指定出口**的号绑上来；' +
      '已绑到别的线路的号不动（避免在 B 机上点一下就把 A 机上的号搬走）';
    applyBtn.addEventListener('click', async () => {
      applyBtn.disabled = true;
      try {
        const d = await post('/api/egress/home/apply', { machine: egressSelMachine });
        const ap = (d && d.applied) || {};
        let msg = '已应用：绑定 ' + (ap.host_bound || 0) + ' 个号';
        if (ap.skipped) msg += '，跳过 ' + ap.skipped + ' 个（已显式绑在别的线路上）';
        if (isPlatform) msg += '；默认出口 ' + (ap.default_ok ? '已设置' : '未设置');
        toast(msg, 'ok');
        if (ap.skipped) {
          toast('跳过的号仍走原来的线路。要把它们全部搬到这条线路上，用「全部改绑」', 'warn');
        }
        loadEgressHome();
      } catch (e) {
        toast(egressMsg('应用失败：' + (e && e.message ? e.message : e)), 'bad');
        applyBtn.disabled = false;
      }
    });
    row.appendChild(applyBtn);

    // 全部改绑：旧行为，显式二次确认（这一步会覆盖"每台机器各管一部分号"的分配）。
    const rebind = el('button', 'ghost', '全部改绑');
    rebind.disabled = busy || !tunnel || !hasMachine;
    rebind.title = '把**我名下所有号**都改绑到这条线路（会覆盖你之前为不同机器做的分配）';
    rebind.addEventListener('click', async () => {
      if (!(await confirmAsk('把' + (isPlatform ? '所有自挂号' : '你名下所有代挂号') +
        '都改绑到这条线路？\n\n⚠ 这会覆盖你之前「让 A 机管一部分、B 机管另一部分」的分配，' +
        '所有号都会从这一条线路出去（同一个出口 IP）。'))) return;
      rebind.disabled = true;
      try {
        const d = await post('/api/egress/home/apply', { machine: egressSelMachine, rebind_all: true });
        const ap = (d && d.applied) || {};
        toast('已全部改绑：' + (ap.host_bound || 0) + ' 个号', 'ok');
        loadEgressHome();
      } catch (e) {
        toast(egressMsg('改绑失败：' + (e && e.message ? e.message : e)), 'bad');
        rebind.disabled = false;
      }
    });
    row.appendChild(rebind);

    // 修复出口（v0.31.31）：端口冲突 / 隧道没落到本机 → 换端口重建。
    // 只碰 ports 与 cmd，不动任何账号的出口配置。
    const fix = el('button', 'ghost', '修复出口');
    fix.title = '检查端口冲突与隧道指向：发现问题就换一个端口并让这台机器重建隧道（不改任何账号配置）';
    fix.disabled = busy;
    fix.addEventListener('click', async () => {
      fix.disabled = true;
      try {
        const d = await post('/api/egress/home/repair', {});
        if (!d.repaired) {
          toast('没有发现需要修复的机器（端口无冲突、隧道指向正常）', 'ok');
        } else {
          for (const a of (d.actions || [])) {
            toast('已修 ' + (a.hostname || a.machine) + '：' + a.detail, 'ok');
          }
          setTimeout(() => loadEgressHome(), 6000);
        }
      } catch (e) {
        toast(egressMsg('修复失败：' + (e && e.message ? e.message : e)), 'bad');
      }
      fix.disabled = false;
    });
    row.appendChild(fix);

    const rel = el('button', 'ghost', '停止该机器代理');
    rel.disabled = busy || !hasMachine;
    rel.addEventListener('click', async () => {
      try {
        await post('/api/egress/home/release', { machine: egressSelMachine });
        toast('已向该机器下发停止指令（不会自动改动账号的出口配置）', 'ok');
        loadEgressHome();
      } catch (e) {
        toast(egressMsg('停止失败：' + (e && e.message ? e.message : e)), 'bad');
      }
    });
    row.appendChild(rel);
  }
  card.appendChild(row);

  // 进行中的显式状态（用户要能一眼看出"在跑"而不是"没反应"）。
  if (watching) {
    const w = el('div', 'sub', '安装包已开始下载。在目标电脑上双击运行它 —— ' +
      '装好后本页会自动检测到那台电脑并接着拉起代理，不需要你再操作。');
    w.style.marginTop = '10px';
    card.appendChild(w);
  }
  if (pulling) {
    const w = el('div', 'sub', '已向目标机器下发拉起指令，正在等待它的 agent 响应…（最长 90 秒）');
    w.style.marginTop = '10px';
    card.appendChild(w);
  }

  // ---- 首次使用：在目标电脑上跑一次 agent ----
  // 注意：**新机器是靠 agent 先 poll 才登记出来的** ——
  // 所以这段不能只在"没在线时"显示，而是**没有 machine 时就要显示**；
  // 否则用户在第二台电脑上永远看不到"该怎么让它出现在列表里"。
  if (v.agent_command && (machines.length === 0 || (!online && cur && !cur.is_default) || !machines.some((m) => m.online))) {
    const note = el('div', 'sub');
    note.style.marginTop = '12px';
    note.appendChild(document.createTextNode('在**目标电脑**上'));
    note.appendChild(document.createTextNode(
      '（与 wbproxy.exe、ai.pem 放在同一目录）运行下面这行 —— 每台电脑各跑一份，它会自动登记到上面的目标机器列表：'));
    card.appendChild(note);

    const cmd = el('div', 'sub');
    cmd.style.cssText = 'margin-top:6px;padding:8px 10px;background:rgba(127,127,127,.12);border-radius:6px;' +
      'font-family:ui-monospace,Consolas,monospace;font-size:12px;word-break:break-all;';
    cmd.textContent = v.agent_command;
    card.appendChild(cmd);

    const copy = el('button', 'ghost', '复制命令');
    copy.style.marginTop = '8px';
    copy.addEventListener('click', async () => {
      if (await writeClipboard(v.agent_command)) { toast('已复制', 'ok'); return; }
      // 两条复制路径都失败（极少数环境）→ 退化成"选中让用户自己复制"。
      const r = document.createRange();
      r.selectNodeContents(cmd);
      const sel = window.getSelection();
      sel.removeAllRanges(); sel.addRange(r);
      toast('已选中，请按 Ctrl+C 复制', 'ok');
    });
    card.appendChild(copy);
  }

  box.appendChild(card);

  // ---- 链路说明（一次讲清，避免用户去猜）----
  const help = el('div', 'card');
  help.style.marginTop = '14px';
  const h2 = el('div', 'section-head');
  h2.appendChild(el('h2', null, '这条链路是什么'));
  help.appendChild(h2);
  help.appendChild(el('div', 'sub', isPlatform
    ? ('目标机器上的 agent 跑一个本地代理，并用 SSH 反向隧道把它接到云端。' +
       '于是平台访问上游时，流量会从「你家的宽带」出去 —— 住宅 IP 比机房 IP 友好得多。' +
       '拉起并应用后：全局默认出口指向这条线路（跟随默认的号自动生效），' +
       '所有自挂号被显式绑定到它（不随默认出口漂移）。')
    : ('你自己电脑上的 agent 跑一个本地代理，并用 SSH 反向隧道把它接到云端。' +
       '于是**你自己的代挂账号**访问上游时，流量会从「你家的宽带」出去 —— ' +
       '住宅 IP 比机房 IP 友好得多，也更像正常用户。' +
       '拉起并应用后：**只把你名下的代挂号**绑到这条线路；' +
       '平台的全局默认出口**不会**被改动（那不是你的号该决定的事）。')));
  const warn = el('div', 'sub');
  warn.style.marginTop = '8px';
  warn.appendChild(document.createTextNode('⚠️ 三点要知道：'));
  warn.appendChild(el('br'));
  if (isPlatform) {
    warn.appendChild(document.createTextNode(
      '① **全局默认出口只有一个**：多台机器时，最后应用的那台决定它；要让部分号走另一台，用管理后台的「批量分配」显式绑定；'));
    warn.appendChild(el('br'));
  } else {
    warn.appendChild(document.createTextNode(
      '① 这条线路**只作用于你自己的代挂号**：不会改变平台默认出口，也不会影响任何其他用户的号；'));
    warn.appendChild(el('br'));
  }
  warn.appendChild(document.createTextNode(
    '② 同一条线路只有一个出口 IP —— 绑它的账号共用这个 IP，会保留"同一 IP 出站"的特征；'));
  warn.appendChild(el('br'));
  warn.appendChild(document.createTextNode(
    '③ 家宽上行带宽是瓶颈，且该机器断网/关机时这条链路会一起断（agent 会自动重连）。'));
  help.appendChild(warn);
  box.appendChild(help);
}

// egressWatchInstall 等"agent 上线"（v0.31.25）—— 一键安装的后半程。
//
// 下载安装包只是"把程序送到用户机器上"，真正的接入发生在用户双击之后。
// 这一步**必须由页面自己接着走**：否则用户双击完回到页面看到的还是"没有机器"，
// 于是他会以为失败、再点一次下载 —— 而我们想要的体验是"点一下，剩下它自己完成"。
function egressWatchInstall() {
  if (egressWatchTimer) return;
  egressWatchSince = Date.now();
  const box = document.getElementById('egressHomeBox');
  toast('安装包已开始下载：在目标电脑上双击运行它，装好后本页会自动继续', 'ok');
  if (box) loadEgressHome(); // 立刻把"等待中"渲染出来
  const step = async () => {
    let cur = null;
    try {
      cur = await api('/api/egress/home');
    } catch (e) { /* 轮询失败不终止 */ }
    const b = document.getElementById('egressHomeBox');
    if (cur && b) renderEgressHome(b, cur);
    const ms = (cur && cur.machines) || [];
    if (ms.length) {
      egressWatchTimer = null;
      const label = egressMachineLabel(ms[0]);
      toast('已检测到你的电脑（' + label + '），正在自动拉起出口代理…', 'ok');
      // 交给 egressPull 完成剩下的（下发 → 就绪 → 自动应用）。
      setTimeout(() => egressPull(cur), 300);
      return;
    }
    if (Date.now() - egressWatchSince > EGRESS_INSTALL_WAIT_MS) {
      egressWatchTimer = null;
      toast('还没有等到 agent 上线。请确认安装包已在目标电脑上运行完成' +
            '（窗口最后显示 Done），且那台电脑能访问服务地址', 'bad');
      loadEgressHome();
      return;
    }
    egressWatchTimer = setTimeout(step, 3000);
  };
  egressWatchTimer = setTimeout(step, 3000);
}

// egressPull 一键拉起：向**选中的那台机器**下发指令 → 轮询状态 → 就绪后自动应用配置。
async function egressPull(v) {
  if (egressTimer) return;
  const machine = egressSelMachine;
  if (!machine) {
    toast('还没有已接入的机器：请先在目标电脑上运行 agent（本页下方有命令）', 'bad');
    return;
  }
  // 提示里用**主机名**而不是机器 id：用户视角的 id 是 `u12:abc…` 这种内部键，
  // 对用户毫无意义（他在页面上看到的是主机名，提示里就该是同一个词）。
  const label = egressLabels[machine] || machine;
  try {
    await post('/api/egress/home/pull', { machine });
  } catch (e) {
    toast(egressMsg('下发指令失败：' + (e && e.message ? e.message : e)), 'bad');
    return;
  }
  egressSince = Date.now();
  toast('已向 ' + label + ' 下发拉起指令，正在等待它的 agent…', 'ok');
  const box = document.getElementById('egressHomeBox');
  const step = async () => {
    let cur = null;
    try {
      cur = await api('/api/egress/home');
    } catch (e) { /* 轮询失败不终止，下一轮再试 */ }
    if (cur && box) renderEgressHome(box, cur);
    // 就绪判据看**目标机器**（不是汇总场）——
    // 多机时汇总场取的是"最近上报的那台"，可能不是你点的那台。
    const target = (cur && (cur.machines || []).find((m) => m.id === machine)) || null;
    if (target && target.ready) {
      egressTimer = null;
      try {
        const d = await post('/api/egress/home/apply', { machine });
        toast('已就绪并自动应用：自挂号绑定 ' + (d.applied ? d.applied.host_bound : 0) + ' 个', 'ok');
      } catch (e) {
        toast(egressMsg('已就绪，但自动应用失败：' + (e && e.message ? e.message : e)), 'bad');
      }
      loadEgressHome();
      return;
    }
    if (Date.now() - egressSince > EGRESS_WAIT_MAX_MS) {
      egressTimer = null;
      toast(egressMsg('等待超时：该机器的 agent 未响应。请确认安装包已在那台电脑上运行完成'
        + '（窗口最后显示 Done），并检查 agent 窗口里的输出。'), 'bad');
      loadEgressHome();
      return;
    }
    egressTimer = setTimeout(step, EGRESS_POLL_MS);
  };
  egressTimer = setTimeout(step, EGRESS_POLL_MS);
}
