# Monaco (редактор VS Code), урезанная AMD-сборка

Источник: `monaco-editor@0.56.0` из npm, каталог `min/vs`. Лицензия MIT — см. `LICENSE.txt`.

Здесь лежит не весь пакет (24 МБ), а то, что нужно панели (4,7 МБ):

* ядро редактора, загрузчик AMD и тема;
* режимы подсветки: php, html, css, javascript, typescript, json, xml, ini,
  shell, sql, yaml, markdown, python, apache, dockerfile;
* `assets/editor.worker-*.js` — рабочий поток самого редактора.

Не включено намеренно: языковые службы json/css/html/typescript
(`assets/*.worker-*.js`, 9 МБ — это IntelliSense, панели он не нужен),
переводы интерфейса (`nls/lang`, 1,7 МБ) и режимы остальных 75 языков.

## Как обновить

```sh
ver=0.57.0
curl -sL "https://registry.npmjs.org/monaco-editor/-/monaco-editor-$ver.tgz" | tar xz
# скопировать из package/min/vs файлы по списку выше; имена чанков содержат
# хеш и меняются от версии к версии, поэтому проще свериться с этим каталогом
```

После обновления проверьте, что редактор открывается и в консоли нет 404:
имена хешированных чанков меняются, и пропущенный файл ломает загрузку целиком.
