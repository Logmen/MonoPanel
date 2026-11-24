package tui

import (
	"fmt"
	"strings"
)

// What the confirmations say. The web UI asks before the same actions (its
// Confirm dialogs) and explains what really happens; these are the same
// explanations for the terminal, keyed by the mp command.

// webNote is the note of a command that always asks: the web UI asks
// before it too, or it interrupts something.
func webNote(path, arg0 string) (string, bool) {
	switch path {
	case "site apply":
		return fmt.Sprintf(T("Панель заново соберёт server-блок nginx и пул php-fpm сайта %s из своего состояния и применит их, проверив конфигурацию. Правки в sites/%s.d/*.conf останутся, изменения руками в сгенерированном блоке будут заменены; если сайт ждёт сертификат, панель закажет его снова.",
			"The panel rebuilds the nginx server block and the php-fpm pool of %s from its own state and applies them after checking the configuration. Edits in sites/%s.d/*.conf stay; manual changes inside the generated block are overwritten. A site waiting for a certificate gets another attempt."), arg0, arg0), true
	case "site fix":
		return siteFixNote(), true
	case "site suspend":
		return T("Все посетители начнут получать заглушку 503. Файлы, база, сертификат и почта остаются на месте; включить сайт обратно — mp site unsuspend.",
			"Every visitor gets a 503 placeholder page. Files, database, certificate and mail stay in place; mp site unsuspend brings the site back."), true
	case "site unsuspend":
		return T("Сайт снова начнёт отдавать своё содержимое вместо заглушки.", "The site serves its content again instead of the placeholder page."), true
	case "mail apply":
		return T("Панель заново соберёт конфигурацию postfix, dovecot и opendkim и почтовые карты из своего состояния и применит их: конфигурация проверяется, затем службы её перечитывают — даже если ни один файл не изменился. Правки, внесённые в эти файлы руками, будут заменены.",
			"The panel rebuilds the postfix, dovecot and opendkim configuration and the mail maps from its own state and applies them: the configuration is checked, then the services reload it, even if no file has changed. Manual changes in these files are overwritten."), true
	case "mail domain dkim":
		return T("Старый ключ сразу перестанет подписывать письма, а получатели проверяют подпись по TXT-записи в DNS: пропишите новую запись (mp mail domain dns), иначе подпись не сойдётся.",
			"The old key stops signing at once, and recipients check the signature against the TXT record in DNS: publish the new record (mp mail domain dns), or the signature will not verify."), true
	case "firewall unban":
		return T("Запрещающие правила ровно для этого адреса без порта удалятся (бан — такое правило, как и добавленное вручную), а fail2ban снимет с него свои баны: адрес снова сможет подключаться. Если перебор продолжится, jail забанит его заново.",
			"Deny rules for exactly this address without a port are deleted (a ban is such a rule, and so is one added by hand), and fail2ban lifts its bans: the address can connect again. If the brute force goes on, a jail bans it again."), true
	case "firewall disable":
		return T("Таблица nftables будет снята целиком: снаружи станут доступны все порты, которые слушают службы сервера. Правила и баны сохранятся и вернутся при включении.",
			"The whole nftables table is removed: every port a service of the server listens on becomes reachable from outside. Rules and bans are kept and come back when the firewall is enabled again."), true
	case "firewall apply":
		return T("Таблица nftables панели будет собрана заново из правил и загружена.", "The panel's nftables table is generated again from the rules and loaded."), true
	case "php remove":
		return fmt.Sprintf(T("Пакеты ветки PHP %s и её php-fpm будут удалены с сервера. Сайты на этой ветке перестанут отвечать — сначала переведите их на другую.",
			"The packages of PHP %s and its php-fpm are removed from the server. Sites on it stop responding: switch them to another version first."), arg0), true
	case "db config set", "db config unset":
		return T("Чтобы применить параметры, MySQL перезапустится: несколько секунд сайты не смогут обратиться к базам. Если с новыми значениями сервер не запустится, вернутся прежние.",
			"MySQL restarts to apply them: for a few seconds the sites cannot reach their databases. If it does not start with the new values, the previous ones come back."), true
	case "db phpmyadmin remove":
		return T("phpMyAdmin уйдёт целиком: его порт закроется, пул и файлы удалятся, временные учётки кнопки «открыть» пропадут, а все вошедшие выйдут. Базы и их пользователи не затрагиваются.",
			"phpMyAdmin goes altogether: its port closes, its pool and files are removed, the temporary accounts of the open button are dropped and everybody signed in is signed out. The databases and their users stay."), true
	case "db engine tune":
		return T("Значения панели пересчитаются по памяти этого хоста (заданные вами останутся), MySQL перезапустится: несколько секунд сайты не смогут обратиться к базам.",
			"The panel's values are recalculated for this host's memory (the ones you set stay) and MySQL restarts: for a few seconds the sites cannot reach their databases."), true
	case "backup target rm":
		return T("Панель забудет репозиторий, его пароль и учётные данные; снимки в нём останутся, но без пароля их не прочитать.",
			"The panel forgets the repository, its password and credentials; the snapshots stay in it, but without the password they cannot be read."), true
	case "ssl panel self-signed":
		return T("Выпущенный сертификат панели будет удалён вместе с файлами, браузеры снова начнут предупреждать о сертификате на порту панели. Сайты это не затронет.",
			"The panel's issued certificate is deleted with its files, and browsers warn about the certificate on the panel port again. Sites are not affected."), true
	case "dns-provider rm":
		return T("Зашифрованные учётные данные провайдера будут удалены: сертификаты, которые проверяются через него, перестанут продлеваться, пока его не добавят снова.",
			"The provider's encrypted credentials are deleted: certificates validated through it can no longer renew until it is added again."), true
	case "update apply":
		return T("Пакет поставится отдельной задачей systemd: панель и агент перезапустятся, идущие задачи дождутся их. Если новая версия не ответит, панель откатится на прежнюю.",
			"The package is installed by a separate systemd job: the panel and the agent restart, and running jobs wait for them. If the new version does not answer, the panel rolls back."), true
	case "migrate run":
		return T("Задача создаст здесь аккаунт и скопирует в него всё, что перечислено в разборе: файлы, базы, сайты, почту, сертификаты, cron, app-сервисы и Valkey. Источник только читается.",
			"A job creates the account here and copies into it everything the plan lists: files, databases, sites, mail, certificates, cron, app services and Valkey. The source is only read."), true
	case "user totp-reset":
		return T("Если у аккаунта включена 2FA, она выключится: для входа хватит пароля, пока 2FA не настроят заново. Это на случай потерянного телефона.",
			"If the account has 2FA on, it is switched off: the password alone signs in until 2FA is set up again. This is for a lost phone."), true
	case "app stop":
		return T("Процесс будет остановлен, и всё, что на нём держится (сайт в режиме proxy, бот, очередь), перестанет отвечать до запуска.",
			"The process is stopped, and whatever depends on it (a site in proxy mode, a bot, a queue) stops responding until it is started again."), true
	case "token revoke":
		return T("Всё, что ходит в API с этим токеном, сразу получит 401; вернуть тот же токен нельзя, только выпустить новый.",
			"Everything that calls the API with this token gets 401 at once; the same token cannot be restored, only a new one issued."), true
	}
	return "", false
}

func siteFixNote() string {
	return T("Панель заново создаст каталоги сайта с их правами, сделает владельца сайта владельцем всего дерева, снова выставит ACL веб-сервера и вернёт метки SELinux. Нужно, когда файлы копировали или правили от root.",
		"The panel recreates the site's directories with their modes, makes the site's owner the owner of the whole tree, sets the web server's ACLs again and restores the SELinux labels. For files copied or edited as root.")
}

// moveIPNotes: which sites move where, as the web UI's dialog says it.
func moveIPNotes(f *form, to string) []string {
	from, _ := f.flagValue("from")
	which := fmt.Sprintf(T("Все сайты с адресов, которых на сервере больше нет, переедут на %s.", "Every site on an address this server no longer has moves to %s."), to)
	if from = strings.TrimSpace(from); from != "" {
		which = fmt.Sprintf(T("Все сайты с %s переедут на %s.", "Every site on %s moves to %s."), from, to)
	}
	return []string{which, fmt.Sprintf(T("Панель перепишет их конфигурацию nginx под новый адрес и заново применит каждый сайт; DNS-записи сайтов должны указывать на %s.",
		"The panel rewrites their nginx configuration for the new address and applies each site again; the sites' DNS records must point to %s."), to)}
}

// userSetNotes: the changes of mp user set the web UI asks about — the
// status both ways and the shell both ways.
func userSetNotes(f *form) []string {
	var notes []string
	if v, ok := f.flagValue("status"); ok {
		switch v {
		case "suspended":
			notes = append(notes, T("Аккаунт больше не сможет работать с панелью: вход, его сеансы и API-токены, mp через локальный сокет. SFTP/SSH, сайты, базы и cron продолжат работать — это блокировка панели, а не остановка хозяйства.",
				"The account can no longer use the panel: signing in, its sessions and API tokens, mp over the local socket. SFTP/SSH, sites, databases and cron keep working — this blocks the panel, it does not stop the hosting."))
		case "active":
			notes = append(notes, T("Аккаунт снова сможет работать с панелью.", "The account can use the panel again."))
		}
	}
	if v, ok := f.flagValue("shell"); ok && v == "true" {
		notes = append(notes, T("Аккаунт получит настоящую командную оболочку на сервере вместо SFTP в пределах своего каталога.", "The account gets a real command shell on the server instead of SFTP within its own directory."))
	}
	if v, ok := f.flagValue("sftp-only"); ok && v == "true" {
		notes = append(notes, T("Вход по SSH закроется, останется доступ к своим файлам по SFTP в пределах домашнего каталога; уже открытые сессии не разрываются.",
			"SSH login is closed and SFTP access to the account's own files within its home stays; sessions already open are not terminated."))
	}
	return notes
}

// mailboxNotes: switching a mailbox off or on, and the new password the
// panel generates when mp mail box set changes none of the mailbox's fields.
func mailboxNotes(f *form) []string {
	if v, ok := f.flagValue("active"); ok {
		switch v {
		case "false":
			return []string{T("Почта на ящик перестанет приниматься — отправитель получит отказ, войти в IMAP и POP3 будет нельзя. Письма на диске останутся, ящик можно включить обратно.",
				"Mail to the mailbox is no longer accepted — the sender gets a bounce — and IMAP and POP3 logins are refused. Messages stay on disk; the mailbox can be enabled again.")}
		case "true":
			return []string{T("Ящик снова начнёт принимать почту и пускать в IMAP и POP3.", "The mailbox accepts mail and allows IMAP and POP3 logins again.")}
		}
	}
	// The request is empty when none of the mailbox's own fields is set, and
	// the panel then generates the password: a flag the group adds (--domain
	// filters the list) changes nothing, and neither does --active=''.
	for _, name := range []string{"password", "name", "quota"} {
		if f.passes(name) {
			return nil
		}
	}
	if v, ok := f.flagValue("active"); ok && v != "" {
		return nil
	}
	return []string{T("Панель сгенерирует новый пароль: старый перестанет работать сразу, почтовые клиенты и вебпочта попросят новый. Он показывается один раз.",
		"The panel generates a new password: the old one stops working at once, and mail clients and webmail ask for the new one. It is shown only once.")}
}

// mailDomainNotes: every switch of mp mail domain set is asked about in the
// web UI, both ways.
func mailDomainNotes(f *form) []string {
	var notes []string
	if v, ok := f.flagValue("active"); ok {
		switch v {
		case "false":
			notes = append(notes, T("Сервер перестанет принимать почту домена: его ящики и алиасы ничего не получат, войти ящиками в IMAP, POP3 и SMTP будет нельзя, исходящие письма перестанут подписываться DKIM. Письма на диске останутся.",
				"The server stops accepting the domain's mail: its mailboxes and aliases receive nothing, its mailboxes cannot log in to IMAP, POP3 or SMTP, and its outgoing mail is no longer signed with DKIM. Messages stay on disk."))
		case "true":
			notes = append(notes, T("Домен снова заработает: почта принимается, ящики пускают в IMAP, POP3 и SMTP, исходящие письма подписываются DKIM, если у домена есть ключ.",
				"The domain works again: its mail is accepted, its mailboxes can log in, and outgoing mail is signed with DKIM if the domain has a key."))
		}
	}
	if v, ok := f.flagValue("send-only"); ok {
		switch v {
		case "true":
			notes = append(notes, T("Этот сервер перестанет принимать почту домена: письма сайтов на его адреса уйдут на MX у почтового провайдера. Подпись DKIM сохраняется; ящиков и алиасов у домена здесь быть не должно.",
				"This server stops receiving the domain's mail: the sites' letters to its addresses go to the MX at your mail provider. DKIM keeps signing; the domain must have no mailboxes or aliases here."))
		case "false":
			notes = append(notes, T("Домен станет обычным почтовым доменом этого сервера: письма сайтов на его адреса будут доставляться сюда. Чтобы принимать почту извне, MX должен указывать на этот сервер.",
				"The domain becomes a regular mail domain of this server: the sites' letters to its addresses are delivered here. For mail from outside, the MX has to point to this server."))
		}
	}
	if v, ok := f.flagValue("lenient"); ok {
		switch v {
		case "true":
			notes = append(notes, T("Панель перестанет отклонять письма с несуществующим доменом отправителя и неправильным HELO — они дойдут до ящика. Это нужно диагностическим приёмникам; обычному домену только добавит спама.",
				"The panel stops rejecting mail from a non-existent sender domain or with a wrong HELO: it reaches the mailbox. Diagnostic sink domains need this; for a regular domain it only adds spam."))
		case "false":
			notes = append(notes, T("Письма от отправителей с несуществующим доменом и неправильным HELO снова будут отклоняться на входе.", "Mail from senders with a non-existent domain or a wrong HELO is rejected at the door again."))
		}
	}
	return notes
}

// updateSettingsNotes: switching update checks off ("-" as the repository)
// and forgetting the repository token.
func updateSettingsNotes(f *form) []string {
	var notes []string
	if v, ok := f.flagValue("repo"); ok && strings.TrimSpace(v) == "-" {
		notes = append(notes, T("Проверка обновлений выключится: панель забудет репозиторий, адрес API и сохранённый токен. Чтобы включить её снова, укажите репозиторий.",
			"Update checks are switched off: the panel forgets the repository, the API address and the saved token. To switch them on again, set the repository."))
	}
	if v, ok := f.flagValue("clear-token"); ok && v == "true" {
		notes = append(notes, T("Пока не сохранён новый токен, приватный репозиторий нельзя ни проверить, ни поставить из него обновление.", "Until a new token is saved, a private repository can be neither checked nor installed from."))
	}
	return notes
}

// serviceNote: what stopping or switching off a unit costs, and what a
// restart of the panel's own units does; "" for a restart of anything else,
// which the web UI does not ask about.
func serviceNote(verb, unit string) string {
	u := strings.TrimSuffix(unit, ".service")
	self := u == "monopanel-api" || u == "monopanel-agent"
	switch verb {
	case "stop":
		return unitStopNote(u)
	case "disable":
		if self {
			return T("После перезагрузки сервера панель может не подняться сама. Сейчас юнит продолжит работать.", "After a reboot the panel may not come up by itself. The unit keeps running now.")
		}
		return T("После перезагрузки сервера юнит сам не запустится. Сейчас он продолжит работать — остановите его отдельно, если нужно.", "After a reboot the unit does not start by itself. It keeps running now: stop it separately if needed.")
	case "restart", "reload-or-restart":
		switch u {
		case "monopanel-api":
			return T("API панели перезапустится: несколько секунд меню не сможет до него достучаться. Задачи, которые выполняются сейчас, прервутся и начнутся заново (перенос аккаунта остановится: удалите созданный им аккаунт и запустите перенос снова).", "The panel API restarts: for a few seconds the menu cannot reach it. Jobs running now are interrupted and start over (a migration stops instead: remove the account it made and run it again).")
		case "monopanel-agent":
			return T("Агент перезапустится, а вместе с ним и API: несколько секунд меню не сможет до них достучаться. Задачи, которые выполняются сейчас, прервутся и начнутся заново (перенос аккаунта остановится: удалите созданный им аккаунт и запустите перенос снова).", "The agent restarts, and the API with it: for a few seconds the menu cannot reach them. Jobs running now are interrupted and start over (a migration stops instead: remove the account it made and run it again).")
		}
	}
	return ""
}

// unitStopNote says what stops working with the unit, by what the unit is.
func unitStopNote(u string) string {
	switch {
	case u == "monopanel-api":
		return T("API панели остановится, а с ним веб-интерфейс и команды этого меню. Сама панель не поднимется: запустите её на сервере — systemctl start monopanel-api.", "The panel API stops, and with it the web UI and the commands of this menu. It does not come back by itself: start it on the server with systemctl start monopanel-api.")
	case u == "monopanel-agent":
		return T("Агент остановится, а вместе с ним и API — без агента он не работает. Запустите панель на сервере командой systemctl start monopanel-api: агент поднимется вместе с ней.", "The agent stops, and the API with it — it cannot work without the agent. Start the panel on the server with systemctl start monopanel-api: the agent comes up with it.")
	case u == "monopanel-firewall":
		return T("Таблица nftables панели будет снята: снаружи станут доступны все порты, которые слушают службы сервера, — пока юнит не запустят снова или не применят firewall.", "The panel's nftables table is removed: every port a service of the server listens on becomes reachable from outside until the unit is started again or the firewall is applied.")
	case u == "nginx":
		return T("Все сайты сервера перестанут отвечать, пока nginx не запустят снова. Сама панель продолжит работать: она слушает свой порт.", "Every site on this server stops answering until nginx is started again. The panel itself keeps working: it listens on its own port.")
	case strings.HasPrefix(u, "monopanel-apache@"):
		return T("Сайты этого аккаунта в режиме apache будут отвечать 502, пока его Apache не запустят снова; сайты других аккаунтов продолжат работать.", "This account's sites in apache mode answer 502 until its Apache is started again; the sites of other accounts keep working.")
	case u == "apache2" || u == "httpd":
		return T("Это Apache дистрибутива: сайты панели в режиме apache работают на Apache своих аккаунтов (monopanel-apache@<логин>) и продолжат работать.", "This is the distribution's Apache: the panel's sites in apache mode run on their accounts' own Apaches (monopanel-apache@<login>) and keep working.")
	case u == "mysql" || u == "mysqld" || u == "mariadb":
		return T("Сайты потеряют базы данных и начнут показывать ошибки, пока сервер не запустят снова; выполняющиеся запросы оборвутся.", "Sites lose their databases and show errors until the server is started again; queries running now are cut off.")
	case (strings.HasPrefix(u, "php") && strings.HasSuffix(u, "-fpm")) || strings.HasPrefix(u, "monopanel-php-fpm@"):
		return T("Сайты на этой версии PHP будут отвечать 502, пока службу не запустят снова.", "Sites on this PHP version answer 502 until the service is started again.")
	}
	return T("Юнит остановится и не заработает, пока его не запустят снова; юниты, которым он нужен, остановятся вместе с ним.", "The unit stops and stays stopped until someone starts it again; units that require it stop with it.")
}

// stackInstallNote: what an installation does, and that installing an
// installed component runs the installation again (the way to repair it).
func stackInstallNote(name string, installed bool) string {
	db := name == "percona" || name == "mysql"
	switch {
	case name == "composer":
		return T("composer.phar скачается с getcomposer.org, контрольная сумма сверится с опубликованной; повторная установка обновляет его.", "composer.phar is downloaded from getcomposer.org and its checksum verified against the published one; installing again updates it.")
	case installed && db:
		return T("Установка пройдёт заново: репозиторий вендора и пакеты сервера обновятся, root вернётся на auth_socket, конфигурация пересчитается под память сервера (ваши значения сохранятся), а MySQL перезапустится — на несколько секунд сайты останутся без базы.", "The installation runs again: the vendor repository and the server packages are refreshed, root goes back to auth_socket, the configuration is recalculated for this host's memory (your own values stay) and MySQL restarts, so sites lose the database for a few seconds.")
	case installed:
		return T("Установка пройдёт заново: пакеты установятся или обновятся, конфигурация компонента от панели запишется снова, а его служба перечитает её или перезапустится. Так чинят сломавшийся компонент; файлы сайтов и базы не затрагиваются.", "The installation runs again: the packages are installed or brought up to date, the panel's configuration of the component is written again and its service reloaded or restarted. This repairs a component that broke; the sites' files and databases are not touched.")
	case db:
		return T("Подключится репозиторий вендора, установится сервер, root перейдёт на auth_socket, конфигурация подберётся по объёму памяти.", "The vendor repository is added, the server installed, root switched to auth_socket, and the configuration sized for the available memory.")
	}
	return T("Подключится репозиторий, если нужен, установятся пакеты и, если есть, служба. Займёт от нескольких секунд до пары минут.", "The repository is added if needed, the packages installed and the service, if any, started. Takes from a few seconds to a couple of minutes.")
}

// stackRemoveNote: what goes with a component.
func stackRemoveNote(name string) string {
	switch name {
	case "memcached":
		return T("Служба остановится, пакет удалится. Сайты, которые держат в нём кеш и сессии, начнут получать ошибки подключения.", "The service stops and the package is removed. Sites that keep their cache and sessions in it start getting connection errors.")
	case "valkey":
		return T("Пакет будет удалён. Пока у аккаунтов остаются экземпляры, удаление откажет: сначала удалите их.", "The package is removed. While any account still has an instance the removal is refused: delete them first.")
	case "composer":
		return T("Файлы composer будут удалены; проекты на сервере продолжат работать, но обновлять зависимости будет нечем.", "The composer files are removed; projects on the server keep working, but there is nothing to update dependencies with.")
	case "sphinx":
		return T("Служба поиска остановится, пакет удалится вместе с файлами индекса. Битрикс, переключённый на Sphinx, вернётся к поиску по базе только после смены настройки.", "The search service stops and the package is removed along with the index files. A Bitrix site switched to Sphinx goes back to database search only after its setting is changed.")
	}
	return T("Пакет будет удалён с сервера.", "The package is removed from the server.")
}
