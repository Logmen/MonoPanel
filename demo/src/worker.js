// demo.monopanel.app — живое демо панели. Каждый посетитель по кнопке
// «Открыть демо» получает свою MonoPanel в контейнере: настоящий API и Web UI
// поверх сервера-имитации (internal/demo, образ собирает demo/Dockerfile).
// Контейнер без выхода в интернет засыпает после 15 минут без запросов и
// живёт не дольше часа; следующий визит начинает с чистого демо.
import { Container, getContainer } from '@cloudflare/containers';

const SESSION = 'mp_demo';
const LANG = 'mp_demo_lang';
const LIFETIME = 60 * 60; // секунд

export class DemoPanel extends Container {
  defaultPort = 8080;
  sleepAfter = '15m';
  enableInternet = false;

  // Час на сессию, даже если в ней всё время что-то делают: демо не должно
  // становиться чьим-то постоянным сервером.
  async onStart() {
    await this.schedule(LIFETIME, 'expire');
  }

  async expire() {
    await this.destroy();
  }
}

// Язык — по тому же правилу, что у Web UI и monopanel.app: языки стран СНГ —
// русский, остальные — английский; выбор по ссылке запоминается в cookie.
const CIS = new Set(['ru', 'uk', 'be', 'kk', 'ky', 'uz', 'tg', 'tk', 'hy', 'az', 'ka', 'os', 'tt', 'ba']);

function browserLang(header) {
  const first = (header || '')
    .split(',')
    .map((part, i) => {
      const [tag, ...params] = part.trim().split(';');
      const q = params.map((p) => p.trim()).find((p) => p.startsWith('q='));
      return { tag: tag.trim().toLowerCase(), q: q ? Number(q.slice(2)) : 1, i };
    })
    .filter((l) => l.tag && l.tag !== '*' && l.q > 0)
    .sort((a, b) => b.q - a.q || a.i - b.i)[0];
  if (!first) return 'en';
  const [primary, region] = first.tag.split(/[-_]/);
  return CIS.has(primary) || (primary === 'ro' && region === 'md') ? 'ru' : 'en';
}

function cookie(request, name) {
  const m = new RegExp(`(?:^|;\\s*)${name}=([^;]*)`).exec(request.headers.get('Cookie') || '');
  return m ? m[1] : null;
}

function langOf(request, url) {
  const chosen = url.searchParams.get('lang') || cookie(request, LANG);
  return chosen === 'ru' || chosen === 'en' ? chosen : browserLang(request.headers.get('Accept-Language'));
}

// Идентификатор сессии — случайные 128 бит; им же назван контейнер.
function sessionOf(request) {
  const id = cookie(request, SESSION);
  return id && /^[0-9a-f]{32}$/.test(id) ? id : null;
}

const set = (name, value, maxAge) => `${name}=${value}; Path=/; Max-Age=${maxAge}; HttpOnly; Secure; SameSite=Lax`;

function redirect(location, cookies = []) {
  const headers = new Headers({ Location: location, 'Cache-Control': 'no-store' });
  for (const c of cookies) headers.append('Set-Cookie', c);
  return new Response(null, { status: 303, headers });
}

const NOTICES = {
  busy: {
    ru: 'Сейчас демо открыто у слишком многих посетителей. Попробуйте через пару минут.',
    en: 'Too many visitors have the demo open right now. Please try again in a couple of minutes.'
  },
  often: {
    ru: 'С этого адреса демо открывали слишком часто. Подождите минуту.',
    en: 'The demo has been started from this address too often. Please wait a minute.'
  },
  ended: {
    ru: 'Демо закрыто. Можно открыть новое — оно начнётся с чистого листа.',
    en: 'The demo is closed. You can open a new one; it starts from scratch.'
  }
};

// Страница с кнопкой — статическая (pages/<язык>.html), сюда только дописывается
// сообщение, если оно есть.
async function landing(request, env, url, notice, status = 200) {
  const lang = langOf(request, url);
  const page = await env.ASSETS.fetch(new Request(new URL(`/${lang}.html`, url), { method: 'GET' }));
  let res = new Response(page.body, { status, headers: page.headers });
  res.headers.set('Cache-Control', 'no-store');
  res.headers.set('Vary', 'Accept-Language, Cookie');
  if (url.searchParams.has('lang')) res.headers.append('Set-Cookie', set(LANG, lang, 365 * 86400));
  if (notice) {
    res = new HTMLRewriter()
      .on('#notice', {
        element(el) {
          el.removeAttribute('hidden');
          el.setInnerContent(NOTICES[notice][lang]);
        }
      })
      .transform(res);
  }
  return res;
}

// Плашка поверх панели. CSP панели разрешает только свои скрипты, поэтому
// «скрыть» сделано флажком и CSS, без JavaScript.
function banner(lang) {
  const t = {
    ru: ['Это демо: действия выполняет сервер-имитация, а не настоящий сервер. Через 15 минут без действий всё вернётся к началу.', 'Вход, если выйдете', 'Завершить демо', 'Скрыть'],
    en: ['This is a demo: a pretend server carries out the actions, not a real one. After 15 idle minutes everything starts over.', 'Sign-in if you sign out', 'End the demo', 'Hide']
  }[lang];
  return `<input type="checkbox" id="mp-demo-hide" hidden>
<div id="mp-demo" role="note" style="position:fixed;z-index:2147483000;left:50%;bottom:14px;transform:translateX(-50%);width:max-content;max-width:calc(100% - 24px);display:flex;flex-wrap:wrap;gap:6px 14px;align-items:center;padding:9px 12px 9px 14px;border:1px solid rgba(55,201,224,.45);background:rgba(7,17,28,.94);color:#d6e9f5;font:13px/1.4 system-ui,-apple-system,'Segoe UI',sans-serif;box-shadow:0 8px 28px rgba(0,0,0,.4)">
<span>${t[0]} ${t[1]}: <b>demo</b> / <b>monopanel-demo</b></span>
<a href="/end" style="color:#37c9e0;font-weight:600;white-space:nowrap">${t[2]}</a>
<a href="https://monopanel.app/" style="color:#37c9e0;white-space:nowrap">monopanel.app</a>
<label for="mp-demo-hide" title="${t[3]}" aria-label="${t[3]}" style="cursor:pointer;padding:0 4px;color:#8fb3c9;font-size:16px;line-height:1">×</label>
</div>
<style>#mp-demo-hide:checked + #mp-demo { display: none !important; }</style>`;
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    const session = sessionOf(request);

    if (url.pathname === '/start') {
      if (request.method !== 'POST') return redirect('/');
      const ip = request.headers.get('CF-Connecting-IP') || 'unknown';
      const { success } = await env.STARTS.limit({ key: ip });
      if (!success) return landing(request, env, url, 'often', 429);
      const id = crypto.randomUUID().replaceAll('-', '');
      try {
        await getContainer(env.DEMO, id).startAndWaitForPorts();
      } catch (err) {
        console.error('demo start', err);
        return landing(request, env, url, 'busy', 503);
      }
      return redirect('/', [set(SESSION, id, LIFETIME), set(LANG, langOf(request, url), 365 * 86400)]);
    }

    if (url.pathname === '/end') {
      if (session) {
        try {
          await getContainer(env.DEMO, session).destroy();
        } catch (err) {
          console.error('demo end', err);
        }
      }
      return redirect('/?ended', [set(SESSION, '', 0)]);
    }

    if (!session) {
      if (request.method === 'GET' && !/\.[a-z0-9]+$/i.test(url.pathname) && !url.pathname.startsWith('/api/')) {
        const notice = url.searchParams.has('ended') ? 'ended' : url.searchParams.has('busy') ? 'busy' : null;
        return landing(request, env, url, notice);
      }
      // Стили и шрифты страницы с кнопкой; API без сессии — нечего открывать.
      return url.pathname.startsWith('/api/') ? new Response('no demo session', { status: 401 }) : env.ASSETS.fetch(request);
    }

    let res;
    try {
      res = await getContainer(env.DEMO, session).fetch(request);
    } catch (err) {
      console.error('demo fetch', err);
      return redirect('/?busy', [set(SESSION, '', 0)]);
    }
    if ((res.headers.get('Content-Type') || '').startsWith('text/html')) {
      const lang = langOf(request, url);
      res = new HTMLRewriter()
        .on('body', {
          element(el) {
            el.append(banner(lang), { html: true });
          }
        })
        .transform(res);
    }
    return res;
  }
};
