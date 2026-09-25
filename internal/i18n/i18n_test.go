package i18n

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestFromEnv(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{}, "en"},
		{map[string]string{"LANG": "C.UTF-8"}, "en"},
		{map[string]string{"LANG": "POSIX", "LANGUAGE": "ru"}, "en"}, // gettext ignores LANGUAGE under C
		{map[string]string{"LANG": "ru_RU.UTF-8"}, "ru"},
		{map[string]string{"LANG": "uk_UA.UTF-8"}, "ru"},
		{map[string]string{"LANG": "kk_KZ"}, "ru"},
		{map[string]string{"LANG": "ro_MD.UTF-8"}, "ru"},
		{map[string]string{"LANG": "ro_RO.UTF-8"}, "en"},
		{map[string]string{"LANG": "de_DE.UTF-8"}, "en"},
		{map[string]string{"LANG": "en_US.UTF-8", "LC_ALL": "ru_RU.UTF-8"}, "ru"},
		{map[string]string{"LANG": "ru_RU.UTF-8", "LC_MESSAGES": "en_GB.UTF-8"}, "en"},
		{map[string]string{"LANG": "en_US.UTF-8", "LANGUAGE": "ru:en"}, "ru"},
		{map[string]string{"LANG": "ru_RU.UTF-8", "MP_LANG": "en"}, "en"},
		{map[string]string{"LANG": "C.UTF-8", "MP_LANG": "ru"}, "ru"},
		{map[string]string{"LANG": "sr_RS@latin"}, "en"},
	} {
		if got := FromEnv(func(k string) string { return c.env[k] }); got != c.want {
			t.Errorf("%v → %s, want %s", c.env, got, c.want)
		}
	}
	Set("ru")
	if T("да", "yes") != "да" || !RU() {
		t.Fatal("Set(ru)")
	}
	Set("en")
	if T("да", "yes") != "yes" || RU() {
		t.Fatal("Set(en)")
	}
}

// The packages that print for a person: every Russian text in them goes
// through T with an English pair, and a format string keeps the same verbs in
// both languages — a lost %s would print %!s(MISSING) in one of them only.
var translated = []string{"../cli", "../tui", "../setup"}

// untranslated are Russian literals that must stay as they are, with why.
var untranslated = map[string]string{
	"выключено": "cli/migrate.go: the mark a server before the English messages put on a disabled cron job",
}

var (
	cyrillic = regexp.MustCompile(`[А-Яа-яЁё]`)
	verbRe   = regexp.MustCompile(`%(\[\d+\])?[-+# 0]*(\d+|\*)?(\.(\d+|\*)?)?[a-zA-Z%]`)
)

func verbs(s string) (list []string, indexed bool) {
	for _, v := range verbRe.FindAllStringSubmatch(s, -1) {
		if v[0] == "%%" {
			continue
		}
		if v[1] != "" {
			indexed = true
		}
		list = append(list, v[1]+v[0][len(v[0])-1:])
	}
	return list, indexed
}

func stringValue(e ast.Expr) (string, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		v := constant.MakeFromLiteral(e.Value, e.Kind, 0)
		if v.Kind() != constant.String {
			return "", false
		}
		return constant.StringVal(v), true
	case *ast.BinaryExpr:
		l, ok1 := stringValue(e.X)
		r, ok2 := stringValue(e.Y)
		return l + r, ok1 && ok2 && e.Op == token.ADD
	case *ast.ParenExpr:
		return stringValue(e.X)
	}
	return "", false
}

func isT(call *ast.CallExpr) bool {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name == "T"
	case *ast.SelectorExpr:
		x, ok := f.X.(*ast.Ident)
		return ok && x.Name == "i18n" && f.Sel.Name == "T"
	}
	return false
}

func TestTexts(t *testing.T) {
	fset := token.NewFileSet()
	pairs := 0
	for _, dir := range translated {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: %v", dir, err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			f, err := parser.ParseFile(fset, file, src, 0)
			if err != nil {
				t.Fatal(err)
			}
			// A package-level var is set before Detect runs and would always
			// be English: texts belong in functions.
			for _, d := range f.Decls {
				if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.VAR {
					ast.Inspect(gd, func(n ast.Node) bool {
						if call, ok := n.(*ast.CallExpr); ok && isT(call) {
							t.Errorf("%s: T in a package-level var runs before the language is known", fset.Position(call.Pos()))
						}
						return true
					})
				}
			}
			inT := map[token.Pos]bool{}
			ast.Inspect(f, func(n ast.Node) bool {
				// The package's own T passes its parameters on to i18n.T.
				if fd, ok := n.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "T" {
					return false
				}
				call, ok := n.(*ast.CallExpr)
				if !ok || !isT(call) {
					return true
				}
				pos := fset.Position(call.Pos())
				if len(call.Args) != 2 {
					t.Errorf("%s: T takes the Russian and the English text", pos)
					return true
				}
				ru, ok1 := stringValue(call.Args[0])
				en, ok2 := stringValue(call.Args[1])
				if !ok1 || !ok2 {
					t.Errorf("%s: T needs literal texts, so that this test can read them", pos)
					return true
				}
				pairs++
				for _, a := range call.Args {
					ast.Inspect(a, func(n ast.Node) bool {
						if lit, ok := n.(*ast.BasicLit); ok {
							inT[lit.Pos()] = true
						}
						return true
					})
				}
				if cyrillic.MatchString(en) {
					t.Errorf("%s: the English text is Russian: %q", pos, en)
				}
				rv, ri := verbs(ru)
				ev, ei := verbs(en)
				if ri || ei {
					slices.Sort(rv)
					slices.Sort(ev)
				}
				if !slices.Equal(rv, ev) {
					t.Errorf("%s: verbs differ: %q %v / %q %v", pos, ru, rv, en, ev)
				}
				return true
			})
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING || inT[lit.Pos()] {
					return true
				}
				if s, _ := stringValue(lit); cyrillic.MatchString(s) {
					if _, ok := untranslated[s]; !ok {
						t.Errorf("%s: Russian text outside T: %s", fset.Position(lit.Pos()), lit.Value)
					}
				}
				return true
			})
		}
	}
	t.Logf("%d texts in two languages", pairs)
}
