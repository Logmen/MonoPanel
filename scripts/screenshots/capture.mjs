// Скриншоты Web UI для документации: headless Chrome по DevTools-протоколу, без
// зависимостей (нужны Node 22+ и Chrome). Панель открывается по имени из примеров —
// panel.example.com подменяется на адрес машины, — вход по API-токену заголовком,
// каждая страница снимается в светлой и тёмной теме: docs/img/<имя>.webp и <имя>.dark.webp.
//
//   node scripts/screenshots/capture.mjs --ssh mp-ubuntu2404 [имя...]
//   MP_TOKEN=<токен> node scripts/screenshots/capture.mjs <адрес> [имя...]
//
// С --ssh токен выпускается на машине на время съёмки и потом отзывается, адрес
// берётся из ssh-конфига.
import { execFileSync, spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const argv = process.argv.slice(2);
let alias = null;
if (argv[0] === '--ssh') alias = argv.splice(0, 2)[1];
const ssh = (cmd) => execFileSync('ssh', ['-o', 'BatchMode=yes', alias, cmd], { encoding: 'utf8' });
const ip = alias ? execFileSync('ssh', ['-G', alias], { encoding: 'utf8' }).match(/^hostname (\S+)$/m)[1] : argv.shift();
let token = process.env.MP_TOKEN;
let tokenId = null;
if (alias) ({ token, record: { id: tokenId } } = JSON.parse(ssh('mp token create --name screenshots --expires 1 --json')));
const only = argv;
if (!ip || !token) {
  console.error('usage: node capture.mjs --ssh <alias> [shot...] | MP_TOKEN=<token> node capture.mjs <address> [shot...]');
  process.exit(2);
}
const HOST = 'panel.example.com';
const BASE = `https://${HOST}:8443`;
const OUT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../docs/img');
const CHROME = process.env.CHROME || 'google-chrome-stable';
const WIDTH = 1440;
const HEIGHT = 900;

// Что снимать: адрес, необязательный шаг перед снимком (JS в странице) и вход
// без токена для экрана логина.
const SHOTS = [
  { name: 'login', path: '/', anonymous: true },
  // Машина для снимков живёт недолго: за час на графиках видно больше, чем за сутки.
  { name: 'dashboard', path: '/', step: clickText('button', '1h') },
  { name: 'sites', path: '/sites' },
  { name: 'site', path: '/sites/example.com' },
  { name: 'php', path: '/php' },
  { name: 'databases', path: '/databases' },
  { name: 'mail', path: '/mail' },
  { name: 'files', path: '/files?user=alex&path=/data/www/example.com' },
  { name: 'editor', path: '/files?user=alex&path=/data/www/example.com', step: clickText('button', 'wp-cron.php'), settle: 4000 },
  { name: 'jobs', path: '/jobs', step: clickRow('site.cms') },
  { name: 'firewall', path: '/firewall' },
  { name: 'backups', path: '/backups' },
  { name: 'users', path: '/users' },
  { name: 'settings', path: '/settings' }
];

function clickText(selector, text) {
  return `(() => { const el = [...document.querySelectorAll(${JSON.stringify(selector)})].find((e) => e.textContent.trim() === ${JSON.stringify(text)}); el?.click(); return !!el; })()`;
}
function clickRow(type) {
  return `(() => { const row = [...document.querySelectorAll('tbody tr')].find((r) => r.children[1]?.textContent.trim() === ${JSON.stringify(type)}); row?.click(); return !!row; })()`;
}

// ── DevTools-протокол ──────────────────────────────────────────────────────
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'mpshots-'));
const chrome = spawn(CHROME, [
  '--headless=new',
  '--remote-debugging-port=0',
  `--user-data-dir=${profile}`,
  '--ignore-certificate-errors',
  `--host-resolver-rules=MAP ${HOST} ${ip}`,
  '--hide-scrollbars',
  '--lang=ru',
  '--no-first-run',
  `--window-size=${WIDTH},${HEIGHT}`,
  'about:blank'
]);
const wsUrl = await new Promise((resolve, reject) => {
  let err = '';
  chrome.stderr.on('data', (d) => {
    err += d;
    const m = err.match(/DevTools listening on (ws:\/\/\S+)/);
    if (m) resolve(m[1]);
  });
  chrome.on('exit', (code) => reject(new Error(`chrome exited ${code}: ${err}`)));
});

const ws = new WebSocket(wsUrl);
await new Promise((r) => ws.addEventListener('open', r, { once: true }));
let seq = 0;
const pending = new Map();
const waiters = [];
ws.addEventListener('message', (ev) => {
  const msg = JSON.parse(ev.data);
  if (msg.id && pending.has(msg.id)) {
    const { resolve, reject } = pending.get(msg.id);
    pending.delete(msg.id);
    msg.error ? reject(new Error(msg.error.message)) : resolve(msg.result);
  } else if (msg.method) {
    for (const w of [...waiters]) if (w.method === msg.method) { waiters.splice(waiters.indexOf(w), 1); w.resolve(msg.params); }
  }
});
const send = (method, params = {}, sessionId) =>
  new Promise((resolve, reject) => {
    const id = ++seq;
    pending.set(id, { resolve, reject });
    ws.send(JSON.stringify({ id, method, params, sessionId }));
  });
const once = (method) => new Promise((resolve) => waiters.push({ method, resolve }));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// Каждый снимок — в своей вкладке: так заголовок авторизации и тема не протекают между ними.
async function capture(shot, theme) {
  const { targetId } = await send('Target.createTarget', { url: 'about:blank' });
  const { sessionId } = await send('Target.attachToTarget', { targetId, flatten: true });
  const s = (m, p) => send(m, p, sessionId);
  await s('Page.enable');
  await s('Network.enable');
  await s('Emulation.setDeviceMetricsOverride', { width: WIDTH, height: HEIGHT, deviceScaleFactor: 1, mobile: false });
  await s('Emulation.setEmulatedMedia', { features: [{ name: 'prefers-color-scheme', value: theme }, { name: 'prefers-reduced-motion', value: 'reduce' }] });
  await s('Page.addScriptToEvaluateOnNewDocument', { source: "localStorage.setItem('lang','ru');localStorage.removeItem('theme')" });
  if (!shot.anonymous) await s('Network.setExtraHTTPHeaders', { headers: { Authorization: `Bearer ${token}` } });
  const loaded = once('Page.loadEventFired');
  await s('Page.navigate', { url: BASE + shot.path });
  await loaded;
  await sleep(2500);
  if (shot.step) {
    const { result } = await s('Runtime.evaluate', { expression: shot.step, returnByValue: true });
    if (!result.value) console.warn(`  ${shot.name}: шаг перед снимком ничего не нашёл`);
    await sleep(shot.settle ?? 1500);
  }
  const { data } = await s('Page.captureScreenshot', { format: 'webp', quality: 88 });
  const file = path.join(OUT, `${shot.name}${theme === 'dark' ? '.dark' : ''}.webp`);
  fs.writeFileSync(file, Buffer.from(data, 'base64'));
  await send('Target.closeTarget', { targetId });
  return file;
}

fs.mkdirSync(OUT, { recursive: true });
try {
  for (const shot of SHOTS.filter((x) => !only.length || only.includes(x.name))) {
    for (const theme of ['light', 'dark']) {
      const file = await capture(shot, theme);
      console.log(`${path.relative(process.cwd(), file)}  ${(fs.statSync(file).size / 1024).toFixed(0)} KB`);
    }
  }
} finally {
  ws.close();
  const exited = new Promise((r) => chrome.once('exit', r));
  chrome.kill();
  await exited;
  fs.rmSync(profile, { recursive: true, force: true, maxRetries: 5, retryDelay: 200 });
  if (tokenId) ssh(`mp token revoke ${tokenId}`);
}
