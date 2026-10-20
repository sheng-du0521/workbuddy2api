#!/usr/bin/env node
// 真浏览器端到端（v0.31.65，审查 F15 的"真实浏览器验收"）。
//
// ## 为什么 jsdom 不够
//
// `tools/uitest.mjs` 用 jsdom 读**磁盘上的 HTML**，验证的是"脚本语法 + 关键 DOM 被填充"。
// 它看不到这些：
//
//   - 真实布局/CSS 导致的"元素在但点不到"（覆盖层、`pointer-events`、z-index）；
//   - 真实 `fetch` 的凭据与重定向（jsdom 里这些是模拟的）；
//   - 浏览器自己的报错（未捕获异常、资源 404 在白屏场景里通常是**唯一线索**）。
//
// ## 依赖：只用系统已装的 Edge/Chrome（不下载 Chromium）
//
// `playwright-core` 不带浏览器；这里用 `channel: msedge|chrome` 驱动本机浏览器。
// 目的很实际：CI/离线环境装不了 150MB 的 Chromium，而"必须有真浏览器才能跑"的检查
// 一旦装不上就会被关掉 —— 那等于没有。没有浏览器时**明确返回 3（跳过）**，不假装通过。
//
// 用法：
//   node tools/e2e.mjs --base http://127.0.0.1:8899 [--shot out.png]
// 退出码：0 通过 / 1 失败 / 3 没有可用浏览器（跳过）

import { chromium } from 'playwright-core';

const arg = (name, def) => {
  const i = process.argv.indexOf('--' + name);
  return i > 0 && process.argv[i + 1] ? process.argv[i + 1] : def;
};

const base = arg('base', 'http://127.0.0.1:8780');
const shot = arg('shot', '');
const user = 'e2e_' + Date.now().toString(36);
const pass = 'E2e_pw_2026';

function fail(msg) {
  console.log('FAIL: ' + msg);
  process.exit(1);
}

let browser = null;
let lastErr = '';
for (const channel of ['msedge', 'chrome']) {
  try {
    browser = await chromium.launch({ channel, headless: true });
    console.log('浏览器：' + channel);
    break;
  } catch (e) {
    lastErr = e.message;
  }
}
if (!browser) {
  console.log('SKIP: 本机没有 Edge/Chrome，跳过真浏览器端到端');
  console.log('  原因：' + String(lastErr).split('\n')[0]);
  process.exit(3);
}

const errors = [];
try {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await ctx.newPage();
  // 未捕获异常是"白屏"的第一手证据，必须收集起来而不是让脚本靠超时失败。
  page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
  // 记录**哪个请求**失败：只报"Failed to load resource"等于没说 —— 401（登录前探测）
  // 与 404（资源真的缺了）处置完全不同，必须能分辨。
  page.on('response', (r) => {
    if (r.status() >= 400) errors.push(`http ${r.status()}: ${r.url()}`);
  });

  // ① 注册（真实表单 + 真实 fetch）
  //
  // ⚠️ 先点「注册」标签页：登录页是**两个 pane 切换**，注册表单默认是 hidden 的。
  // 这一点用 API 直接 POST 是看不见的 —— 第一次跑真浏览器就在这里超时
  // （`#btnReg` 一直 hidden），也算"真浏览器验收"的一次真实收获。
  await page.goto(base + '/', { waitUntil: 'domcontentloaded' });
  await page.click('.tabs .tab[data-tab="register"]');
  await page.waitForSelector('#btnReg', { state: 'visible', timeout: 10000 });
  await page.fill('#regUser', user);
  await page.fill('#regPass', pass);
  await page.fill('#regPass2', pass);
  await page.click('#btnReg');
  await page.waitForURL('**/dashboard.html', { timeout: 15000 });
  console.log('注册并跳转控制台 ✓');

  // ② 控制台真的渲染出来了（不是只有壳）：余额区可见
  await page.waitForSelector('#balance', { state: 'visible', timeout: 10000 });
  const title = await page.title();
  if (!title) fail('控制台标题为空（页面可能白屏）');

  // ③ 切到「API 密钥」，生成一把 Key，并断言**明文真的显示在弹窗里**
  await page.getByRole('button', { name: 'API 密钥' }).first().click();
  await page.waitForSelector('#btnNewKey', { state: 'visible', timeout: 10000 });
  await page.click('#btnNewKey');
  await page.waitForFunction(() => document.body.innerText.includes('sk-wb-'), null,
    { timeout: 15000 });
  console.log('生成密钥并在页面展示明文 ✓');

  // ④ 顺手确认面板不是"渲染成空白"：主容器有可见文本
  const nav = await page.evaluate(() => document.body.innerText.trim().length);
  if (nav < 200) fail('控制台正文只有 ' + nav + ' 个字符（疑似渲染失败）');

  if (shot) {
    await page.screenshot({ path: shot, fullPage: false });
    console.log('截图：' + shot);
  }

  // 登录页会探测一次会话（/api/auth/me），未登录时 401 是**正常回答**，
  // 不是缺陷。只放行这一条：其它任何 4xx/5xx 都要判失败 ——
  // 把所有 401 一起放过，就等于"顺手把真正的认证回归也放过了"。
  const expected = /^http 401: .*\/api\/auth\/me$/;
  const fatal = errors.filter((e) => !expected.test(e));
  if (fatal.length) {
    fail('浏览器报错 ' + fatal.length + ' 条：\n  ' + fatal.slice(0, 5).join('\n  '));
  }
  if (errors.length > fatal.length) {
    console.log('（忽略 ' + (errors.length - fatal.length) + ' 条预期内探测：登录前 /api/auth/me 401）');
  }
  console.log('E2E OK：注册 → 控制台 → 生成密钥（真浏览器，无 console/page 错误）');
} finally {
  await browser.close();
}
