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
    'console.hint': 'Service commands (api, agent, helper, fsop, setup) are not available from the console; --server and --token are supplied by the panel itself, and every command gets a one-time token.'
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
    'console.hint': 'Служебные команды (api, agent, helper, fsop, setup) из консоли недоступны; --server и --token подставляет сама панель, каждая команда получает одноразовый токен.'
  }
});
