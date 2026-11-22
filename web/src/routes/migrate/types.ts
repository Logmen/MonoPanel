// Shapes of the migration API: apitypes.MigrationPlan / MigrationBundle /
// MigrationGrantResponse and the store rows a bundle carries. Only the fields
// the page shows are named; a foreign source (FASTPANEL, BitrixVM) leaves many
// lists out entirely, so every list may come as null.

export type Panel = 'monopanel' | 'fastpanel' | 'bitrixvm';

export type Issue = { kind: string; target?: string; text: string; fix?: string };

export type PlanUser = { login: string; email?: string; shell: boolean; quota_mb?: number | null; home?: string; unix_uid?: number | null };

export type PlanSite = {
  domain: string;
  aliases: string[] | null;
  mode: string;
  php_version: string;
  backend?: string;
  docroot: string;
  ssl: string;
  redirect_https: boolean;
  http3: boolean;
  preset: string;
  cms: string;
  cms_version?: string;
  session_store: string;
  allow_from: string[] | null;
  rate_limit: number;
  security_headers: boolean;
};

export type PlanDatabase = { name: string; charset: string; collation: string; size_bytes: number; users?: { name: string; host: string; auth_plugin: string }[] | null };

export type PlanCron = { schedule: string; command: string; enabled: boolean; php_version?: string; comment?: string };

export type PlanApp = { name: string; description?: string; command: string; workdir: string; env_file?: string; env: string[] | null; restart: string };

export type PlanValkey = { purpose: string; memory_mb: number };

export type PlanMailDomain = { id: number; name: string; active: boolean; dkim_selector?: string; lenient: boolean; send_only: boolean };

export type PlanMailbox = { domain_id: number; address: string; name?: string; quota_mb: number; active: boolean; size_bytes: number };

export type PlanAlias = { domain_id: number; address: string; destination: string; active: boolean };

export type PlanCert = { name: string; names: string[] | null; kind: string; auto_renew: boolean; not_after?: string; files: boolean };

export type Bundle = {
  panel: string;
  hostname: string;
  family: string;
  scope: string;
  generated: string;
  user: PlanUser;
  sites: PlanSite[] | null;
  site_nginx?: Record<string, string> | null;
  databases: PlanDatabase[] | null;
  cron: PlanCron[] | null;
  apps: PlanApp[] | null;
  valkey?: PlanValkey[] | null;
  mail_domains: PlanMailDomain[] | null;
  mailboxes: PlanMailbox[] | null;
  mail_aliases: PlanAlias[] | null;
  certificates: PlanCert[] | null;
  sizes: { files_bytes: number; mail_bytes: number; databases?: Record<string, number> | null };
  notes?: string[] | null;
};

export type Plan = { bundle: Bundle; login: string; conflicts: Issue[] | null; warnings: Issue[] | null; ok: boolean };

export type Grant = { token: string; scope: string; expires_at: string; command: string };
