package tui

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// A form is generated from a cobra command: one field per positional
// argument of its Use string and one per flag. Values bound from the
// selected row are filled in. The command line it builds is shown live and
// is exactly what runs.

type fieldKind int

const (
	kindText fieldKind = iota
	kindNumber
	kindBool
	kindEnum
	kindList
)

type field struct {
	kind     fieldKind
	name     string // argument name from Use, or flag name
	label    string
	arg      bool
	variadic bool
	required bool
	choices  []string
	usage    string
	initial  string // what the field started with: the default or the row's value
	input    textInput
	val      string // value of a bool or enum field
	forced   bool   // bound from the context: passed whenever it has a value
	pinned   bool   // passed even when equal to the initial value
	secret   bool
	stdin    string // set on a secret piped to stdin: the flag that tells the command to read it
	array    bool   // stringArray: the flag is repeated per item instead of a comma list
	pairs    bool   // a piped list of KEY=VALUE lines
	typ      string
}

// StdinOrderAnnotation, on a command, lists its -stdin flags in the order it
// reads them ("current-password-stdin,password-stdin"): the TUI may then pipe
// several one-line secrets to it, one per line in that order.
const StdinOrderAnnotation = "mp:stdin-order"

// pipedList says the field is a list secret for stdin (--cred-stdin,
// --env-stdin): its items go one per line, after a one-line secret if the
// command reads one too.
func (fd *field) pipedList() bool { return fd.stdin != "" && fd.kind == kindList }

// items are the values of a piped list: comma or semicolon separated, an
// item in double quotes keeps either.
func (fd *field) items() []string { return splitPairs(fd.value()) }

func (fd *field) value() string {
	if fd.kind == kindBool || fd.kind == kindEnum {
		return fd.val
	}
	return fd.input.value()
}

func (fd *field) setValue(v string) {
	switch fd.kind {
	case kindBool:
		if b, err := strconv.ParseBool(v); err == nil && b {
			fd.val = "true"
		} else {
			fd.val = "false"
		}
	case kindEnum:
		fd.val = v
		for _, c := range fd.choices {
			if c == v {
				return
			}
		}
		fd.choices = append(fd.choices, v)
	default:
		fd.input.set(v)
	}
}

// passes says whether the field goes on the command line.
func (fd *field) passes() bool {
	v := fd.value()
	switch {
	case fd.pipedList():
		return len(fd.items()) > 0
	case fd.arg, fd.stdin != "":
		return strings.TrimSpace(v) != ""
	case fd.kind == kindNumber && strings.TrimSpace(v) == "":
		// An emptied number means "the default", not --n=''.
		return false
	case fd.forced:
		return v != ""
	}
	return v != fd.initial || fd.pinned
}

// formEnv is what decides which flags a form shows.
type formEnv struct {
	remote bool     // hide the flags that bypass the API (--local)
	admin  bool     // ordinary accounts do not get the administrator's flags
	php    []string // the PHP branches installed on the server: the choices of --php
}

type form struct {
	screenBase
	cmd    *cobra.Command
	path   string
	act    *act
	vals   map[string]string
	fields []*field
	focus  int // len(fields) is the run button
	offset int
	err    string
	yes    bool // the command asks y/N itself: --yes is passed once the TUI has asked
	reveal bool
	extra  []string // output switches typed in the palette: --json, --no-wait
}

// resolve reads a binding: "=text" is the text itself, anything else is a
// key of the row (or of the screen's context).
func resolve(ref string, vals map[string]string) (string, bool) {
	if strings.HasPrefix(ref, "=") {
		return ref[1:], true
	}
	v, ok := vals[ref]
	return v, ok
}

func newForm(c *cobra.Command, env formEnv, a *act, vals map[string]string) *form {
	f := &form{cmd: c, path: cmdPath(c), act: a, vals: vals}
	if f.act == nil {
		f.act = &act{path: f.path}
	}
	a = f.act
	// The breadcrumb names the command; its description is on the form.
	f.title = "mp " + f.path
	if a.label != "" {
		f.title = a.label
	}
	for i, spec := range parseUse(c.Use) {
		fd := &field{kind: kindText, arg: true, name: spec.name, required: spec.required, variadic: spec.variadic}
		fd.label = "<" + spec.name + ">"
		if !spec.required {
			fd.label = "[" + spec.name + "]"
		}
		if spec.variadic {
			fd.label += "…"
		}
		if ch := argChoices(c, i); len(ch) > 0 && !spec.variadic {
			// "" first: not chosen yet (a required one is then reported).
			fd.kind = kindEnum
			fd.choices = append([]string{""}, ch...)
		}
		if i < len(a.args) {
			if v, ok := resolve(a.args[i], vals); ok {
				if spec.variadic && v != "" {
					// One value of the row stays one argument: a php.ini
					// value such as "E_ALL & ~E_DEPRECATED" has spaces.
					v = shellQuote(v)
				}
				fd.setValue(v)
			}
		}
		fd.initial = fd.value()
		f.fields = append(f.fields, fd)
	}

	// Flags in the order the command declares them; the ones inherited from
	// the group (--user of cron, app, files…) first, the global ones never.
	inherited := c.InheritedFlags()
	root := c.Root()
	fs := c.Flags()
	sorted := fs.SortFlags
	fs.SortFlags = false
	var group, local []*pflag.Flag
	stdinBases := map[string]string{}
	fs.VisitAll(func(fl *pflag.Flag) {
		switch {
		case fl.Name == "help", fl.Hidden, fl.Deprecated != "":
			return
		case root.PersistentFlags().Lookup(fl.Name) == fl:
			return
		case fl.Name == "yes":
			f.yes = true
			return
		case env.remote && localFlag(fl.Name):
			return
		case !env.admin && adminFlag(f.path, fl.Name):
			return
		case a.hides(fl.Name):
			return
		}
		if strings.HasSuffix(fl.Name, "-stdin") && fl.Value.Type() == "bool" {
			stdinBases[strings.TrimSuffix(fl.Name, "-stdin")] = fl.Name
		}
		if inherited.Lookup(fl.Name) == fl {
			group = append(group, fl)
		} else {
			local = append(local, fl)
		}
	})
	fs.SortFlags = sorted
	for _, fl := range append(group, local...) {
		if _, ok := stdinBases[strings.TrimSuffix(fl.Name, "-stdin")]; ok && strings.HasSuffix(fl.Name, "-stdin") {
			f.fields = append(f.fields, stdinField(fs, fl))
			continue
		}
		if (fl.Value.Type() == "string" || listType(fl.Value.Type())) && stdinCounterpart(fl.Name, stdinBases) {
			// The secret goes through stdin instead, never on the command
			// line: a list of credentials (--cred, --env) as much as a password.
			continue
		}
		fd := flagField(f.path, fl)
		if ref, ok := a.flags[fl.Name]; ok {
			if v, ok := resolve(ref, vals); ok && v != "" {
				fd.setValue(v)
				fd.forced = true
			}
		}
		if ref, ok := a.prefill[fl.Name]; ok {
			if v, ok := resolve(ref, vals); ok {
				fd.setValue(v)
				fd.initial = fd.value()
			}
		}
		if fl.Name == "php" && fd.typ == "string" && len(env.php) > 0 {
			// After the row's value: the site's own branch stays a choice.
			fd.offerPHP(env.php)
		}
		f.fields = append(f.fields, fd)
	}
	f.focus = f.firstFocus()
	return f
}

// stdinCounterpart says whether a flag carries the same secret as a
// --<base>-stdin flag: --password next to --password-stdin, --admin-password
// next to it too, --token next to --token-stdin.
func stdinCounterpart(name string, bases map[string]string) bool {
	for base := range bases {
		if name == base || strings.HasSuffix(name, "-"+base) {
			return true
		}
	}
	return false
}

// stdinField is the field of a --<base>-stdin flag: the secret itself,
// piped to the command instead of put on its command line. A list twin
// (--cred next to --cred-stdin) or a usage that speaks of KEY=VALUE makes it
// a list: the command reads one item per line.
func stdinField(fs *pflag.FlagSet, fl *pflag.Flag) *field {
	base := strings.TrimSuffix(fl.Name, "-stdin")
	fd := &field{kind: kindText, name: base, label: base + " (stdin)", usage: fl.Usage, secret: true, stdin: fl.Name, typ: "string"}
	twin := fs.Lookup(base)
	if (twin != nil && listType(twin.Value.Type())) || strings.Contains(fl.Usage, "KEY=VALUE") {
		fd.kind, fd.typ = kindList, "stringSlice"
		// The command takes KEY=VALUE lines: the form checks them first.
		fd.pairs = strings.Contains(fl.Usage, "KEY=VALUE")
		if twin != nil && twin.Usage != "" {
			// The list flag says what the values are: a provider's keys.
			fd.usage = twin.Usage
			fd.pairs = fd.pairs || strings.Contains(twin.Usage, "KEY=VALUE")
		}
	}
	return fd
}

// offerPHP turns a --php field into a choice of the branches installed on
// the server, as the web UI's select is. "" is the command's default (the
// newest installed) where the form starts without a branch (a new site);
// a site that has one is not offered "no branch", which an empty --php
// would not set anyway. What the field holds stays a choice too (a site on
// a branch that is being removed).
func (fd *field) offerPHP(versions []string) {
	cur := fd.value()
	fd.kind = kindEnum
	fd.choices = nil
	if fd.initial == "" {
		fd.choices = append(fd.choices, "")
	}
	fd.choices = append(fd.choices, versions...)
	// The value the form started with stays reachable: choosing it again
	// means "no change".
	for _, v := range []string{fd.initial, cur} {
		if v != "" && !slices.Contains(fd.choices, v) {
			fd.choices = append(fd.choices, v)
		}
	}
	fd.val = cur
}

// phpField is the form's --php field, nil when it has none.
func (f *form) phpField() *field {
	for _, fd := range f.fields {
		if !fd.arg && fd.stdin == "" && fd.name == "php" && fd.typ == "string" {
			return fd
		}
	}
	return nil
}

// refreshPHP gives an open form the branches installed now: the list came
// after the form opened, or it has changed since. A field already typed into
// stays as it is.
func (f *form) refreshPHP(versions []string) {
	fd := f.phpField()
	if fd == nil || len(versions) == 0 {
		return
	}
	if fd.kind == kindEnum || (fd.kind == kindText && fd.value() == fd.initial) {
		fd.offerPHP(versions)
	}
}

func numberType(t string) bool {
	switch t {
	case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "count", "float32", "float64":
		return true
	}
	return false
}

func listType(t string) bool {
	switch t {
	case "stringSlice", "stringArray", "intSlice", "int32Slice", "int64Slice", "uintSlice", "float64Slice", "boolSlice", "durationSlice":
		return true
	}
	return false
}

func flagField(path string, fl *pflag.Flag) *field {
	fd := &field{name: fl.Name, label: "--" + fl.Name, usage: fl.Usage, typ: fl.Value.Type()}
	def := fl.DefValue
	switch {
	case fd.typ == "bool":
		fd.kind = kindBool
	case numberType(fd.typ):
		fd.kind = kindNumber
	case listType(fd.typ):
		fd.kind = kindList
		fd.array = fd.typ == "stringArray"
		def = strings.TrimSuffix(strings.TrimPrefix(def, "["), "]")
	default:
		fd.kind = kindText
		if ch := flagChoices(path, fl.Name, fl.Usage, def); len(ch) > 0 {
			fd.kind = kindEnum
			fd.choices = ch
			has := false
			for _, c := range ch {
				has = has || c == def
			}
			if !has {
				fd.choices = append([]string{def}, ch...)
			}
		}
	}
	if fd.kind == kindText || fd.kind == kindList {
		// The variables of a backup repository are cloud credentials
		// (AWS_SECRET_ACCESS_KEY=…): the panel keeps them encrypted.
		fd.secret = secretFlag(fl.Name, fl.Usage) || (path == "backup target add" && fl.Name == "env")
	}
	if ann := fl.Annotations[cobra.BashCompOneRequiredFlag]; len(ann) > 0 && ann[0] == "true" {
		fd.required = true
		fd.label += "*"
	}
	fd.setValue(def)
	fd.initial = fd.value()
	return fd
}

// secretFlag tells the flags whose value must not be shown: passwords,
// tokens, secrets, DNS credentials, a private key given as text (a key file
// path or a public key is fine to show).
func secretFlag(name, usage string) bool {
	base := name
	if i := strings.LastIndexByte(name, '-'); i >= 0 {
		base = name[i+1:]
	}
	switch base {
	case "password", "token", "secret", "cred":
		return true
	case "key":
		// The usage is in the language of the terminal, like these words.
		u := strings.ToLower(usage)
		for _, w := range []string{"file", "path", "public", "~/", T("файл", "file"), T("путь", "path"), T("публичн", "public")} {
			if strings.Contains(u, w) {
				return false
			}
		}
		return true
	}
	return false
}

func (f *form) firstFocus() int {
	for i, fd := range f.fields {
		if fd.required && fd.value() == "" {
			return i
		}
	}
	for i, fd := range f.fields {
		if !(fd.arg && fd.value() != "") && !fd.forced {
			return i
		}
	}
	return 0
}

// complete says whether the command can run as it is: what a quick action
// checks before skipping the form.
func (f *form) complete() bool {
	_, _, _, err := f.build()
	return err == nil
}

// build validates the fields and returns the arguments after the binary,
// the same words with secrets masked for display, and what goes to stdin.
func (f *form) build() (argv, shown []string, stdin string, err error) {
	words := strings.Fields(f.path)
	argv = append(argv, words...)
	shown = append(shown, words...)
	var pos []string
	var missing, missingArgs []string
	gap := ""
	for _, fd := range f.fields {
		if !fd.arg {
			continue
		}
		v := strings.TrimSpace(fd.value())
		if v == "" {
			if fd.required {
				missingArgs = append(missingArgs, fd.label)
			}
			if gap == "" {
				gap = fd.label
			}
			continue
		}
		if gap != "" {
			return nil, nil, "", fmt.Errorf(T("сначала заполните %s", "fill in %s first"), gap)
		}
		if fd.variadic {
			items, werr := splitWords(v)
			if werr != nil {
				return nil, nil, "", fmt.Errorf(T("%s: незакрытая кавычка", "%s: unclosed quote"), fd.label)
			}
			pos = append(pos, items...)
		} else {
			// A stray space typed around a name is not part of it.
			pos = append(pos, v)
		}
	}
	var flags, flagsShown []string
	for _, fd := range f.fields {
		if fd.arg {
			continue
		}
		v := fd.value()
		if fd.required && strings.TrimSpace(v) == "" {
			missing = append(missing, fd.label)
		}
		if fd.kind == kindNumber && strings.TrimSpace(v) != "" {
			if _, perr := strconv.ParseFloat(strings.TrimSpace(v), 64); perr != nil {
				return nil, nil, "", fmt.Errorf(T("%s: нужно число", "%s: must be a number"), fd.label)
			}
		}
		if !fd.passes() || fd.stdin != "" {
			continue
		}
		if fd.kind == kindText && strings.TrimSpace(v) == "-" && strings.Contains(fd.usage, "stdin") {
			// "-" means "read stdin", and a command run from the menu gets
			// none: mp site nginx --set - would write empty directives.
			return nil, nil, "", fmt.Errorf(T("%s: «-» (stdin) из меню не передать — укажите файл", "%s: “-” (stdin) cannot be given from the menu — name a file"), fd.label)
		}
		a, s := fd.render()
		flags = append(flags, a...)
		flagsShown = append(flagsShown, s...)
	}
	// One stdin: a command reads its one-line secrets first, in the order it
	// declares their -stdin flags (mp user set: the current password, then
	// the new one), and then the lines of a list (mp backup target add
	// --password-stdin --env-stdin); two lists it could not tell apart.
	var piped []string
	var secrets []*field
	var list *field
	clash := false
	for _, fd := range f.fields {
		if fd.stdin == "" || !fd.passes() {
			continue
		}
		flags = append(flags, "--"+fd.stdin)
		flagsShown = append(flagsShown, "--"+fd.stdin)
		piped = append(piped, fd.name)
		if fd.pipedList() {
			clash = clash || list != nil
			list = fd
		} else {
			secrets = append(secrets, fd)
		}
	}
	if clash {
		return nil, nil, "", fmt.Errorf(T("через stdin можно передать не больше одного списка: %s", "stdin carries at most one list: %s"), strings.Join(piped, ", "))
	}
	// Several one-line secrets go only to a command that says in which
	// order it reads them (mp user set: the current password, then the new
	// one); otherwise which line is which would be a guess.
	if len(secrets) > 1 {
		order := strings.Split(f.cmd.Annotations[StdinOrderAnnotation], ",")
		pos := map[string]int{}
		for i, name := range order {
			pos[strings.TrimSpace(name)] = i + 1
		}
		for _, fd := range secrets {
			if pos[fd.stdin] == 0 {
				var names []string
				for _, x := range secrets {
					names = append(names, x.name)
				}
				return nil, nil, "", fmt.Errorf(T("через stdin передаётся один секрет за раз: %s", "only one secret goes through stdin at a time: %s"), strings.Join(names, ", "))
			}
		}
		sort.SliceStable(secrets, func(i, j int) bool { return pos[secrets[i].stdin] < pos[secrets[j].stdin] })
	}
	var lines []string
	for _, secret := range secrets {
		lines = append(lines, secret.value())
	}
	if list != nil {
		items := list.items()
		if list.pairs {
			for i, it := range items {
				// The item is not shown: it may be the secret itself.
				if k, _, ok := strings.Cut(it, "="); !ok || strings.TrimSpace(k) == "" {
					return nil, nil, "", fmt.Errorf(T("%s: элемент %d — не KEY=VALUE", "%s: item %d is not KEY=VALUE"), list.label, i+1)
				}
			}
		}
		lines = append(lines, items...)
	}
	stdin = strings.Join(lines, "\n")
	flags = append(flags, f.extra...)
	flagsShown = append(flagsShown, f.extra...)
	if len(missing) > 0 {
		return nil, nil, "", fmt.Errorf(T("заполните: %s", "fill in: %s"), strings.Join(missing, ", "))
	}
	// cobra's own rule decides how many arguments are needed: "renew
	// <id|hostname>" also runs with --all and no argument.
	if verr := f.cmd.ValidateArgs(pos); verr != nil {
		if len(missingArgs) > 0 {
			return nil, nil, "", fmt.Errorf(T("заполните: %s", "fill in: %s"), strings.Join(missingArgs, ", "))
		}
		return nil, nil, "", fmt.Errorf(T("аргументы: %v", "arguments: %v"), verr)
	}
	dash := false
	for _, p := range pos {
		dash = dash || strings.HasPrefix(p, "-")
	}
	posShown := make([]string, len(pos))
	for i, p := range pos {
		posShown[i] = shellQuote(p)
	}
	if dash {
		// A value that starts with "-" must not be read as a flag.
		argv = append(append(append(argv, flags...), "--"), pos...)
		shown = append(append(append(shown, flagsShown...), "--"), posShown...)
	} else {
		argv = append(append(argv, pos...), flags...)
		shown = append(append(shown, posShown...), flagsShown...)
	}
	return argv, shown, stdin, nil
}

// render is how a flag field goes on the command line: --name=value for the
// child (a value starting with "-" stays a value), --name value for people.
func (fd *field) render() (argv, shown []string) {
	v := fd.value()
	if !fd.secret && (fd.kind == kindText || fd.kind == kindNumber) {
		// A stray space typed around a value is not part of it; a secret
		// is passed exactly as typed.
		v = strings.TrimSpace(v)
	}
	name := "--" + fd.name
	show := func(s string) string {
		if fd.secret && !fd.arg {
			return "•••"
		}
		return shellQuote(s)
	}
	switch fd.kind {
	case kindBool:
		if v == "true" {
			return []string{name}, []string{name}
		}
		return []string{name + "=false"}, []string{name + "=false"}
	case kindList:
		items := splitList(v)
		if len(items) == 0 {
			return []string{name + "="}, []string{name + "=''"}
		}
		if fd.array {
			for _, it := range items {
				argv = append(argv, name+"="+it)
				shown = append(shown, name, show(it))
			}
			return argv, shown
		}
		// A string slice is read as one CSV line: an item with a comma or
		// a quote is quoted, so KEY=a,b stays one variable.
		joined := joinList(items)
		return []string{name + "=" + joined}, []string{name, show(joined)}
	}
	if v == "" || strings.HasPrefix(v, "-") {
		return []string{name + "=" + v}, []string{name + "=" + show(v)}
	}
	return []string{name + "=" + v}, []string{name, show(v)}
}

// line is the equivalent mp command line, secrets masked; it is shown even
// while the form is incomplete.
func (f *form) line() string {
	_, shown, _, err := f.build()
	if err != nil {
		// Show what there is so far.
		words := []string{"mp"}
		words = append(words, strings.Fields(f.path)...)
		for _, fd := range f.fields {
			if fd.arg {
				if v := strings.TrimSpace(fd.value()); v != "" {
					words = append(words, shellQuote(v))
				}
			}
		}
		for _, fd := range f.fields {
			if !fd.arg && fd.passes() {
				if fd.stdin != "" {
					words = append(words, "--"+fd.stdin)
					continue
				}
				_, s := fd.render()
				words = append(words, s...)
			}
		}
		return strings.Join(words, " ")
	}
	if f.yes {
		shown = append(shown, "--yes")
	}
	return "mp " + strings.Join(shown, " ")
}

func (f *form) stdinNote() string {
	var piped []string
	for _, fd := range f.fields {
		if fd.stdin != "" && fd.passes() {
			piped = append(piped, fd.name)
		}
	}
	switch len(piped) {
	case 0:
	case 1:
		return fmt.Sprintf(T("%s передаётся через stdin, не в командной строке", "%s goes through stdin, not on the command line"), piped[0])
	default:
		return fmt.Sprintf(T("%s передаются через stdin, не в командной строке", "%s go through stdin, not on the command line"), strings.Join(piped, T(" и ", " and ")))
	}
	for _, fd := range f.fields {
		// The command has no stdin form of this flag: say what that costs.
		if fd.secret && fd.stdin == "" && !fd.arg && fd.passes() && fd.value() != "" {
			return fmt.Sprintf(T("--%s виден в списке процессов (ps), пока команда работает", "--%s shows in the process list (ps) while the command runs"), fd.name)
		}
	}
	return ""
}

// carry copies the values of the fields another form shares by name: plan →
// run takes the same source.
func (f *form) carry(from *form) {
	for _, src := range from.fields {
		for _, dst := range f.fields {
			if dst.arg == src.arg && dst.name == src.name && dst.kind == src.kind && src.passes() {
				dst.setValue(src.value())
				dst.forced = !dst.arg
			}
		}
	}
}

func (f *form) name() string { return f.title }

// typing: a form takes every key itself (q, r, ? are text there).
func (f *form) typing() bool { return true }

func (f *form) hints(m *model) []hint {
	h := []hint{{"enter", T("далее", "next")}, {"ctrl+s", T("выполнить", "run")}, {"esc", T("отмена", "cancel")}, {"F1", T("справка", "help")}}
	if f.focus < len(f.fields) {
		fd := f.fields[f.focus]
		switch fd.kind {
		case kindBool:
			h = append(h, hint{"space", T("вкл/выкл", "toggle")})
		case kindEnum:
			h = append(h, hint{"←/→", T("выбор", "choose")})
		}
		if fd.secret {
			h = append(h, hint{"ctrl+r", T("показать", "reveal")})
		}
		if !fd.arg && fd.stdin == "" && !fd.forced {
			h = append(h, hint{"ctrl+p", T("передать как есть", "pass as is")})
		}
	}
	return h
}

func (f *form) update(m *model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.PasteMsg:
		if f.focus < len(f.fields) {
			if fd := f.fields[f.focus]; fd.kind != kindBool && fd.kind != kindEnum {
				content := msg.Content
				if fd.pipedList() && strings.ContainsAny(content, "\r\n") {
					// Pasted lines (the variables of a credentials file) are
					// one item each, whatever they hold.
					var lines []string
					for _, l := range strings.FieldsFunc(content, func(r rune) bool { return r == '\n' || r == '\r' }) {
						if l = strings.TrimSpace(l); l != "" {
							lines = append(lines, l)
						}
					}
					content = joinPairs(lines)
					// Next to items already typed they stay items of their own.
					in := &fd.input
					if before := strings.TrimSpace(string(in.runes[:in.pos])); content != "" && before != "" && !strings.HasSuffix(before, ",") && !strings.HasSuffix(before, ";") {
						content = ", " + content
					}
					if after := strings.TrimSpace(string(in.runes[in.pos:])); content != "" && after != "" && !strings.HasPrefix(after, ",") && !strings.HasPrefix(after, ";") {
						content += ", "
					}
				} else {
					// A copied line ends with its line break; in a one-line
					// field it would become a trailing space — part of a
					// password piped to the command.
					content = strings.TrimRight(content, "\r\n")
				}
				fd.input.insert(content)
				f.err = ""
			}
		}
	case tea.KeyPressMsg:
		k := msg.String()
		switch k {
		case "esc":
			m.pop()
			return nil
		case "ctrl+s":
			return m.submit(f)
		case "tab", "down":
			f.move(1)
			return nil
		case "shift+tab", "up":
			f.move(-1)
			return nil
		case "enter":
			if f.focus >= len(f.fields)-1 {
				return m.submit(f)
			}
			f.move(1)
			return nil
		case "ctrl+r":
			f.reveal = !f.reveal
			return nil
		case "ctrl+p":
			if f.focus < len(f.fields) {
				if fd := f.fields[f.focus]; !fd.arg && fd.stdin == "" {
					fd.pinned = !fd.pinned
				}
			}
			return nil
		}
		if f.focus >= len(f.fields) {
			return nil
		}
		fd := f.fields[f.focus]
		f.err = ""
		switch fd.kind {
		case kindBool:
			if k == "space" || k == "left" || k == "right" || k == "x" {
				if fd.val == "true" {
					fd.val = "false"
				} else {
					fd.val = "true"
				}
			}
		case kindEnum:
			switch k {
			case "space", "right":
				fd.cycle(1)
			case "left":
				fd.cycle(-1)
			default:
				if msg.Text != "" {
					fd.jump(msg.Text)
				}
			}
		default:
			fd.input.handle(msg)
		}
	}
	return nil
}

func (f *form) move(d int) {
	n := len(f.fields) + 1
	f.focus = (f.focus + d + n) % n
}

func (fd *field) cycle(d int) {
	if len(fd.choices) == 0 {
		return
	}
	i := 0
	for j, c := range fd.choices {
		if c == fd.val {
			i = j
		}
	}
	fd.val = fd.choices[(i+d+len(fd.choices))%len(fd.choices)]
}

// jump selects the next choice that starts with the typed letter.
func (fd *field) jump(prefix string) {
	start := 0
	for j, c := range fd.choices {
		if c == fd.val {
			start = j + 1
		}
	}
	for i := 0; i < len(fd.choices); i++ {
		c := fd.choices[(start+i)%len(fd.choices)]
		if strings.HasPrefix(c, prefix) {
			fd.val = c
			return
		}
	}
}

func (f *form) view(m *model, w, h int) string {
	var top []string
	if short := f.cmd.Short; short != "" && short != f.title {
		for _, l := range wrap(short, w-2) {
			top = append(top, " "+styleMuted.Render(l))
		}
	}
	if f.act.note != nil {
		if n := f.act.note(f.vals); n != "" {
			for _, l := range wrap(n, w-2) {
				top = append(top, " "+styleWarn.Render(l))
			}
		}
	}
	if len(top) > 3 {
		top = top[:3]
	}

	// The bottom block: help for the focused field, the command line, the error.
	var bottom []string
	bottom = append(bottom, styleMuted.Render(strings.Repeat("─", max(0, w))))
	if f.focus < len(f.fields) {
		fd := f.fields[f.focus]
		help := fd.usage
		if fd.arg {
			help = T("аргумент", "argument")
			if fd.required {
				help += T(", обязательный", ", required")
			}
			if fd.variadic {
				help += T("; несколько значений через пробел", "; several values separated by spaces")
			}
		} else if fd.kind == kindList {
			help += T(" (через запятую)", " (comma-separated)")
		}
		switch {
		case fd.pipedList():
			help = fmt.Sprintf(T("%s — через stdin (--%s), по одному в строке; здесь — через запятую или «;», значение с ними — в кавычках", "%s — through stdin (--%s), one per line; here separated by commas or semicolons, a value with one in double quotes"), fd.usage, fd.stdin)
		case fd.stdin != "":
			help = fmt.Sprintf(T("%s — передаётся через stdin (--%s)", "%s — passed through stdin (--%s)"), fd.usage, fd.stdin)
		}
		// A piped list carries how to type it: a line more for that.
		lines := 2
		if fd.pipedList() {
			lines = 3
		}
		for i, l := range wrap(help, w-2) {
			if i == lines {
				break
			}
			bottom = append(bottom, " "+styleMuted.Render(l))
		}
	}
	line := f.line()
	cl := wrap(line, w-2)
	if len(cl) > 3 {
		cl = append(cl[:2], truncate(strings.Join(cl[2:], " "), w-2))
	}
	for _, l := range cl {
		bottom = append(bottom, " "+styleKey.Render(l))
	}
	if note := f.stdinNote(); note != "" {
		bottom = append(bottom, " "+styleMuted.Render(truncate(note, w-2)))
	}
	if f.err != "" {
		for i, l := range wrap(f.err, w-2) {
			if i == 2 {
				break
			}
			bottom = append(bottom, " "+styleErr.Render(l))
		}
	}

	// Fields and the run button, scrolled to keep the focus visible.
	rows := len(f.fields) + 1
	avail := h - len(top) - len(bottom) - 1
	if avail < 3 {
		avail = 3
	}
	if f.focus < f.offset {
		f.offset = f.focus
	}
	if f.focus >= f.offset+avail {
		f.offset = f.focus - avail + 1
	}
	if f.offset > rows-avail {
		f.offset = max(0, rows-avail)
	}
	labelW := 8
	for _, fd := range f.fields {
		labelW = max(labelW, textWidth(fd.label))
	}
	labelW = min(labelW, max(10, w/3))
	valueW := max(8, w-labelW-6)

	var mid []string
	for i := f.offset; i < rows && len(mid) < avail; i++ {
		if i == len(f.fields) {
			btn := " " + T("Выполнить", "Run") + " "
			if f.focus == i {
				mid = append(mid, "   "+styleCursor.Render("▶"+btn))
			} else {
				mid = append(mid, "   "+styleKey.Render("▶")+btn)
			}
			continue
		}
		fd := f.fields[i]
		gutter := " "
		if fd.passes() {
			gutter = styleKey.Render("●")
			if fd.pinned {
				gutter = styleWarn.Render("●")
			}
		}
		focused := f.focus == i
		label := fit(fd.label, labelW)
		if focused {
			label = styleTitle.Render(label)
		} else if fd.required {
			label = styleBold.Render(label)
		}
		mark := " "
		if focused {
			mark = styleKey.Render("▸")
		}
		mid = append(mid, mark+gutter+" "+label+"  "+fd.view(valueW, focused, f.reveal))
	}
	for len(mid) < avail {
		mid = append(mid, "")
	}
	out := append(append(top, ""), mid...)
	return strings.Join(append(out, bottom...), "\n")
}

func (fd *field) view(w int, focus, reveal bool) string {
	switch fd.kind {
	case kindBool:
		box := "[ ]"
		if fd.val == "true" {
			box = "[x]"
		}
		if focus {
			return styleCursor.Render(box)
		}
		return box
	case kindEnum:
		v := fd.val
		if v == "" {
			v = "—"
		}
		if focus {
			return styleKey.Render("‹ ") + styleCursor.Render(v) + styleKey.Render(" ›") + styleMuted.Render("  "+truncate(strings.Join(fd.choices, " "), max(0, w-textWidth(v)-6)))
		}
		return v
	}
	if !focus && fd.value() == "" {
		return ""
	}
	return fd.input.view(w, focus, fd.secret && !reveal)
}

// fill puts a command line typed in the palette into the form: the words
// into the argument fields, the flags into theirs (passed as typed). ready
// is false when the form still needs something the line could not carry: a
// secret for stdin.
func (f *form) fill(words []string) (ready bool, err error) {
	c := f.cmd
	c.InheritedFlags()
	fs := c.Flags()
	root := c.Root()
	var pos []string
	vals := map[string][]string{}
	var order []string
	for i := 0; i < len(words); i++ {
		w := words[i]
		if w == "--" {
			pos = append(pos, words[i+1:]...)
			break
		}
		if !strings.HasPrefix(w, "-") || w == "-" {
			pos = append(pos, w)
			continue
		}
		var fl *pflag.Flag
		val, hasVal := "", false
		if strings.HasPrefix(w, "--") {
			name, v, ok := strings.Cut(w[2:], "=")
			fl, val, hasVal = fs.Lookup(name), v, ok
		} else {
			fl = fs.ShorthandLookup(w[1:2])
			if len(w) > 2 {
				val, hasVal = strings.TrimPrefix(w[2:], "="), true
			}
		}
		if fl == nil {
			return false, fmt.Errorf(T("неизвестный флаг: %s", "unknown flag: %s"), w)
		}
		if root.PersistentFlags().Lookup(fl.Name) == fl {
			switch fl.Name {
			case "json", "no-wait":
				f.extra = append(f.extra, w)
				continue
			}
			return false, fmt.Errorf(T("--%s задаётся при запуске mp, а не в команде", "--%s is given when mp starts, not in a command"), fl.Name)
		}
		if !hasVal {
			if fl.Value.Type() == "bool" || fl.NoOptDefVal != "" {
				val = "true"
				if fl.NoOptDefVal != "" {
					val = fl.NoOptDefVal
				}
			} else {
				i++
				if i >= len(words) {
					return false, fmt.Errorf(T("флагу %s нужно значение", "the flag %s needs a value"), w)
				}
				val = words[i]
			}
		}
		if _, seen := vals[fl.Name]; !seen {
			order = append(order, fl.Name)
		}
		vals[fl.Name] = append(vals[fl.Name], val)
	}
	ai := 0
	for _, fd := range f.fields {
		if !fd.arg || ai >= len(pos) {
			continue
		}
		if fd.variadic {
			quoted := make([]string, 0, len(pos)-ai)
			for _, p := range pos[ai:] {
				quoted = append(quoted, shellQuote(p))
			}
			fd.setValue(strings.Join(quoted, " "))
			ai = len(pos)
			continue
		}
		fd.setValue(pos[ai])
		ai++
	}
	if ai < len(pos) {
		return false, fmt.Errorf(T("лишние аргументы: %s", "unexpected arguments: %s"), strings.Join(pos[ai:], " "))
	}
	ready = true
	for _, name := range order {
		list := vals[name]
		if name == "yes" {
			continue // the TUI asks itself
		}
		var fd *field
		for i, x := range f.fields {
			switch {
			case x.arg:
			case x.stdin == name:
				// --password-stdin typed: the secret itself goes in the form.
				ready, f.focus = false, i
			case x.stdin == "" && x.name == name:
				fd = x
			case x.stdin != "" && (name == x.name || strings.HasSuffix(name, "-"+x.name)):
				// --password typed next to a --password-stdin: it goes through stdin.
				fd = x
			}
		}
		if strings.HasSuffix(name, "-stdin") {
			continue
		}
		if fd == nil {
			return false, fmt.Errorf(T("флаг --%s здесь недоступен", "the flag --%s is not available here"), name)
		}
		switch {
		case fd.pipedList():
			// --cred A=1,B=2 typed next to --cred-stdin: the items as the
			// list flag would read them (CSV, or one per --x of an array),
			// so that a value with a semicolon stays one item.
			var items []string
			for _, v := range list {
				if fl := fs.Lookup(name); fl != nil && fl.Value.Type() == "stringArray" {
					items = append(items, v)
				} else {
					items = append(items, splitList(v)...)
				}
			}
			fd.setValue(joinPairs(items))
		case fd.kind == kindList:
			fd.setValue(strings.Join(list, ","))
		default:
			fd.setValue(list[len(list)-1])
		}
		if fd.stdin == "" {
			fd.pinned = true
		}
	}
	return ready, nil
}

// changed says whether some flag goes on the command line beyond what the
// screen bound itself.
func (f *form) changed() bool {
	for _, fd := range f.fields {
		if !fd.arg && !fd.forced && fd.passes() {
			return true
		}
	}
	return false
}
