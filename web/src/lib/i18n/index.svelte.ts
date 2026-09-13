import { browser } from '$app/environment';
import { detectLocale, type Locale } from './detect';
import { common } from './msgs/common';
import { shell } from './msgs/shell';
import { dashboard } from './msgs/dashboard';
import { console as consolePage } from './msgs/console';
import { jobs } from './msgs/jobs';
import { mail } from './msgs/mail';
import { files } from './msgs/files';
import { settings } from './msgs/settings';
import { site } from './msgs/site';
import { users } from './msgs/users';
import { ssl } from './msgs/ssl';
import { stack } from './msgs/stack';
import { php } from './msgs/php';
import { backups } from './msgs/backups';
import { databases } from './msgs/databases';
import { sites } from './msgs/sites';
import { firewall } from './msgs/firewall';

export type { Locale } from './detect';
export type LangMode = 'auto' | Locale;

const en = {
  ...common.en, ...shell.en, ...dashboard.en, ...consolePage.en, ...jobs.en, ...mail.en, ...files.en, ...settings.en,
  ...site.en, ...users.en, ...ssl.en, ...stack.en, ...php.en, ...backups.en, ...databases.en, ...sites.en, ...firewall.en
};
export type MsgKey = keyof typeof en;
const ru: Record<MsgKey, string> = {
  ...common.ru, ...shell.ru, ...dashboard.ru, ...consolePage.ru, ...jobs.ru, ...mail.ru, ...files.ru, ...settings.ru,
  ...site.ru, ...users.ru, ...ssl.ru, ...stack.ru, ...php.ru, ...backups.ru, ...databases.ru, ...sites.ru, ...firewall.ru
};
const dict: Record<Locale, Record<string, string>> = { en, ru };

/* ── language: auto by default (CIS → ru, otherwise en), or the one picked ─ */
export const lang = $state<{ mode: LangMode; locale: Locale }>({ mode: 'auto', locale: 'ru' });

function detected(): Locale {
  if (!browser) return 'ru';
  const list = navigator.languages?.length ? navigator.languages : [navigator.language];
  return detectLocale(list);
}
function applyLang() {
  lang.locale = lang.mode === 'auto' ? detected() : lang.mode;
  if (browser) document.documentElement.lang = lang.locale;
}
export function initLang() {
  try {
    const v = localStorage.getItem('lang');
    if (v === 'ru' || v === 'en') lang.mode = v;
  } catch {
    /* private mode */
  }
  applyLang();
}
export function setLang(mode: LangMode) {
  lang.mode = mode;
  try {
    if (mode === 'auto') localStorage.removeItem('lang');
    else localStorage.setItem('lang', mode);
  } catch {
    /* private mode */
  }
  applyLang();
}

/* ── t: the message for a key in the current language; {name} is a variable ─
   Reading lang.locale here is what makes every `{t('…')}` in a template
   re-render when the language changes — call t() from templates, $derived
   or event handlers, never into a plain const at module init. */
export function t(key: MsgKey, vars?: Record<string, string | number>): string {
  let s = dict[lang.locale][key] ?? en[key] ?? key;
  if (vars) for (const [k, v] of Object.entries(vars)) s = s.split('{' + k + '}').join(String(v));
  return s;
}

/* ── tn: plural forms — keys `<base>.one`, `.few`, `.many`, `.other`; {n} is the number ─ */
export function tn(base: string, n: number, vars?: Record<string, string | number>): string {
  const form = new Intl.PluralRules(lang.locale === 'ru' ? 'ru' : 'en').select(n);
  const table = dict[lang.locale];
  const key = `${base}.${form}` in table ? `${base}.${form}` : `${base}.other`;
  return t(key as MsgKey, { n, ...vars });
}

/* ── dates and numbers follow the language ─ */
export function dateLocale(): string {
  return lang.locale === 'ru' ? 'ru-RU' : 'en-GB';
}
