// Страница демо в dist/: pages/<язык>.html и оформление monopanel.app из
// site/public — стили, шрифты и значок одни на сайт и демо.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const DIR = path.dirname(fileURLToPath(import.meta.url));
const SITE = path.join(DIR, '..', 'site', 'public');
const OUT = path.join(DIR, 'dist');

fs.rmSync(OUT, { recursive: true, force: true });
fs.mkdirSync(path.join(OUT, 'assets'), { recursive: true });
for (const f of ['assets/fonts.css', 'assets/site.css', 'favicon.svg', 'favicon.ico']) fs.copyFileSync(path.join(SITE, f), path.join(OUT, f));
fs.cpSync(path.join(SITE, 'fonts'), path.join(OUT, 'fonts'), { recursive: true });
fs.cpSync(path.join(DIR, 'pages'), OUT, { recursive: true });
console.log('dist: ru.html, en.html and the monopanel.app styles');
