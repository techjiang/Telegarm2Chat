import { connect, evaluate, navigate, shot, sleep, setViewport } from './cdp.mjs';

const APP = process.env.T2C_URL || 'http://localhost:8080';
let pass = 0, fail = 0;
const check = (name, cond, extra = '') => {
  if (cond) { pass++; console.log('  ✓', name); }
  else { fail++; console.log('  ✗', name, extra); }
};

const c = await connect();
await setViewport(c, 1280, 860);

// 收集前端报错
const errors = [];
c.ws.addEventListener('message', (e) => {
  const m = JSON.parse(e.data);
  if (m.method === 'Runtime.exceptionThrown') {
    errors.push(m.params.exceptionDetails.exception?.description || 'unknown');
  }
  if (m.method === 'Runtime.consoleAPICalled' && m.params.type === 'error') {
    errors.push(m.params.args.map(a => a.value || a.description).join(' '));
  }
});

console.log('== 首次访问应显示注册页 ==');
await navigate(c, APP);
const title = await evaluate(c, `document.querySelector('.auth-card h1')?.textContent`);
check('渲染注册页（首用户）', title === '创建账号', `得到 ${title}`);
check('提示首个用户为管理员',
  (await evaluate(c, `document.querySelector('.auth-card .sub')?.textContent || ''`)).includes('管理员'));

console.log('== 注册流程 ==');
await evaluate(c, `
  (() => {
    document.querySelector('#f-user').value = 'webuser';
    document.querySelector('#f-name').value = '网页用户';
    document.querySelector('#f-pass').value = 'webuserpass1234';
    document.querySelector('.auth-card form').dispatchEvent(new Event('submit', {bubbles:true, cancelable:true}));
  })()
`);
await sleep(1200);
const inApp = await evaluate(c, `!!document.querySelector('.shell')`);
check('注册后进入主界面', inApp);
check('token 已持久化', !!(await evaluate(c, `localStorage.getItem('t2c.token')`)));
check('侧栏显示用户名', (await evaluate(c, `document.querySelector('.me-row')?.textContent || ''`)).includes('网页用户'));

console.log('== WebSocket 连接 ==');
await sleep(800);
const dotOn = await evaluate(c, `document.querySelector('.brand .dot')?.className`);
check('WS 已连接（绿点）', (dotOn || '').includes('on'), `得到 ${dotOn}`);

console.log('== 创建频道 ==');
await evaluate(c, `document.querySelectorAll('.search-row .icon-btn')[0].click()`);
await sleep(300);
check('打开创建弹层', await evaluate(c, `!!document.querySelector('.modal')`));
await evaluate(c, `
  (() => {
    const inputs = document.querySelectorAll('.modal input');
    inputs[0].value = '前端测试频道';
    inputs[1].value = '浏览器端到端';
    [...document.querySelectorAll('.modal .btn')].find(b => b.textContent === '确定').click();
  })()
`);
await sleep(900);
const chatTitle = await evaluate(c, `document.querySelector('.panel-head h2')?.textContent`);
check('频道创建并打开', chatTitle === '前端测试频道', `得到 ${chatTitle}`);
const slug = await evaluate(c, `document.querySelector('.slug-chip')?.textContent || ''`);
check('显示频道号', slug.length > 5, `得到 ${slug}`);

console.log('== 发送消息 ==');
await evaluate(c, `
  (() => {
    const i = document.querySelector('#input');
    i.value = '这是浏览器发的第一条消息 @webuser';
    i.dispatchEvent(new Event('input', {bubbles:true}));
  })()
`);
await evaluate(c, `document.querySelector('.send-btn').click()`);
await sleep(1000);
const msgs = await evaluate(c, `[...document.querySelectorAll('.msg .text')].map(n => n.textContent)`);
check('消息渲染在列表', msgs.some(m => m.includes('第一条消息')), JSON.stringify(msgs));
check('@提及被高亮', (await evaluate(c, `document.querySelectorAll('.who-name').length`)) > 0);
check('无残留"发送中"状态',
  (await evaluate(c, `document.querySelectorAll('.msg.pending').length`)) === 0);
check('输入框已清空', (await evaluate(c, `document.querySelector('#input').value`)) === '');

console.log('== XSS 防护 ==');
await evaluate(c, `
  (() => {
    const i = document.querySelector('#input');
    i.value = '<img src=x onerror="window.__xss=1">恶意内容';
    i.dispatchEvent(new Event('input', {bubbles:true}));
    document.querySelector('.send-btn').click();
  })()
`);
await sleep(1000);
const xssRan = await evaluate(c, `!!window.__xss`);
check('注入脚本未执行', xssRan === false);
const imgInjected = await evaluate(c, `document.querySelectorAll('.messages img').length`);
check('未注入 img 元素', imgInjected === 0, `得到 ${imgInjected}`);
const textShown = await evaluate(c, `[...document.querySelectorAll('.msg .text')].map(n=>n.textContent).join('|')`);
check('尖括号被当文本转义', textShown.includes('<img'), textShown.slice(0, 120));

console.log('== 刷新后保持登录并恢复历史 ==');
await navigate(c, APP);
await sleep(1200);
check('刷新后仍在主界面', await evaluate(c, `!!document.querySelector('.shell')`));
const listCount = await evaluate(c, `document.querySelectorAll('.chat-item').length`);
check('会话列表已恢复', listCount >= 1, `得到 ${listCount}`);
await evaluate(c, `document.querySelector('.chat-item').click()`);
await sleep(900);
const restored = await evaluate(c, `[...document.querySelectorAll('.msg .text')].map(n=>n.textContent).join('|')`);
check('历史消息已加载', restored.includes('第一条消息'), restored.slice(0, 100));

console.log('== 发送后可撤回 ==');
const delBtn = await evaluate(c, `(() => {
  const rows = [...document.querySelectorAll('.msg')];
  const last = rows[rows.length-1];
  const b = last.querySelector('.msg-actions button');
  if (!b) return 'none';
  b.click(); return 'clicked';
})()`);
check('撤回按钮存在并点击', delBtn === 'clicked', delBtn);
await sleep(800);
const afterDel = await evaluate(c, `[...document.querySelectorAll('.msg .text')].map(n=>n.textContent).join('|')`);
check('消息已从界面移除', !afterDel.includes('恶意内容'), afterDel.slice(0, 100));

console.log('== 头像与昵称 ==');
await evaluate(c, `document.querySelectorAll('.search-row .icon-btn')[1].click()`);
await sleep(500);
check('设置里有「我的资料」入口', await evaluate(c, `!!document.querySelector('.profile-entry')`));
await evaluate(c, `document.querySelector('.profile-entry').click()`);
await sleep(450);
check('资料面板打开且只有一层弹层',
  (await evaluate(c, `document.querySelectorAll('.modal-bg').length`)) === 1);
check('默认是字母头像', await evaluate(c, `(() => {
  const p = document.querySelector('.avatar-preview .avatar');
  return !!p && !p.querySelector('img');
})()`));

// 用一支 1×1 的 PNG 走真实文件选择，覆盖 FileReader → canvas → 上传整条链路。
const TINY_PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
  'base64');
const { writeFileSync } = await import('node:fs');
writeFileSync('/tmp/t2c-avatar.png', TINY_PNG);
const doc = await c.send('DOM.getDocument', {});
const fi = await c.send('DOM.querySelector', { nodeId: doc.root.nodeId, selector: '.modal input[type=file]' });
await c.send('DOM.setFileInputFiles', { files: ['/tmp/t2c-avatar.png'], nodeId: fi.nodeId });
await sleep(2200);
const previewSrc = await evaluate(c, `document.querySelector('.avatar-preview .avatar img')?.src || ''`);
check('头像上传后预览出图', previewSrc.length > 0, previewSrc.slice(0, 60));
await evaluate(c, `document.querySelector('.modal-bg')?.remove()`);
await sleep(300);

await navigate(c, APP);
await sleep(1400);
await evaluate(c, `document.querySelectorAll('.search-row .icon-btn')[1].click()`);
await sleep(400);
await evaluate(c, `document.querySelector('.profile-entry').click()`);
await sleep(600);
check('刷新后头像从服务端恢复', await evaluate(c, `(() => {
  const i = document.querySelector('.avatar-preview .avatar img');
  return !!i && i.src.includes('/avatar');
})()`));

await evaluate(c, `(() => {
  const i = document.querySelector('.modal input[type=text]');
  i.value = '网页用户改名';
  [...document.querySelectorAll('.modal .btn')].find(b => b.textContent === '保存昵称').click();
})()`);
await sleep(1200);
check('昵称已更新到侧栏', (await evaluate(c, `document.querySelector('.me-row')?.textContent || ''`)).includes('网页用户改名'));
await evaluate(c, `document.querySelector('.modal-bg')?.remove()`);
// 刷新后没有选中会话，先点回会话再断言消息里的头像。
await evaluate(c, `document.querySelector('.chat-item')?.click()`);
await sleep(1200);
check('消息旁渲染出头像图片', (await evaluate(c, `document.querySelectorAll('.msg .av img').length`)) >= 1);
// 只断言「已经加载完的图都不破」：lazy 图此刻可能还没开始下载。
check('已加载的图片都不破损', await evaluate(c, `
  [...document.querySelectorAll('img')].every(i => !i.complete || i.naturalWidth > 0)`));

console.log('== 按频道号加入（幂等）==');
const mySlug = await evaluate(c, `document.querySelector('.slug-chip')?.textContent || ''`);
check('拿到频道号', mySlug.length > 5, mySlug);
const beforeJoins = await evaluate(c, `
  [...document.querySelectorAll('.system-line')].filter(n => n.textContent.includes('加入了频道')).length`);
await evaluate(c, `document.querySelectorAll('.search-row .icon-btn')[0].click()`);
await sleep(350);
await evaluate(c, `(() => {
  const ins = document.querySelectorAll('.modal input');
  ins[3].value = ${JSON.stringify(mySlug)};
  [...document.querySelectorAll('.modal .btn')].find(b => b.textContent === '确定').click();
})()`);
await sleep(1100);
check('重复加入不报错', !(await evaluate(c, `!!document.querySelector('.modal .alert:not(.hidden)')`)));
await sleep(800);
const afterJoins = await evaluate(c, `
  [...document.querySelectorAll('.system-line')].filter(n => n.textContent.includes('加入了频道')).length`);
check('重复加入不重复写系统消息', afterJoins === beforeJoins, `${beforeJoins} -> ${afterJoins}`);
await evaluate(c, `document.querySelector('.modal-bg')?.remove()`);

console.log('== 搜索 ==');
await evaluate(c, `
  (() => {
    const s = document.querySelector('.search-row input');
    s.value = '第一条';
    s.dispatchEvent(new KeyboardEvent('keydown', {key:'Enter', bubbles:true}));
  })()
`);
await sleep(900);
const hitCount = await evaluate(c, `document.querySelectorAll('.result-item').length`);
check('搜索结果有命中', hitCount >= 1, `得到 ${hitCount}`);
check('命中词高亮', (await evaluate(c, `document.querySelectorAll('.result-item mark').length`)) >= 1);
await evaluate(c, `document.querySelector('.modal-bg')?.remove()`);

console.log('== 响应式（移动端）==');
await setViewport(c, 390, 844, true);
await sleep(400);
const sidebarHidden = await evaluate(c, `document.querySelector('.sidebar').getBoundingClientRect().right <= 1`);
check('移动端侧栏默认收起', sidebarHidden);
check('汉堡按钮可见', await evaluate(c, `(() => {
  const b = document.querySelector('.mobile-toggle');
  return b && getComputedStyle(b).display !== 'none';
})()`));
await evaluate(c, `document.querySelector('.mobile-toggle').click()`);
await sleep(400);
check('点击后抽屉展开', await evaluate(c, `document.querySelector('.shell').classList.contains('drawer-open')`));
check('抽屉有遮罩层', await evaluate(c, `(() => {
  const s = document.querySelector('.scrim');
  return s && getComputedStyle(s).display !== 'none';
})()`));
await evaluate(c, `document.querySelector('.scrim').click()`);
await sleep(350);
check('点遮罩关闭抽屉', !(await evaluate(c, `document.querySelector('.shell').classList.contains('drawer-open')`)));
await shot(c, '/tmp/ui-mobile.png');

await setViewport(c, 1280, 860);
await sleep(300);
await evaluate(c, `document.querySelector('.shell')?.classList.remove('drawer-open')`);
await shot(c, '/tmp/ui-desktop.png');

console.log('== 退出登录 ==');
await evaluate(c, `document.querySelectorAll('.search-row .icon-btn')[1].click()`);
await sleep(600);
await evaluate(c, `[...document.querySelectorAll('.modal .btn')].find(b=>b.textContent==='退出登录').click()`);
await sleep(700);
check('回到登录页', (await evaluate(c, `document.querySelector('.auth-card h1')?.textContent`)) === 'Telegarm2Chat');
check('token 已清除', !(await evaluate(c, `localStorage.getItem('t2c.token')`)));

console.log('== 错误处理 ==');
await evaluate(c, `
  (() => {
    document.querySelector('#f-user').value = 'webuser';
    document.querySelector('#f-pass').value = 'wrongpassword123';
    document.querySelector('.auth-card form').dispatchEvent(new Event('submit', {bubbles:true, cancelable:true}));
  })()
`);
await sleep(900);
const errText = await evaluate(c, `document.querySelector('.alert')?.textContent || ''`);
check('错误密码显示提示', errText.includes('错误'), errText);

console.log('\n前端控制台错误:', errors.length ? errors : '无');
check('无未捕获 JS 异常', errors.length === 0, errors.join('; ').slice(0, 200));

console.log(`\n结果: ${pass} 通过, ${fail} 失败`);
c.ws.close();
process.exit(fail ? 1 : 0);
