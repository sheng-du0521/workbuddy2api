// 前端**真机复现**工具（v0.24.9 入库）：用 jsdom 加载管理页，把 fetch 直通
// **真实后端**（X-Admin-Key 鉴权），走完「点导航 → 渲染卡片 → 点开明细」这条
// 用户真实路径，并打印**实际发出的每一个请求**。
//
// 为什么需要它：本文档里多次出现「前端看起来坏了，其实接口是好的」这类分歧
// （v0.24.9 的「不显示积分明细」就是典型）。uitest.mjs 用的是**桩数据**，
// 只能验证渲染逻辑；这个工具连的是真库真上游，能回答「用户看到的到底是什么」。
//
// 用法：
//   node tools/uirepro.mjs [页面=admin.html] [导航=host]      # 完整走一遍（含点击）
//   SKIP_CLICK=1 node tools/uirepro.mjs admin.html pool       # 只渲染不点开
//     → 用于验证「渲染即显示明细、且零出站」这类不变量
//   WB_ADMIN_USER=xxx node tools/uirepro.mjs …                # 覆盖管理员用户名
//
// 注：/api/auth/me 走用户会话（不认 X-Admin-Key），真浏览器里由 admin-login 建会话；
// 本工具没有管理员密码，故**只这一个端点**用本地桩，其余全部直通真实后端。
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { JSDOM, VirtualConsole } from 'jsdom';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const UI = path.join(ROOT, 'cmd/webapp/ui');
const APP_JS = fs.readFileSync(path.join(UI, 'app.js'), 'utf8');
const KEY = fs.readFileSync(path.join(ROOT, 'dist/data/admin.key'), 'utf8').trim();

const page = process.argv[2] || 'admin.html';
const BASE = 'http://127.0.0.1:8780';

const log = [];
const seen = [];

// 管理页需要**已登录会话**（与真浏览器一致）。先用密钥换一次真实登录，拿到 Cookie 再复用，
// 否则所有接口都会 401，复现就失去了意义。
async function login() {
  const name = process.env.WB_ADMIN_USER || 'admin';
  const res = await fetch(BASE + '/api/auth/admin/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username: name, password: KEY }),
  });
  const setC = res.headers.getSetCookie ? res.headers.getSetCookie() : [res.headers.get('set-cookie')].filter(Boolean);
  log.push('登录: HTTP ' + res.status + ' · cookie ' + (setC.length ? setC[0].split(';')[0] : '(无)'));
  return setC.map((c) => c.split(';')[0]).join('; ');
}

async function run() {
  return new Promise((resolve) => {
    const html = fs.readFileSync(path.join(UI, page), 'utf8')
      .replace('<script src="/app.js"></script>', '<script>' + APP_JS + '</script>');
    const vc = new VirtualConsole();
    vc.on('jsdomError', (e) => { if (!/Not implemented: navigation/.test(String(e && e.message))) log.push('[jsdomError] ' + e.message); });
    vc.on('error', (...a) => log.push('[console.error] ' + a.join(' ')));

    const dom = new JSDOM(html, {
      runScripts: 'dangerously', pretendToBeVisual: true, virtualConsole: vc,
      url: BASE + '/' + page,
      beforeParse(win) {
        win.fetch = async (url, opts = {}) => {
          const u = String(url);
          // /api/auth/me 走的是**用户会话**（不认 X-Admin-Key），真浏览器里由 admin-login 建会话。
          // 本脚本没有管理员密码，故只这一个端点用本地桩，其余全部直通真实后端。
          if (u.startsWith('/api/auth/me')) {
            const body = JSON.stringify({ user: { id: 1, username: 'admin', is_admin: true } });
            seen.push({ u, status: 200, body });
            return { ok: true, status: 200, text: () => Promise.resolve(body), json: () => Promise.resolve(JSON.parse(body)) };
          }
          const full = u.startsWith('http') ? u : BASE + u;
          const headers = Object.assign({ 'X-Admin-Key': KEY }, opts.headers || {});
          let res, text;
          try {
            res = await fetch(full, { method: opts.method || 'GET', headers, body: opts.body });
            text = await res.text();
          } catch (e) {
            seen.push({ u, status: 'FETCH-ERR', body: String(e.message).slice(0, 160) });
            return { ok: false, status: 0, text: () => Promise.resolve(''), json: () => Promise.resolve({}) };
          }
          seen.push({ u, status: res.status, body: text.slice(0, 300) });
          return {
            ok: res.ok, status: res.status,
            text: () => Promise.resolve(text),
            json: () => Promise.resolve(JSON.parse(text)),
          };
        };
        win.onerror = (m, s, l, c) => log.push('[window.onerror] ' + m + ' @' + l + ':' + c);
        win.confirm = () => true;
        win.alert = () => {};
      },
    });

    const win = dom.window;
    const doc = win.document;
    const wait = (ms) => new Promise((r) => win.setTimeout(r, ms));

    (async () => {
      await wait(1200);
      const items = Array.from(doc.querySelectorAll('.nav-item'));
      log.push('导航项: ' + items.map((x) => x.dataset.nav).join(', '));
      const want = process.argv[3] || 'host';
      const acct = items.find((x) => x.dataset.nav === want);
      if (!acct) { log.push('!! 没找到导航项 ' + want); }
      else { log.push('点击导航: ' + want + ' / ' + acct.textContent.trim()); acct.dispatchEvent(new win.MouseEvent('click', { bubbles: true })); }

      await wait(2500);
      const cards = doc.querySelectorAll('.ecard, .card');
      log.push('卡片数: ' + cards.length);
      const box = doc.querySelector('.credit');
      log.push('积分区块存在: ' + !!box);
      if (box) log.push('积分区块文本: ' + box.textContent.trim().slice(0, 120));
      const btn = doc.querySelector('.credit .credit-more');
      log.push('明细相关按钮: ' + Array.from(doc.querySelectorAll('.credit .credit-more'))
        .map((b) => '"' + b.textContent.trim() + '"').join(' / '));
      if (process.env.SKIP_CLICK) {
        log.push('（SKIP_CLICK=1：只渲染，不点开 —— 用于验证"零出站就能看到明细"）');
      } else if (btn) {
        btn.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
        await wait(6000);
        const after = doc.querySelector('.credit');
        log.push('--- 点击后 ---');
        log.push('文本: ' + (after ? after.textContent.trim().slice(0, 300) : '(区块消失)'));
        log.push('HTML: ' + (after ? after.innerHTML.slice(0, 600) : ''));
        // 再点「查看全部 N 个积分包」——弹窗是明细的第二层，单独验。
        const more = Array.from(doc.querySelectorAll('.credit-more')).find((b) => /查看全部/.test(b.textContent));
        log.push('「查看全部」按钮: ' + (more ? '"' + more.textContent.trim() + '"' : '缺失'));
        if (more) {
          more.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
          await wait(1000);
          const modal = doc.querySelector('.modal, .modal-body');
          log.push('弹窗: ' + (modal ? modal.textContent.trim().slice(0, 240) : '(未出现)'));
        }
      }
      console.log('===== ' + page + ' =====');
      log.forEach((l) => console.log('  ' + l));
      console.log('  ---- 实际发出的请求 ----');
      seen.forEach((s) => console.log('  ' + s.status + '  ' + s.u + '\n        ' + s.body.replace(/\n/g, ' ').slice(0, 200)));
      try { dom.window.close(); } catch (e) {}
      resolve();
    })();
  });
}
run();
