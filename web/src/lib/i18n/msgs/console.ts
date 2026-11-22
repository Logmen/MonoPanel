import { frag } from '../frag';

export const console = frag({
  en: {
    'console.title': 'Console',
    'console.sub': 'mp commands as {login}: the same binary and the same permissions as over SSH; output arrives as the command runs',
    'console.admin': 'administrator',
    'console.clear': 'Clear',
    'console.empty': 'Type a command, for example doctor, or press a button above.',
    'console.abort': 'Abort',
    'console.run': 'Run',
    'console.aborted': '[aborted]',
    'console.hint': 'Service commands (api, agent, helper, fsop, setup), commands that read or write files on the server (files get/put, ssl import, ssl panel import, site nginx --set, migrate --key) and update trust are not available from the console — the panel’s pages do the same. --server and --token are supplied by the panel itself, every command gets a one-time token, and passwords typed here reach the command on stdin, not on its command line.'
  },
  ru: {
    'console.title': 'Консоль',
    'console.sub': 'команды mp от имени {login}: тот же бинарник и те же права, что по ssh; вывод приходит по мере выполнения',
    'console.admin': 'администратора',
    'console.clear': 'очистить',
    'console.empty': 'Введите команду, например doctor, или нажмите кнопку выше.',
    'console.abort': 'прервать',
    'console.run': 'выполнить',
    'console.aborted': '[прервано]',
    'console.hint': 'Служебные команды (api, agent, helper, fsop, setup), команды, которые читают или пишут файлы на сервере (files get/put, ssl import, ssl panel import, site nginx --set, migrate --key), и update trust из консоли недоступны — то же делают страницы панели. --server и --token подставляет сама панель, каждая команда получает одноразовый токен, а введённые здесь пароли команда получает через stdin, а не в командной строке.'
  }
});
