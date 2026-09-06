export class ApiError extends Error {
  status: number;
  detail: string;
  errors?: { message: string; location?: string }[];
  constructor(status: number, detail: string, errors?: { message: string; location?: string }[]) {
    super(detail);
    this.status = status;
    this.detail = detail;
    this.errors = errors;
  }
  get text(): string {
    if (this.errors?.length) return this.detail + ': ' + this.errors.map((e) => (e.location ? e.location.replace(/^body\./, '') + ' — ' : '') + e.message).join('; ');
    return this.detail;
  }
}

export async function api<T = unknown>(path: string, init: RequestInit & { json?: unknown } = {}): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json', ...((init.headers as Record<string, string>) || {}) };
  let body = init.body;
  if (init.json !== undefined) {
    headers['Content-Type'] = 'application/json';
    body = JSON.stringify(init.json);
  }
  const res = await fetch('/api/v1' + path, { ...init, headers, body, credentials: 'same-origin' });
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  let data: any = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = text;
  }
  if (!res.ok) throw new ApiError(res.status, data?.detail || data?.title || res.statusText, data?.errors);
  return data as T;
}

export function jobEvents(id: number, onEvent: (type: string, data: any) => void): () => void {
  const es = new EventSource(`/api/v1/jobs/${id}/events`);
  for (const t of ['snapshot', 'queued', 'started', 'progress', 'log', 'done', 'failed', 'ping']) {
    es.addEventListener(t, (e) => {
      try {
        onEvent(t, JSON.parse((e as MessageEvent).data));
      } catch {
        /* ignore */
      }
      if (t === 'done' || t === 'failed') es.close();
    });
  }
  es.onerror = () => {};
  return () => es.close();
}

export function bytes(n: number | undefined | null): string {
  if (!n) return '0 Б';
  const units = ['Б', 'КБ', 'МБ', 'ГБ', 'ТБ'];
  let f = n;
  let i = 0;
  while (f >= 1024 && i < units.length - 1) {
    f /= 1024;
    i++;
  }
  return f.toFixed(i === 0 ? 0 : 1) + ' ' + units[i];
}

export function when(s: string | null | undefined): string {
  if (!s) return '—';
  const d = new Date(s);
  return isNaN(d.getTime()) ? s : d.toLocaleString('ru-RU', { dateStyle: 'short', timeStyle: 'short' });
}

export function daysLeft(s: string | null | undefined): string {
  if (!s) return '—';
  const d = Math.round((new Date(s).getTime() - Date.now()) / 86400000);
  return `${new Date(s).toLocaleDateString('ru-RU')} (${d} дн.)`;
}
