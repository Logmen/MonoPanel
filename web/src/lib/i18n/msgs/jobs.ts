import { frag } from '../frag';

export const jobs = frag({
  en: {
    'jobs.title': 'Jobs',
    'jobs.sub': 'Queue of asynchronous operations: installation, applying configurations, certificates, backups',
    'jobs.filterAll': 'All',
    'jobs.filterRunning': 'Running',
    'jobs.filterFailed': 'Failed',
    'jobs.filterDone': 'Done',
    'jobs.colType': 'Type',
    'jobs.colProgress': 'Progress',
    'jobs.colWho': 'Requested by',
    'jobs.colCreated': 'Created',
    'jobs.colMessage': 'Message',
    'jobs.empty': 'No jobs.'
  },
  ru: {
    'jobs.title': 'Задачи',
    'jobs.sub': 'очередь асинхронных операций: установка, применение конфигураций, сертификаты, бэкапы',
    'jobs.filterAll': 'все',
    'jobs.filterRunning': 'идут',
    'jobs.filterFailed': 'ошибки',
    'jobs.filterDone': 'готово',
    'jobs.colType': 'Тип',
    'jobs.colProgress': 'Прогресс',
    'jobs.colWho': 'Кто',
    'jobs.colCreated': 'Создана',
    'jobs.colMessage': 'Сообщение',
    'jobs.empty': 'Задач нет.'
  }
});
