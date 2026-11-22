package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
)

// act is one menu action: an mp command whose arguments and flags come from
// the selected row or from the screen (the user page binds --user, the site
// page the domain). It always runs as a child process of this binary.
type act struct {
	path  string // "site add"
	key   string // hotkey on its screen
	label string // instead of the command's Short

	args    []string          // positional arguments: row keys, or "=literal"
	flags   map[string]string // flag → row key or "=literal": passed whenever set
	prefill map[string]string // flag → row key: the current value; passed only when changed
	hide    []string          // flags the screen sets itself and does not show
	only    []string          // a focused form: just these flags (a password change)

	quick  bool // run at once when nothing is missing (read-only views)
	change bool // an edit form: refuse to run when no flag differs (mail box set would reset the password)
	admin  bool // administrators only
	when   func(v map[string]string) bool
	tty    bool   // run in the terminal itself (a QR code, an interactive prompt)
	edit   string // "file" or "nginx": get → $EDITOR → put
	next   string // after success, offer this command with the same values
	note   func(v map[string]string) string
	run    func(m *model, v map[string]string) tea.Cmd // several commands in a row
	apply  func(m *model, f *form) tea.Cmd             // a filter form: the values go to a list
}

func (a *act) hides(name string) bool {
	for _, h := range a.hide {
		if h == name {
			return true
		}
	}
	if len(a.only) == 0 {
		return false
	}
	for _, o := range a.only {
		// --password stands for --password-stdin too.
		if o == name || o+"-stdin" == name {
			return false
		}
	}
	return true
}

func (a *act) labelFor(c *cobra.Command) string {
	if a.label != "" {
		return a.label
	}
	if c != nil && c.Short != "" {
		return c.Short
	}
	return a.path
}

// rowBound says whether the action takes values from a row (as opposed to
// the screen's own context), so it needs one selected.
func (a *act) rowBound(ctx map[string]string) bool {
	refs := append([]string{}, a.args...)
	for _, r := range a.flags {
		refs = append(refs, r)
	}
	for _, r := range a.prefill {
		refs = append(refs, r)
	}
	for _, r := range refs {
		if r == "" || strings.HasPrefix(r, "=") {
			// "" leaves an argument for the user (put <local> <remote>).
			continue
		}
		if _, ok := ctx[r]; !ok {
			return true
		}
	}
	return false
}

// shownFor says whether the action applies to these values and this role.
func (a *act) shownFor(v map[string]string, admin bool) bool {
	if a.admin && !admin {
		return false
	}
	if !admin && adminOnly(a.path) {
		return false
	}
	return a.when == nil || a.when(v)
}

// template is the command an action stands for, with what is bound already:
// shown in the help next to the action.
func (a *act) template(c *cobra.Command, v map[string]string) string {
	words := append([]string{"mp"}, strings.Fields(a.path)...)
	specs := parseUse(c.Use)
	for i, s := range specs {
		val := ""
		if i < len(a.args) {
			val, _ = resolve(a.args[i], v)
		}
		switch {
		case val != "":
			words = append(words, shellQuote(val))
		case s.required:
			words = append(words, "<"+s.name+">")
		}
	}
	names := make([]string, 0, len(a.flags))
	for name := range a.flags {
		names = append(names, name)
	}
	sort.Strings(names)
	// Merges the group's flags (--user of cron, files…) into c.Flags(), so
	// the lookup below finds them too.
	c.InheritedFlags()
	for _, name := range names {
		val, ok := resolve(a.flags[name], v)
		if !ok || val == "" {
			continue
		}
		if fl := c.Flags().Lookup(name); fl != nil && fl.Value.Type() == "bool" {
			if val == "true" {
				words = append(words, "--"+name)
			} else {
				words = append(words, "--"+name+"=false")
			}
			continue
		}
		words = append(words, "--"+name, shellQuote(val))
	}
	return strings.Join(words, " ")
}

// same binds flags to the row keys of the same name.
func same(names ...string) map[string]string {
	out := make(map[string]string, len(names))
	for _, n := range names {
		out[n] = n
	}
	return out
}

func merge(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// confirmSpec is the question asked before a command that destroys or
// interrupts something.
type confirmSpec struct {
	title string
	line  string
	note  string
	word  string // to be typed (account and site deletion with files)
}

// destructive verbs: the command removes, stops or switches something off.
func destructive(name string) bool {
	switch name {
	case "rm", "remove", "delete", "disable", "revoke", "purge", "suspend", "restore", "stop", "self-signed", "tune", "unset", "untrust", "totp-reset":
		return true
	}
	return false
}

// confirmFor decides whether the form's command needs a confirmation and
// what it says: wherever the web UI asks before the same action (its
// Confirm dialogs), and before whatever destroys or interrupts something.
// The note says what really happens; the screen's own note comes first.
func confirmFor(f *form) *confirmSpec {
	args := f.args()
	arg0 := ""
	if len(args) > 0 {
		arg0 = args[0]
	}
	c := &confirmSpec{title: f.act.labelFor(f.cmd), line: f.line()}
	if f.act.note != nil {
		c.note = f.act.note(f.vals)
	}
	set := func(name string) bool { v, ok := f.flagValue(name); return ok && v == "true" }
	// about asks with these notes, unless the screen has said it already.
	about := func(notes ...string) *confirmSpec {
		if c.note == "" {
			c.note = strings.Join(notes, " ")
		}
		return c
	}
	// asks asks only when there is something to say: a flag that switches
	// a state the web UI asks about.
	asks := func(notes []string) *confirmSpec {
		if len(notes) == 0 {
			return nil
		}
		return about(notes...)
	}
	switch f.path {
	case "user rm":
		c.word = arg0
		if set("purge") {
			c.note = fmt.Sprintf(T("Домашний каталог /var/www/%s будет удалён со всеми файлами.", "The home directory /var/www/%s is deleted with all its files."), arg0)
		} else {
			c.note = fmt.Sprintf(T("Сайты, базы, cron, app-сервисы, Valkey и сертификаты аккаунта будут удалены; файлы останутся в /var/www/%s.", "The account's sites, databases, cron jobs, app services, Valkey and certificates are deleted; the files stay in /var/www/%s."), arg0)
		}
		return c
	case "site rm":
		if set("purge") {
			c.word = arg0
			c.note = T("Каталог сайта будет удалён вместе с файлами.", "The site directory is deleted with its files.")
		} else {
			c.note = T("Конфигурация и пул сайта будут удалены, файлы останутся.", "The site's configuration and pool are deleted, the files stay.")
		}
		return c
	case "backup restore":
		if set("in-place") {
			c.word = arg0
			c.note = T("Снимок будет восстановлен поверх текущих файлов: их нынешнее содержимое пропадёт.", "The snapshot is restored over the current files: what they hold now is lost.")
		}
		return c
	case "cms install":
		if set("force") {
			c.note = T("Файлы в docroot будут удалены, прежняя база этой CMS на сайте очищена.", "The files in the docroot are deleted and the site's previous database of this CMS is emptied.")
			return c
		}
		return nil
	case "selinux":
		if arg0 != "permissive" {
			return nil
		}
		return about(T("SELinux перестанет блокировать нарушения и будет только записывать их в журнал.", "SELinux stops blocking violations and only logs them."))
	case "site nginx":
		if set("clear") {
			return about(T("Файл своих директив сайта будет удалён, и nginx перечитает конфигурацию без него — после той же проверки, что при сохранении.", "The site's file of custom directives is deleted and nginx reloads without it, after the same check as on saving."))
		}
		return nil
	case "ssl renew":
		// As the web UI asks: a renewal orders a new certificate now.
		return about(T("Сертификат будет перевыпущен сейчас, не дожидаясь автопродления. У Let's Encrypt есть лимиты (пять одинаковых сертификатов в неделю), ручные продления в них входят; текущий сертификат работает, пока не выпущен новый.", "The certificate is reissued now instead of waiting for auto-renewal. Let's Encrypt has rate limits (five identical certificates a week) and manual renewals count against them; the current certificate keeps working until the new one is issued."))
	case "update trust":
		// The key that vouches for packages installed as root.
		switch {
		case set("off"):
			c.note = T("Проверка подписи релизов будет выключена: обновление примется по одной контрольной сумме.", "The release signature check is switched off: an update is then accepted by its checksum alone.")
		case set("clear"):
			c.note = T("Свой ключ будет убран: релизы снова проверяются встроенным ключом.", "The custom key is removed: releases are checked against the built-in key again.")
		case func() bool { v, ok := f.flagValue("key"); return ok && v != "" }():
			c.note = T("Устанавливаться будут только релизы, подписанные этим ключом.", "From now on only releases signed with this key are installed.")
		default:
			return nil
		}
		return c
	case "app set":
		if set("disable") {
			return about(T("Сервис будет остановлен и не запустится при загрузке, пока его не включат снова.", "The service is stopped and does not start at boot until it is enabled again."))
		}
		return nil
	case "user set":
		return asks(userSetNotes(f))
	case "mail box set":
		return asks(mailboxNotes(f))
	case "mail domain set":
		return asks(mailDomainNotes(f))
	case "update settings":
		return asks(updateSettingsNotes(f))
	case "site move-ip":
		return about(moveIPNotes(f, arg0)...)
	case "config set":
		if !set("restart") {
			return nil
		}
		// The screen warns about a new address; the restart comes first.
		restart := T("monopanel-api перезапустится: панель несколько секунд не будет отвечать, идущие задачи прервутся и начнутся заново (перенос аккаунта остановится: удалите созданный им аккаунт и запустите перенос снова). Сайты, почта и базы не затрагиваются.", "monopanel-api restarts: the panel does not answer for a few seconds and running jobs are interrupted and start over (a migration stops instead: remove the account it made and run it again). Sites, mail and databases are not affected.")
		if c.note == "" && (arg0 == "web.listen" || arg0 == "web.hostname") {
			c.note = T("После перезапуска панель может отвечать по другому адресу.", "After the restart the panel may answer at another address.")
		}
		c.note = strings.TrimSpace(restart + " " + c.note)
		return c
	case "mail webmail":
		// Only a run over an installed webmail asks, as the web UI's
		// "Reinstall / update" does; the first install is just a form.
		if w := f.vals["webmail"]; w != "" {
			return about(fmt.Sprintf(T("Вебпочта уже установлена (%s): команда установит Roundcube заново на указанный сайт — переустановка, обновление или перенос.", "Webmail is already installed (%s): this installs Roundcube again on the given site — a reinstall, an update or a move."), w))
		}
		return nil
	case "service stop", "service disable", "service restart", "service reload-or-restart":
		if note := serviceNote(f.cmd.Name(), arg0); note != "" {
			return about(note)
		}
		return nil
	case "stack install":
		return about(stackInstallNote(arg0, f.vals["name"] == arg0 && f.vals["installed"] == "true"))
	case "stack remove":
		return about(stackRemoveNote(arg0))
	case "stack upgrade":
		return about(stackUpgradeNote(arg0))
	case "php ext disable":
		note := fmt.Sprintf(T("Расширение выключится у всех сайтов ветки %s сразу: мастер php-fpm один на версию, он перезапустится.", "The extension is switched off for every site on %s at once: there is one php-fpm master per version, and it restarts."), arg0)
		if f.vals["critical"] == "true" {
			note = T("Это расширение нужно типовому сайту: сайты, которые им пользуются, начнут отдавать ошибки. ", "A typical site needs this extension: the sites that use it start returning errors. ") + note
		}
		return about(note)
	}
	if note, ok := webNote(f.path, arg0); ok {
		return about(note)
	}
	if destructive(f.cmd.Name()) || f.yes {
		return c
	}
	return nil
}

// args are the positional values the form will pass.
func (f *form) args() []string {
	var out []string
	for _, fd := range f.fields {
		v := strings.TrimSpace(fd.value())
		if fd.arg && v != "" {
			if fd.variadic {
				items, _ := splitWords(v)
				out = append(out, items...)
			} else {
				// As build passes it: the name to type matches the argument.
				out = append(out, v)
			}
		}
	}
	return out
}

// passes says whether the flag goes to the command, a secret piped to its
// stdin twin too (password stands for --password-stdin).
func (f *form) passes(name string) bool {
	for _, fd := range f.fields {
		if !fd.arg && fd.name == name && fd.passes() {
			return true
		}
	}
	return false
}

// flagValue is the value of a flag the form passes.
func (f *form) flagValue(name string) (string, bool) {
	for _, fd := range f.fields {
		if !fd.arg && fd.name == name && fd.stdin == "" && fd.passes() {
			return fd.value(), true
		}
	}
	return "", false
}
