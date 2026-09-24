// Сайт MonoPanel: тема как в панели, вкладки с командами, копирование,
// версия последнего релиза и подсветка раздела в меню.
(() => {
  const root = document.documentElement;

  // Тема: как в системе по умолчанию, светлая или тёмная — если выбрали.
  // Ключ тот же, что у панели (web/src/lib/state.svelte.ts).
  const modes = document.querySelectorAll('[data-theme-mode]');
  const paint = () => {
    const mode = root.dataset.theme || 'system';
    modes.forEach((b) => b.setAttribute('aria-pressed', String(b.dataset.themeMode === mode)));
  };
  modes.forEach((b) =>
    b.addEventListener('click', () => {
      const mode = b.dataset.themeMode;
      if (mode === 'system') delete root.dataset.theme;
      else root.dataset.theme = mode;
      try {
        if (mode === 'system') localStorage.removeItem('theme');
        else localStorage.setItem('theme', mode);
      } catch {
        /* приватное окно: выбор просто не запомнится */
      }
      paint();
    })
  );
  paint();

  const toast = document.getElementById('toast');
  let toastTimer;
  const notify = (text) => {
    toast.textContent = text;
    toast.classList.add('on');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => toast.classList.remove('on'), 1800);
  };

  // Вкладки с командами: стрелки влево-вправо переключают, как в обычном tablist.
  const tabs = [...document.querySelectorAll('[role="tab"]')];
  const select = (tab, focus) => {
    tabs.forEach((t) => {
      const on = t === tab;
      t.setAttribute('aria-selected', String(on));
      t.tabIndex = on ? 0 : -1;
      document.getElementById(t.getAttribute('aria-controls')).classList.toggle('off', !on);
    });
    if (focus) tab.focus();
  };
  tabs.forEach((t, i) => {
    t.addEventListener('click', () => select(t));
    t.addEventListener('keydown', (e) => {
      const step = e.key === 'ArrowRight' ? 1 : e.key === 'ArrowLeft' ? -1 : 0;
      if (!step) return;
      e.preventDefault();
      select(tabs[(i + step + tabs.length) % tabs.length], true);
    });
  });

  // Копируются команды открытой вкладки — по одной на строку, без приглашения «$».
  document.querySelector('[data-copy]')?.addEventListener('click', async () => {
    const lines = [...document.querySelectorAll('[role="tabpanel"]:not(.off) .ln')].map((ln) => {
      const copy = ln.cloneNode(true);
      copy.querySelector('.p')?.remove();
      return copy.textContent;
    });
    try {
      await navigator.clipboard.writeText(lines.join('\n') + '\n');
      notify('Команды скопированы');
    } catch {
      // Буфер недоступен (не HTTPS, запрет браузера): выделяем команды, «$» в выделение не попадёт.
      getSelection().selectAllChildren(document.querySelector('[role="tabpanel"]:not(.off) pre'));
      notify('Команды выделены — скопируйте их вручную');
    }
  });

  // Версия последнего релиза. Нет ответа от GitHub — бейджа просто нет.
  const ver = document.getElementById('ver');
  if (ver) {
    fetch('https://api.github.com/repos/Logmen/MonoPanel/releases/latest', { headers: { Accept: 'application/vnd.github+json' } })
      .then((r) => (r.ok ? r.json() : Promise.reject(new Error(String(r.status)))))
      .then((release) => {
        if (!release.tag_name) return;
        ver.lastElementChild.textContent = release.tag_name;
        ver.hidden = false;
      })
      .catch(() => {});
  }

  // Документация: копирование блоков кода целиком.
  document.querySelectorAll('.code-copy').forEach((b) =>
    b.addEventListener('click', async () => {
      const pre = b.parentElement.querySelector('pre');
      try {
        await navigator.clipboard.writeText(pre.textContent);
        notify('Скопировано');
      } catch {
        getSelection().selectAllChildren(pre);
        notify('Текст выделен — скопируйте его вручную');
      }
    })
  );

  // Документация: в оглавлении подсвечен заголовок, до которого дочитали.
  const tocLinks = [...document.querySelectorAll('.toc a')];
  if (tocLinks.length) {
    const byId = new Map(tocLinks.map((a) => [decodeURIComponent(a.hash.slice(1)), a]));
    const heads = [...byId.keys()].map((id) => document.getElementById(id)).filter(Boolean);
    let queued = false;
    const update = () => {
      queued = false;
      let active = heads[0];
      for (const h of heads) {
        if (h.getBoundingClientRect().top > innerHeight * 0.3) break;
        active = h;
      }
      tocLinks.forEach((a) => a.classList.toggle('on', byId.get(active.id) === a));
    };
    addEventListener('scroll', () => {
      if (!queued) requestAnimationFrame(update);
      queued = true;
    }, { passive: true });
    update();
  }

  // Подсветка раздела, который сейчас в середине экрана.
  const links = [...document.querySelectorAll('.menu a[href^="#"]')];
  if (links.length && 'IntersectionObserver' in window) {
    const spy = new IntersectionObserver(
      (entries) => {
        entries.forEach((e) => {
          if (!e.isIntersecting) return;
          links.forEach((a) => a.classList.toggle('tab-on', a.getAttribute('href') === '#' + e.target.id));
        });
      },
      { rootMargin: '-45% 0px -50% 0px' }
    );
    document.querySelectorAll('main > section[id]').forEach((s) => spy.observe(s));
  }
})();
