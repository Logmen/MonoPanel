import { frag } from '../frag';

export const audit = frag({
  en: {
    'audit.title': 'Audit log',
    'audit.sub': 'Who signed in and from where, who created tokens, who changed what',
    'audit.filterAll': 'All',
    'audit.filterAuth': 'Sign-ins',
    'audit.filterTokens': 'Tokens',
    'audit.filterDenied': 'Refusals',
    'audit.colTime': 'Time',
    'audit.colWho': 'Who',
    'audit.colAction': 'Action',
    'audit.colTarget': 'Target',
    'audit.colIP': 'IP',
    'audit.colResult': 'Result',
    'audit.colDetails': 'Details',
    'audit.empty': 'No entries.',
    'audit.file': 'The same log is written to /var/log/monopanel/audit.log as JSON lines; refused sign-ins are also in the journal: journalctl -u monopanel-api.'
  },
  ru: {
    'audit.title': 'Журнал действий',
    'audit.sub': 'кто и откуда входил, кто выпускал токены, кто что менял',
    'audit.filterAll': 'все',
    'audit.filterAuth': 'входы',
    'audit.filterTokens': 'токены',
    'audit.filterDenied': 'отказы',
    'audit.colTime': 'Время',
    'audit.colWho': 'Кто',
    'audit.colAction': 'Действие',
    'audit.colTarget': 'Объект',
    'audit.colIP': 'IP',
    'audit.colResult': 'Результат',
    'audit.colDetails': 'Подробности',
    'audit.empty': 'Записей нет.',
    'audit.file': 'Тот же журнал пишется в /var/log/monopanel/audit.log строками JSON; отказы во входе есть и в системном журнале: journalctl -u monopanel-api.'
  }
});
