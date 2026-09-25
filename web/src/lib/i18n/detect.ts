// Which language to show when the person has not chosen one: Russian for a
// browser set to a language of the CIS, English for everything else. Only
// the first (preferred) language decides — that is the interface language.
// The same rule is inlined in src/app.html so <html lang> is right before
// the app boots, and repeated by the pages nginx serves for a new or a
// suspended site (templates/site/index.html.tmpl,
// templates/nginx/site-suspended.conf.tmpl) and by the monopanel.app worker
// (site/src/worker.js); keep the lists identical.
export type Locale = 'ru' | 'en';

const CIS = new Set(['ru', 'uk', 'be', 'kk', 'ky', 'uz', 'tg', 'tk', 'hy', 'az', 'ka', 'os', 'tt', 'ba']);

export function detectLocale(languages: readonly string[] | undefined | null): Locale {
  const first = (languages ?? []).find((l) => typeof l === 'string' && l.trim() !== '');
  if (!first) return 'en';
  const [primary, region] = first.trim().toLowerCase().split(/[-_]/);
  if (CIS.has(primary)) return 'ru';
  if (primary === 'ro' && region === 'md') return 'ru'; // Moldova
  return 'en';
}
