import { ApiError } from '$lib/api';

// A panel account as GET /users and GET /users/{login} return it (store.User).
export type Account = {
  id: number;
  login: string;
  role: string;
  email?: string;
  unix_uid?: number;
  unix_gid?: number;
  home?: string;
  shell: boolean;
  status: string;
  quota_mb?: number;
  created_at: string;
  updated_at: string;
};

/** The message of a failed call, worded by the API. */
export const errText = (e: unknown): string => (e instanceof ApiError ? e.text : String(e));

/** systemd reports a unit that has not run since boot with the zero time. */
export const realTime = (s?: string | null): string => (s && !s.startsWith('0001-') ? s : '');

/** Accounts that can own app services and Valkey: active, with a Unix user.
 *  Cron needs only the Unix user (a suspended account keeps its crontab), so
 *  it passes activeOnly = false. */
export const withUnix = (list: Account[] | null | undefined, activeOnly = true): Account[] =>
  (list ?? []).filter((a) => a.unix_uid != null && (activeOnly ? a.status === 'active' : a.status !== 'deleting'));

// A password made in the browser, the counterpart of `mp user add|set --generate`:
// 20 characters from an alphabet without look-alikes, from the system CSPRNG.
const alphabet = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789';
export function genPassword(n = 20): string {
  const out: string[] = [];
  const buf = new Uint8Array(1);
  // Rejection sampling keeps every character equally likely.
  while (out.length < n) {
    crypto.getRandomValues(buf);
    if (buf[0] < 256 - (256 % alphabet.length)) out.push(alphabet[buf[0] % alphabet.length]);
  }
  return out.join('');
}

/** KEY=value lines of a text area → the API's env list; blank lines and # comments are skipped. */
export const envList = (text: string): string[] =>
  text
    .split('\n')
    .map((s) => s.trim())
    .filter((s) => s && !s.startsWith('#'));

/** Comma-separated labels → a list (token scopes), as `mp token create --scopes a,b`:
 *  a label may contain spaces, so only commas (and line breaks) separate them. */
export const scopeList = (text: string): string[] =>
  text
    .split(/[,\n]/)
    .map((s) => s.trim())
    .filter(Boolean);

export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
}
