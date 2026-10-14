#!/usr/bin/env node
// jscheck.mjs —— 抽取 HTML 内联 <script> 并做语法检查。
//
// 背景：本项目前端是「无框架 + 内联脚本」（admin.html 内联约 4300 行 JS）。
// 没有构建步骤意味着**没有任何东西会在提交前发现语法错误** —— 而坏一个字符
// 就是整页白屏。本脚本是那条最低限度的防线，跑起来零依赖。
//
// 用法：node tools/jscheck.mjs <file.html> [file2.html ...]
//
// 为什么放进仓库（v0.24.0）：此前 jscheck.js / uitest.js 住在仓库外的
// ~/.workbuddy/.../workspace/ 且硬编码绝对路径 —— 不受版本控制、不进 CI、换机即失效
// （见 docs/项目全面审查报告-v0.23.3.md 的 P1-3）。
import fs from 'node:fs';
import path from 'node:path';

const files = process.argv.slice(2);
if (files.length === 0) {
  console.error('用法：node tools/jscheck.mjs <file.html> [file2.html ...]');
  process.exit(2);
}

let total = 0;
let bad = 0;
for (const f of files) {
  if (!fs.existsSync(f)) {
    // 文件不存在单独报，避免被当成「没有内联脚本」而静默通过。
    console.error(`${f}: 文件不存在`);
    bad++;
    continue;
  }
  const html = fs.readFileSync(f, 'utf8');
  // 只取**内联**脚本（带 src= 的外部脚本不检查）。
  const re = /<script(?![^>]*\bsrc=)[^>]*>([\s\S]*?)<\/script>/gi;
  let m;
  let i = 0;
  let errs = 0;
  while ((m = re.exec(html))) {
    i++;
    try {
      // new Function 只做语法解析，不执行 —— 等价于 node --check 对模块的作用。
      // eslint-disable-next-line no-new-func
      new Function(m[1]);
    } catch (e) {
      errs++;
      bad++;
      console.error(`${f} SCRIPT#${i} ERROR: ${e.message}`);
    }
  }
  total += i;
  console.log(`${path.relative(process.cwd(), f)}: checked ${i} inline scripts, errors=${errs}`);
}
console.log(`TOTAL scripts=${total} errors=${bad}`);
process.exit(bad ? 1 : 0);
