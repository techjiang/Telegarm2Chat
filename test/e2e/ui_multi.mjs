import { connect, evaluate, navigate, shot, sleep, setViewport } from './cdp.mjs';

const APP = process.env.T2C_URL || 'http://localhost:8080';
let pass = 0, fail = 0;
const check = (n, c, e = '') => { c ? (pass++, console.log('  ✓', n)) : (fail++, console.log('  ✗', n, e)); };

// 两个独立 CDP 连接 = 两个独立"浏览器"
const A = await connect(true);
const B = await connect(true);
for (const c of [A, B]) await setViewport(c, 1100, 760);

// 用页面内 fetch 完成注册（等价于表单提交后写入 token），再刷新进入应用
const reg = async (c, user, name) => {
  await navigate(c, APP);
  const ok = await evaluate(c, `
    (async () => {
      localStorage.clear();
      const r = await fetch('/api/v1/auth/register', {
        method:'POST', headers:{'Content-Type':'application/json'},
        body: JSON.stringify({username:'${user}', displayName:'${name}', password:'${user}pass1234'})
      });
      const j = await r.json();
      if (!j.ok) return 'ERR:' + JSON.stringify(j.error);
      localStorage.setItem('t2c.token', j.data.token);
      return 'OK';
    })()
  `);
  if (ok !== 'OK') throw new Error('注册失败: ' + ok);
  await navigate(c, APP);
  await sleep(900);
};

console.log('== 两个用户各自注册 ==');
await reg(A, 'zhangsan', '张三');
await reg(B, 'lisi', '李四');
check('A 进入主界面', await evaluate(A, `!!document.querySelector('.shell')`));
check('B 进入主界面', await evaluate(B, `!!document.querySelector('.shell')`));

console.log('== A 建频道，B 用频道号加入 ==');
await evaluate(A, `document.querySelectorAll('.search-row .icon-btn')[0].click()`);
await sleep(300);
await evaluate(A, `
  (() => {
    const i = document.querySelectorAll('.modal input');
    i[0].value = '双人测试';
    [...document.querySelectorAll('.modal .btn')].find(b=>b.textContent==='确定').click();
  })()
`);
await sleep(900);
const slug = await evaluate(A, `document.querySelector('.slug-chip').textContent`);
check('A 拿到频道号', slug.startsWith('c-'), slug);

// B 通过邀请接口加入（前端通过"发起私聊"入口不便测，直接走 API 更贴近真实邀请流程）
await evaluate(B, `
  fetch('/api/v1/chats/join', {
    method:'POST',
    headers:{'Content-Type':'application/json','Authorization':'Bearer '+localStorage.getItem('t2c.token')},
    body: JSON.stringify({slug:'${slug}'})
  }).then(r=>r.json())
`);
await sleep(600);
await navigate(B, APP);
await sleep(1200);
const bChats = await evaluate(B, `[...document.querySelectorAll('.chat-title')].map(n=>n.textContent)`);
check('B 看到该频道', bChats.includes('双人测试'), JSON.stringify(bChats));

console.log('== A 发消息，B 应实时收到（不刷新）==');
await evaluate(B, `[...document.querySelectorAll('.chat-item')].find(n=>n.textContent.includes('双人测试')).click()`);
await sleep(700);
await evaluate(A, `
  (() => {
    const i = document.querySelector('#input');
    i.value = '@lisi 实时推送验证消息';
    i.dispatchEvent(new Event('input', {bubbles:true}));
    document.querySelector('.send-btn').click();
  })()
`);
await sleep(1500);
const bTexts = await evaluate(B, `[...document.querySelectorAll('.msg .text')].map(n=>n.textContent).join('|')`);
check('B 实时收到消息（未刷新）', bTexts.includes('实时推送验证消息'), bTexts.slice(0, 120));
check('B 能看到 @提及高亮', (await evaluate(B, `document.querySelectorAll('.who-name').length`)) > 0);
check('B 的未读已清除', (await evaluate(B, `document.querySelectorAll('.chat-item .unread').length`)) === 0);

console.log('== B 回复，A 应实时收到 ==');
await evaluate(B, `
  (() => {
    const i = document.querySelector('#input');
    i.value = '收到，这是李四的回复';
    i.dispatchEvent(new Event('input', {bubbles:true}));
    document.querySelector('.send-btn').click();
  })()
`);
await sleep(1500);
const aTexts = await evaluate(A, `[...document.querySelectorAll('.msg .text')].map(n=>n.textContent).join('|')`);
check('A 实时收到回复', aTexts.includes('李四的回复'), aTexts.slice(0, 120));
check('A 侧栏预览已更新',
  (await evaluate(A, `document.querySelector('.chat-item .chat-preview')?.textContent || ''`)).includes('李四的回复'));

console.log('== 撤回同步 ==');
await evaluate(B, `(() => {
  const rows=[...document.querySelectorAll('.msg')];
  const target=rows.find(r=>r.textContent.includes('李四的回复'));
  target.querySelector('.msg-actions button')?.click();
})()`);
await sleep(1200);
const aAfter = await evaluate(A, `[...document.querySelectorAll('.msg .text')].map(n=>n.textContent).join('|')`);
check('A 端消息同步消失', !aAfter.includes('李四的回复'), aAfter.slice(0, 120));

await evaluate(A, `document.querySelector('.chat-item').click()`);
await sleep(400);
await shot(A, '/tmp/ui-a.png');
await shot(B, '/tmp/ui-b.png');

console.log(`\n结果: ${pass} 通过, ${fail} 失败`);
A.ws.close(); B.ws.close();
process.exit(fail ? 1 : 0);
