import { api } from './api';

export type Principal = { user_id?: number; login: string; role: string; via: string };

export const auth = $state<{ me: Principal | null; loading: boolean; hostname: string; version: string }>({ me: null, loading: true, hostname: '', version: '' });

export async function loadMe() {
  try {
    auth.me = await api<Principal>('/auth/me');
  } catch {
    auth.me = null;
  } finally {
    auth.loading = false;
  }
  try {
    const h = await api<{ version: string }>('/health');
    auth.version = h.version;
  } catch {
    /* ignore */
  }
}

export async function logout() {
  try {
    await api('/auth/logout', { method: 'POST' });
  } catch {
    /* ignore */
  }
  auth.me = null;
}

/* ── toasts ─────────────────────────────────────────────────────────────── */
export const toastValue = $state({ text: '', kind: 'ok' as 'ok' | 'err', visible: false });
let toastTimer: ReturnType<typeof setTimeout> | undefined;
export function notify(text: string, kind: 'ok' | 'err' = 'ok') {
  toastValue.text = text;
  toastValue.kind = kind;
  toastValue.visible = true;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (toastValue.visible = false), kind === 'err' ? 7000 : 4000);
}

/* ── theme: system by default, light/dark when the user picks one ───────── */
export type ThemeMode = 'system' | 'light' | 'dark';
export const theme = $state<{ mode: ThemeMode }>({ mode: 'system' });

function applyTheme() {
  const el = document.documentElement;
  if (theme.mode === 'system') delete el.dataset.theme;
  else el.dataset.theme = theme.mode;
}
export function initTheme() {
  try {
    const t = localStorage.getItem('theme');
    if (t === 'light' || t === 'dark') theme.mode = t;
  } catch {
    /* private mode */
  }
  applyTheme();
}
export function setTheme(mode: ThemeMode) {
  theme.mode = mode;
  try {
    if (mode === 'system') localStorage.removeItem('theme');
    else localStorage.setItem('theme', mode);
  } catch {
    /* ignore */
  }
  applyTheme();
}

/* ── motion: honour the OS preference in Svelte transitions ─────────────── */
export const motion = { fast: 160, base: 240 };
export function dur(ms: number): number {
  try {
    return matchMedia('(prefers-reduced-motion: reduce)').matches ? 0 : ms;
  } catch {
    return ms;
  }
}
