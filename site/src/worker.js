// Язык сайта. Русские страницы живут по прежним адресам, английские — те же
// адреса под /en/. Кому какие показывать, решает то же правило, что у Web UI
// панели (web/src/lib/i18n/detect.ts): первый язык браузера из стран СНГ —
// русский, любой другой — английский. Выбор в переключателе (?lang=ru|en)
// запоминается в cookie и побеждает язык браузера. Поисковые роботы и клиенты
// без Accept-Language не перенаправляются: другую версию страницы они находят
// по hreflang. Воркер запускается только для страниц (run_worker_first в
// wrangler.jsonc) — стили, картинки и install.sh отдаются как статика.

const CIS = new Set(['ru', 'uk', 'be', 'kk', 'ky', 'uz', 'tg', 'tk', 'hy', 'az', 'ka', 'os', 'tt', 'ba']);
const BOT = /bot\b|bot\/|crawl|spider|slurp|facebookexternalhit|embedly|preview|lighthouse|pagespeed/i;
const YEAR = 365 * 24 * 3600;

// langOf maps a language tag to the site's language.
function langOf(tag) {
  const [primary, region] = tag.toLowerCase().split(/[-_]/);
  return CIS.has(primary) || (primary === 'ro' && region === 'md') ? 'ru' : 'en';
}

// browserLang is the site's language for the browser's preferred language,
// or null when the request names none.
export function browserLang(header) {
  const langs = (header || '')
    .split(',')
    .map((part, i) => {
      const [tag, ...params] = part.trim().split(';');
      const q = params.map((p) => p.trim()).find((p) => p.startsWith('q='));
      return { tag: tag.trim(), q: q ? Number(q.slice(2)) : 1, i };
    })
    .filter((l) => l.tag && l.tag !== '*' && l.q > 0)
    .sort((a, b) => b.q - a.q || a.i - b.i);
  return langs.length ? langOf(langs[0].tag) : null;
}

function cookieLang(header) {
  const m = /(?:^|;\s*)lang=(ru|en)(?:;|$)/.exec(header || '');
  return m ? m[1] : null;
}

// preferred is the language this visitor should see, or null to leave the
// requested page as it is.
export function preferred(request) {
  const chosen = cookieLang(request.headers.get('Cookie'));
  if (chosen) return chosen;
  if (BOT.test(request.headers.get('User-Agent') || '')) return null;
  return browserLang(request.headers.get('Accept-Language'));
}

const isEnglish = (path) => path === '/en' || path.startsWith('/en/');

// counterpart is the same page in the other language.
export function counterpart(path, lang) {
  if (lang === 'en') return isEnglish(path) ? path : '/en' + path;
  return isEnglish(path) ? path.slice(3) || '/' : path;
}

function redirect(location, extra = {}) {
  return new Response(null, {
    status: 302,
    headers: { Location: location, Vary: 'Accept-Language, Cookie', 'Cache-Control': 'private, no-store', ...extra }
  });
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    const page = !/\.[a-z0-9]+$/i.test(url.pathname);
    if (page && (request.method === 'GET' || request.method === 'HEAD')) {
      const chosen = url.searchParams.get('lang');
      if (chosen === 'ru' || chosen === 'en') {
        url.searchParams.delete('lang');
        return redirect(counterpart(url.pathname, chosen) + url.search + url.hash, {
          'Set-Cookie': `lang=${chosen}; Path=/; Max-Age=${YEAR}; SameSite=Lax; Secure`
        });
      }
      const want = preferred(request);
      if (want && (want === 'en') !== isEnglish(url.pathname)) return redirect(counterpart(url.pathname, want) + url.search);
      // Тот же адрес другому посетителю может ответить переадресацией: кешам — по какому признаку.
      const res = await env.ASSETS.fetch(request);
      const out = new Response(res.body, res);
      out.headers.append('Vary', 'Accept-Language, Cookie');
      return out;
    }
    return env.ASSETS.fetch(request);
  }
};
