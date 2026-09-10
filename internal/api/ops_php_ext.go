package api

import (
	"context"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/osprofile"
	"monopanel/internal/store"
)

// criticalExtensions are the ones a typical site stops working without. They
// can still be switched off — the administrator is root here — but the panel
// says so first.
var criticalExtensions = map[string]bool{
	"mysqlnd": true, "pdo": true, "pdo_mysql": true, "mysqli": true,
	"mbstring": true, "xml": true, "dom": true, "simplexml": true,
	"session": true, "json": true, "curl": true, "opcache": true,
}

type phpExtInput struct {
	Version string `path:"version" pattern:"^[578]\\.[0-9]$"`
}

type phpExtOutput struct {
	Body apitypes.PHPExtensions
}

type phpExtToggleInput struct {
	Version string `path:"version" pattern:"^[578]\\.[0-9]$"`
	Body    apitypes.PHPExtensionRequest
}

// phpLayoutFor resolves an installed branch and its layout.
func (s *Server) phpLayoutFor(ctx context.Context, version string) (*store.PHPVersion, *osprofile.PHPLayout, error) {
	row, err := s.db.GetPHPVersion(ctx, version)
	if err != nil {
		return nil, nil, huma.Error404NotFound("PHP " + version + " не установлен")
	}
	if row.Status != store.PHPInstalled {
		return nil, nil, huma.Error409Conflict("PHP " + version + ": " + row.Status)
	}
	layout := osprofile.PHP(s.profile, version)
	if layout == nil {
		return nil, nil, huma.Error501NotImplemented("управление расширениями не поддержано на этой ОС")
	}
	return row, layout, nil
}

// phpModules reads what exists and what is switched on. Everything with an ini
// in mods-available can be enabled; what php-fpm actually loads is exactly the
// content of its conf.d — phpquery answers from Debian's own registry and goes
// on listing a module after phpdismod removed it from the SAPI. On EL the
// branch's php.d is the whole story: one ini per extension, on when its
// extension= line is live.
func (s *Server) phpModules(ctx context.Context, version string) ([]apitypes.PHPExtension, error) {
	if s.profile.Family() == osprofile.FamilyRHEL {
		return s.phpModulesEL(ctx, version)
	}
	dir, err := s.agent.ListDir(ctx, "/etc/php/"+version+"/mods-available")
	if err != nil {
		return nil, huma.Error502BadGateway(err.Error())
	}
	enabled := map[string]bool{}
	if confd, err := s.agent.ListDir(ctx, "/etc/php/"+version+"/fpm/conf.d"); err == nil {
		for _, e := range confd.Entries {
			// Имена вида "20-imagick.ini": приоритет спереди, модуль дальше.
			name := strings.TrimSuffix(e.Name, ".ini")
			if _, rest, ok := strings.Cut(name, "-"); ok {
				name = rest
			}
			enabled[name] = true
		}
	}
	out := make([]apitypes.PHPExtension, 0, len(dir.Entries))
	for _, e := range dir.Entries {
		if e.IsDir || !strings.HasSuffix(e.Name, ".ini") {
			continue
		}
		name := strings.TrimSuffix(e.Name, ".ini")
		out = append(out, apitypes.PHPExtension{Name: name, Enabled: enabled[name], Critical: criticalExtensions[name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Server) registerPHPExtensions() {
	huma.Register(s.api, huma.Operation{
		OperationID: "php-extensions", Method: http.MethodGet, Path: "/php/versions/{version}/extensions",
		Summary: "Extensions of a branch and whether each is switched on", Tags: []string{"php"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *phpExtInput) (*phpExtOutput, error) {
		row, _, err := s.phpLayoutFor(ctx, in.Version)
		if err != nil {
			return nil, err
		}
		mods, err := s.phpModules(ctx, in.Version)
		if err != nil {
			return nil, err
		}
		return &phpExtOutput{Body: apitypes.PHPExtensions{Version: row.Version, Extensions: mods}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "php-extension-toggle", Method: http.MethodPost, Path: "/php/versions/{version}/extensions",
		Summary: "Switch an extension on or off for the whole branch", Tags: []string{"php"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *phpExtToggleInput) (*phpExtOutput, error) {
		p := principalFrom(ctx)
		_, layout, err := s.phpLayoutFor(ctx, in.Version)
		if err != nil {
			return nil, err
		}
		mods, err := s.phpModules(ctx, in.Version)
		if err != nil {
			return nil, err
		}
		known := false
		for _, m := range mods {
			if m.Name == in.Body.Name {
				known = true
			}
		}
		if !known {
			return nil, huma.Error422UnprocessableEntity("нет такого расширения у PHP " + in.Version + ": " + in.Body.Name)
		}

		if s.profile.Family() == osprofile.FamilyRHEL {
			if err := s.phpToggleEL(ctx, layout, in.Body.Name, in.Body.Enabled); err != nil {
				return nil, err
			}
		} else {
			// phpenmod/phpdismod правят символические ссылки в conf.d обоих SAPI;
			// изменения подхватываются только после перезапуска php-fpm.
			tool := "phpdismod"
			if in.Body.Enabled {
				tool = "phpenmod"
			}
			res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: tool, Args: []string{"-v", in.Version, in.Body.Name}, TimeoutSeconds: 60})
			if err != nil {
				return nil, huma.Error502BadGateway(err.Error())
			}
			if res.ExitCode != 0 {
				return nil, huma.Error422UnprocessableEntity(strings.TrimSpace(res.Output))
			}
		}
		if _, err := s.agent.Service(ctx, layout.FPMService, "reload-or-restart"); err != nil {
			return nil, huma.Error502BadGateway(err.Error())
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "php.extension", Target: in.Version + ":" + in.Body.Name,
			IP: requestInfo(ctx).IP, Details: map[string]any{"enabled": in.Body.Enabled}})

		mods, err = s.phpModules(ctx, in.Version)
		if err != nil {
			return nil, err
		}
		return &phpExtOutput{Body: apitypes.PHPExtensions{Version: in.Version, Extensions: mods}}, nil
	})
}

// extLineRe matches the line that loads an extension, live or commented out.
var extLineRe = regexp.MustCompile(`^(\s*)(;\s*)?((?:zend_)?extension\s*=.*)$`)

// phpModulesEL lists the branch's php.d (Remi: /etc/opt/remi/phpXY/php.d):
// "20-imagick.ini" is the extension imagick, enabled while its extension=
// line is not commented out.
func (s *Server) phpModulesEL(ctx context.Context, version string) ([]apitypes.PHPExtension, error) {
	layout := osprofile.PHP(s.profile, version)
	if layout == nil || len(layout.IniDirs) == 0 {
		return nil, huma.Error501NotImplemented("управление расширениями не поддержано на этой ОС")
	}
	dir, err := s.agent.ListDir(ctx, layout.IniDirs[0])
	if err != nil {
		return nil, huma.Error502BadGateway(err.Error())
	}
	out := make([]apitypes.PHPExtension, 0, len(dir.Entries))
	for _, e := range dir.Entries {
		if e.IsDir || !strings.HasSuffix(e.Name, ".ini") {
			continue
		}
		name := strings.TrimSuffix(e.Name, ".ini")
		if _, rest, ok := strings.Cut(name, "-"); ok {
			name = rest
		}
		if name == "monopanel" || strings.HasPrefix(name, "monopanel") {
			continue // the panel's own settings, not an extension
		}
		enabled := false
		if f, err := s.agent.ReadFile(ctx, path.Join(layout.IniDirs[0], e.Name), 64*1024); err == nil {
			for _, line := range strings.Split(f.Content, "\n") {
				if m := extLineRe.FindStringSubmatch(line); m != nil && m[2] == "" {
					enabled = true
					break
				}
			}
		}
		out = append(out, apitypes.PHPExtension{Name: name, Enabled: enabled, Critical: criticalExtensions[name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// phpToggleEL comments the extension= line out (or back in) in the
// extension's ini; the rest of the file, its settings, stays as it is.
func (s *Server) phpToggleEL(ctx context.Context, layout *osprofile.PHPLayout, name string, enabled bool) error {
	dir, err := s.agent.ListDir(ctx, layout.IniDirs[0])
	if err != nil {
		return huma.Error502BadGateway(err.Error())
	}
	file := ""
	for _, e := range dir.Entries {
		n := strings.TrimSuffix(e.Name, ".ini")
		if _, rest, ok := strings.Cut(n, "-"); ok {
			n = rest
		}
		if n == name && strings.HasSuffix(e.Name, ".ini") {
			file = path.Join(layout.IniDirs[0], e.Name)
		}
	}
	if file == "" {
		return huma.Error422UnprocessableEntity("нет такого расширения: " + name)
	}
	f, err := s.agent.ReadFile(ctx, file, 64*1024)
	if err != nil {
		return huma.Error502BadGateway(err.Error())
	}
	const marker = " ; switched off in MonoPanel"
	lines := strings.Split(f.Content, "\n")
	for i, line := range lines {
		m := extLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		directive := strings.TrimSuffix(strings.TrimRight(m[3], " \t"), marker)
		if enabled {
			lines[i] = m[1] + directive
		} else {
			lines[i] = m[1] + "; " + directive + marker
		}
	}
	_, err = s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: []agent.FileSpec{{Path: file, Content: strings.Join(lines, "\n"), Mode: 0o644}}, Origin: "php:ext"})
	if err != nil {
		return huma.Error502BadGateway(err.Error())
	}
	return nil
}
