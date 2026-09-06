package api

import (
	"context"
	"net/http"
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
// in mods-available can be enabled; phpquery reports what the FPM SAPI loads.
func (s *Server) phpModules(ctx context.Context, version string) ([]apitypes.PHPExtension, error) {
	if s.profile.Family() != osprofile.FamilyDebian {
		return nil, huma.Error501NotImplemented("пока поддержано только на Debian/Ubuntu (phpenmod)")
	}
	dir, err := s.agent.ListDir(ctx, "/etc/php/"+version+"/mods-available")
	if err != nil {
		return nil, huma.Error502BadGateway(err.Error())
	}
	enabled := map[string]bool{}
	if res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "phpquery", Args: []string{"-v", version, "-s", "fpm", "-M"}, TimeoutSeconds: 30}); err == nil && res.ExitCode == 0 {
		for _, m := range strings.Fields(res.Output) {
			enabled[m] = true
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
