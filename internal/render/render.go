// Package render turns the panel's desired state into configuration files
// from embedded templates, honouring overrides under /etc/monopanel/templates.
package render

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"text/template"

	"monopanel/templates"
)

// Renderer renders templates.
type Renderer struct {
	overrideDir string
	funcs       template.FuncMap
}

// New creates a renderer; overrideDir may be empty.
func New(overrideDir string) *Renderer {
	return &Renderer{
		overrideDir: overrideDir,
		funcs: template.FuncMap{
			"join": strings.Join,
			"indent": func(n int, s string) string {
				pad := strings.Repeat(" ", n)
				return pad + strings.ReplaceAll(s, "\n", "\n"+pad)
			},
			"default": func(def, v string) string {
				if v == "" {
					return def
				}
				return v
			},
		},
	}
}

// Source returns the template bytes and whether an override was used.
func (r *Renderer) Source(name string) ([]byte, bool, error) {
	if r.overrideDir != "" {
		if b, err := os.ReadFile(filepath.Join(r.overrideDir, filepath.FromSlash(name))); err == nil {
			return b, true, nil
		}
	}
	b, err := fs.ReadFile(templates.FS, name)
	if err != nil {
		return nil, false, fmt.Errorf("template %s: %w", name, err)
	}
	return b, false, nil
}

// Render executes a template by name (e.g. "nginx/site.conf.tmpl").
func (r *Renderer) Render(name string, data any) (string, error) {
	if err := oneLineValues(data); err != nil {
		return "", fmt.Errorf("render %s: %w", name, err)
	}
	src, _, err := r.Source(name)
	if err != nil {
		return "", err
	}
	t, err := template.New(path.Base(name)).Funcs(r.funcs).Option("missingkey=error").Parse(string(src))
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render %s: %w", name, err)
	}
	return buf.String(), nil
}

// RenderSet executes a template together with extra files that only define
// named blocks (e.g. nginx/presets/*.conf.tmpl), so the main template can
// {{ template "preset-wordpress" . }}.
func (r *Renderer) RenderSet(name string, data any, extra ...string) (string, error) {
	if err := oneLineValues(data); err != nil {
		return "", fmt.Errorf("render %s: %w", name, err)
	}
	src, _, err := r.Source(name)
	if err != nil {
		return "", err
	}
	t, err := template.New(path.Base(name)).Funcs(r.funcs).Option("missingkey=error").Parse(string(src))
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", name, err)
	}
	for _, e := range extra {
		b, _, err := r.Source(e)
		if err != nil {
			return "", err
		}
		if _, err := t.New(path.Base(e)).Parse(string(b)); err != nil {
			return "", fmt.Errorf("parse %s: %w", e, err)
		}
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, path.Base(name), data); err != nil {
		return "", fmt.Errorf("render %s: %w", name, err)
	}
	return buf.String(), nil
}

// oneLineValues refuses data with a string that holds a line break, a
// carriage return or NUL, and names the field. The templates use text/template,
// which escapes nothing, and in every format the panel renders — nginx and
// Apache, php-fpm pools, systemd units, postfix and dovecot, the crontab — a
// new line starts a new directive: such a value would add one of its author's
// choosing (an access_log the nginx master opens as root, a php-fpm slowlog, an
// ExecStartPre=+). The API validates the values long before they get here; this
// is the last word for one that slips past it, from any caller.
func oneLineValues(data any) error {
	v := reflect.ValueOf(data)
	if !v.IsValid() {
		return nil
	}
	root := v.Type().Name()
	if v.Kind() == reflect.Pointer && !v.IsNil() {
		root = v.Elem().Type().Name()
	}
	if root == "" {
		root = "data"
	}
	return checkOneLine(v, root)
}

func checkOneLine(v reflect.Value, at string) error {
	switch v.Kind() {
	case reflect.String:
		if strings.ContainsAny(v.String(), "\n\r\x00") {
			return fmt.Errorf("%s holds a line break, a carriage return or NUL, which would add a line of its own to the file", at)
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			return checkOneLine(v.Elem(), at)
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			if f := t.Field(i); f.IsExported() {
				if err := checkOneLine(v.Field(i), at+"."+f.Name); err != nil {
					return err
				}
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := checkOneLine(v.Index(i), fmt.Sprintf("%s[%d]", at, i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
		for _, k := range keys {
			at := fmt.Sprintf("%s[%q]", at, fmt.Sprint(k.Interface()))
			if err := checkOneLine(k, at); err != nil {
				return err
			}
			if err := checkOneLine(v.MapIndex(k), at); err != nil {
				return err
			}
		}
	}
	return nil
}

// ListDir returns the file names of an embedded template directory.
func (r *Renderer) ListDir(dir string) ([]string, error) {
	entries, err := fs.ReadDir(templates.FS, dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// List returns every embedded template path.
func (r *Renderer) List() ([]string, error) {
	var out []string
	err := fs.WalkDir(templates.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !strings.HasSuffix(p, ".go") {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// Snippets returns the static files of a directory (name -> content),
// e.g. "nginx/snippets".
func (r *Renderer) Snippets(dir string) (map[string]string, error) {
	entries, err := fs.ReadDir(templates.FS, dir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, _, err := r.Source(path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out[e.Name()] = string(b)
	}
	return out, nil
}
