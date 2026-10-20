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

  // ③b 一键接入（v0.31.69，需求 P3）：点开弹窗 → 生成 → 看到下载按钮。
  //
  // 只验到"生成成功、能下载"这一步，**不点下载**：
  //   · 无头浏览器点下载会在临时目录留一个含明文 key 的文件；
  //   · 真正的"脚本能不能写对文件"由 Go 用例真跑 .bat 覆盖（那层更可靠）。
  // 这里要抓的是 JS 层的断链：按钮没绑、弹窗打不开、接口路径写错、渲染抛异常。
  // 先把「已生成 N 把密钥」弹窗关掉：它的遮罩盖住整页，不关就点不到卡片上的按钮
  // （实测表现为 locator 一直等不到可点击元素，而元素其实一直在）。
  if (await page.locator('.modal-mask').count()) {
    await page.locator('.modal-mask .modal-x').first().click();
    await page.waitForSelector('.modal-mask', { state: 'detached', timeout: 5000 });
  }
  // ⚠️ 不能用 getByRole('button', {name:'接入'})：左侧导航里有「接入说明」，
  // 名字是**子串匹配**，会先命中它（实测点了导航，弹窗永远不出现）。
  // 限定在密钥卡（.ecard）里找，那里只有我们加的那颗按钮。
  try {
    await page.locator('.ecard button', { hasText: '接入' }).first().click();
  } catch (e) {
    // 失败时把卡片里的按钮文本打出来 —— 只报 "locator 超时" 等于没说，
    // 到底是"按钮没渲染"还是"文本不一样"必须能一眼区分。
    const dump = await page.evaluate(() => {
      const cards = Array.from(document.querySelectorAll('.ecard'));
      return cards.map((c, i) => 'card' + i + ': ' +
        Array.from(c.querySelectorAll('button')).map((b) => JSON.stringify(b.innerText)).join(' | ')).join('\n');
    });
    console.log('调试 · 密钥卡内的按钮：\n' + dump);
    throw e;
  }
  await page.waitForSelector('.modal input[list="onboardModelList"]', { timeout: 10000 });
  await page.fill('.modal input[list="onboardModelList"]', 'glm-5.3');
  await page.getByRole('button', { name: '生成接入包' }).click();
  await page.waitForFunction(
    () => /下载 wb2api-onboard-\w+\.(bat|sh)/.test(document.body.innerText), null,
    { timeout: 15000 });
  // 手动兜底必须同时给出来（不给的话"不想跑脚本"的人就没有第二条路）。
  if (!(await page.evaluate(() => document.body.innerText.includes('复制 JSON')))) {
    fail('一键接入弹窗缺少「复制 JSON」兜底');
  }
  console.log('一键接入弹窗可用（生成脚本 + 手动兜底）✓');

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
  console.log('E2E OK：注册 → 控制台 → 生成密钥 → 一键接入（真浏览器，无 console/page 错误）');
} finally {
  await browser.close();
}
