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
