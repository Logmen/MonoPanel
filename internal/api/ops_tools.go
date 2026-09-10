package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/jobs"
	"monopanel/internal/osprofile"
	"monopanel/internal/render"
	"monopanel/internal/store"
)

// Small server-level tools next to the web stack: memcached, jpegoptim, git
// and composer. They install and uninstall from the panel; memcached has
// settings, composer needs a PHP branch and is fetched from getcomposer.org.

const (
	settingMemcachedMemory = "memcached.memory_mb"
	settingMemcachedConns  = "memcached.max_conn"
	settingComposerVersion = "composer.version"
	composerVersionsURL    = "https://getcomposer.org/versions"
	composerPhar           = "/usr/local/lib/monopanel/composer.phar"
	composerBin            = "/usr/local/bin/composer"
	memcachedDefaultMB     = 128
	memcachedDefaultConns  = 1024
)

var toolNames = []string{"memcached", "jpegoptim", "git", "composer"}

func isTool(name string) bool {
	for _, t := range toolNames {
		if t == name {
			return true
		}
	}
	return false
}

type memcachedOutput struct {
	Body apitypes.MemcachedSettings
}

type memcachedInput struct {
	Body apitypes.MemcachedUpdate
}

type stackComponentInput struct {
	Component string `path:"component"`
}

// composerLatest asks getcomposer.org for the current stable release; tests
// replace it.
var composerLatest = func(ctx context.Context) (version, url, sha string, err error) {
	raw, err := fetchBytes(ctx, composerVersionsURL)
	if err != nil {
		return "", "", "", err
	}
	var versions struct {
		Stable []struct {
			Path    string `json:"path"`
			Version string `json:"version"`
			SHA256  string `json:"sha256"`
		} `json:"stable"`
	}
	if err := json.Unmarshal(raw, &versions); err != nil || len(versions.Stable) == 0 {
		return "", "", "", errors.New("getcomposer.org: unexpected versions list")
	}
	v := versions.Stable[0]
	return v.Version, "https://getcomposer.org" + v.Path, v.SHA256, nil
}

// composerDownload fetches the phar; tests replace it.
var composerDownload = func(ctx context.Context, url string) ([]byte, error) {
	return fetchBytesN(ctx, url, 32<<20)
}

// toolComponents describes the tools for the stack listing.
func (s *Server) toolComponents(ctx context.Context) []apitypes.StackComponent {
	out := []apitypes.StackComponent{}
	mc := osprofile.Memcached(s.profile)
	pkgs := []string{mc.Package, "jpegoptim", "git"}
	installed := map[string]string{}
	if q, err := s.agent.Pkg(ctx, "query", pkgs...); err == nil {
		installed = q.Installed
	}
	c := apitypes.StackComponent{Name: "memcached", Kind: "tool", Removable: true}
	if v, ok := installed[mc.Package]; ok {
		c.Installed, c.Version = true, v
		if st, err := s.agent.Service(ctx, mc.Service, "status"); err == nil {
			c.Service = &st.Status
		}
	}
	out = append(out, c)
	for _, name := range []string{"jpegoptim", "git"} {
		c := apitypes.StackComponent{Name: name, Kind: "tool", Removable: true}
		if v, ok := installed[name]; ok {
			c.Installed, c.Version = true, v
		}
		out = append(out, c)
	}
	c = apitypes.StackComponent{Name: "composer", Kind: "tool", Removable: true}
	if st, err := s.agent.Stat(ctx, composerBin); err == nil && len(st.Entries) == 1 && st.Entries[0].Exists {
		c.Installed = true
		c.Version, _ = s.db.GetSetting(ctx, settingComposerVersion)
	}
	return append(out, c)
}

func (s *Server) memcachedSettings(ctx context.Context) apitypes.MemcachedSettings {
	st := apitypes.MemcachedSettings{MemoryMB: memcachedDefaultMB, MaxConnections: memcachedDefaultConns}
	if v, _ := s.db.GetSetting(ctx, settingMemcachedMemory); v != "" {
		st.MemoryMB, _ = strconv.Atoi(v)
	}
	if v, _ := s.db.GetSetting(ctx, settingMemcachedConns); v != "" {
		st.MaxConnections, _ = strconv.Atoi(v)
	}
	mc := osprofile.Memcached(s.profile)
	if q, err := s.agent.Pkg(ctx, "query", mc.Package); err == nil {
		_, st.Installed = q.Installed[mc.Package]
	}
	return st
}

// writeMemcachedConfig renders the settings into the OS's file and restarts
// the service.
func (s *Server) writeMemcachedConfig(ctx context.Context, st apitypes.MemcachedSettings) error {
	mc := osprofile.Memcached(s.profile)
	conf, err := s.render.Render(mc.Template, render.Memcached{MemoryMB: st.MemoryMB, MaxConn: st.MaxConnections, Listen: "127.0.0.1", User: mc.User})
	if err != nil {
		return err
	}
	_, err = s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: []agent.FileSpec{{Path: mc.ConfFile, Content: conf, Mode: 0o644}}, Restart: []string{mc.Service}, Force: true, Origin: "memcached"})
	return err
}

func (s *Server) installMemcached(ctx context.Context, jc *jobs.Context) error {
	mc := osprofile.Memcached(s.profile)
	jc.Progress(10, "installing memcached")
	res, err := s.agent.Pkg(ctx, "install", mc.Package)
	if err != nil {
		return err
	}
	logTail(jc, res.Output, 2)
	jc.Progress(60, "configuration")
	st := s.memcachedSettings(ctx)
	if err := s.writeMemcachedConfig(ctx, st); err != nil {
		return err
	}
	if _, err := s.agent.Service(ctx, mc.Service, "enable"); err != nil {
		return err
	}
	status, err := s.agent.Service(ctx, mc.Service, "status")
	if err != nil {
		return err
	}
	if status.Status.ActiveState != "active" {
		return fmt.Errorf("memcached is %s after install", status.Status.ActiveState)
	}
	jc.Logf("memcached %s: 127.0.0.1:11211, %d MB, %d connections; PHP sites need the memcached extension of their branch (mp php ext enable)", res.Installed[mc.Package], st.MemoryMB, st.MaxConnections)
	jc.Progress(100, "memcached ready")
	return nil
}

func (s *Server) installToolPackages(ctx context.Context, jc *jobs.Context, tool string) error {
	pkgs := osprofile.ToolPackages(s.profile, tool)
	jc.Progress(10, "installing "+tool)
	res, err := s.agent.Pkg(ctx, "install", pkgs...)
	if err != nil {
		return err
	}
	logTail(jc, res.Output, 2)
	jc.Logf("%s %s", tool, res.Installed[tool])
	jc.Progress(100, tool+" ready")
	return nil
}

// installComposer puts the current stable composer.phar under /usr/local/lib
// and a wrapper on the PATH that runs it with the newest PHP branch the
// panel installed; a second run updates both.
func (s *Server) installComposer(ctx context.Context, jc *jobs.Context) error {
	versions, err := s.db.ListPHPVersions(ctx)
	if err != nil {
		return err
	}
	php := ""
	newest := ""
	for _, v := range versions {
		if v.Status != store.PHPInstalled {
			continue
		}
		if newest == "" || versionLess(newest, v.Version) {
			newest = v.Version
		}
	}
	if newest == "" {
		return errors.New("composer runs on PHP: install a PHP branch first (mp php install 8.4)")
	}
	if l := osprofile.PHP(s.profile, newest); l != nil {
		php = l.CLIBinary
	}
	jc.Progress(10, "getcomposer.org")
	version, url, sha, err := composerLatest(ctx)
	if err != nil {
		return fmt.Errorf("getcomposer.org: %w", err)
	}
	jc.Logf("composer %s from %s", version, url)
	jc.Progress(30, "downloading composer.phar")
	phar, err := composerDownload(ctx, url)
	if err != nil {
		return fmt.Errorf("download composer.phar: %w", err)
	}
	sum := sha256.Sum256(phar)
	if got := hex.EncodeToString(sum[:]); sha != "" && got != sha {
		return fmt.Errorf("composer.phar checksum mismatch: got %s, getcomposer.org says %s", got, sha)
	}
	jc.Progress(70, "installing")
	wrapper := "#!/bin/sh\n# Generated by MonoPanel: composer on the newest PHP branch the panel installed.\nexec " + php + " " + composerPhar + " \"$@\"\n"
	if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: []agent.FileSpec{
		{Path: composerPhar, ContentBase64: base64.StdEncoding.EncodeToString(phar), Mode: 0o755},
		{Path: composerBin, Content: wrapper, Mode: 0o755},
	}, Origin: "composer"}); err != nil {
		return err
	}
	if err := s.db.SetSetting(ctx, settingComposerVersion, version); err != nil {
		return err
	}
	jc.Logf("composer %s at %s, runs with PHP %s (%s)", version, composerBin, newest, php)
	jc.Progress(100, "composer ready")
	return nil
}

// jobStackRemove uninstalls a tool.
func (s *Server) jobStackRemove(ctx context.Context, jc *jobs.Context) error {
	var p apitypes.StackInstallRequest
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	switch p.Component {
	case "memcached":
		mc := osprofile.Memcached(s.profile)
		jc.Progress(20, "stopping memcached")
		s.agent.Service(ctx, mc.Service, "stop")    //nolint:errcheck // gone with the package anyway
		s.agent.Service(ctx, mc.Service, "disable") //nolint:errcheck // gone with the package anyway
		jc.Progress(50, "removing the package")
		if _, err := s.agent.Pkg(ctx, "remove", mc.Package); err != nil {
			return err
		}
	case "jpegoptim", "git":
		jc.Progress(30, "removing "+p.Component)
		if _, err := s.agent.Pkg(ctx, "remove", p.Component); err != nil {
			return err
		}
	case "composer":
		jc.Progress(30, "removing composer")
		if _, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{composerBin, composerPhar}}); err != nil {
			return err
		}
		s.db.SetSetting(ctx, settingComposerVersion, "") //nolint:errcheck // the files are gone, that is what matters
	default:
		return fmt.Errorf("%s cannot be removed from the panel", p.Component)
	}
	jc.Logf("%s removed", p.Component)
	jc.Progress(100, "removed")
	return nil
}

func (s *Server) registerTools() {
	huma.Register(s.api, huma.Operation{
		OperationID: "stack-remove", Method: http.MethodDelete, Path: "/stack/{component}", Summary: "Uninstall a tool (memcached, jpegoptim, git, composer) (async)", Tags: []string{"stack"},
		Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *stackComponentInput) (*jobRefOutput, error) {
		p := principalFrom(ctx)
		if !isTool(in.Component) {
			return nil, huma.Error422UnprocessableEntity("only the tools can be removed from the panel: " + strings.Join(toolNames, ", "))
		}
		job, err := s.jobs.Enqueue(ctx, "stack.remove", apitypes.StackInstallRequest{Component: in.Component}, jobs.WithLockKey("stack"), jobs.WithRequestedBy(p.Login))
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "stack.remove", Target: in.Component, IP: requestInfo(ctx).IP})
		return &jobRefOutput{Status: http.StatusAccepted, Body: apitypes.JobRef{JobID: job.ID}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "stack-memcached", Method: http.MethodGet, Path: "/stack/memcached", Summary: "memcached settings", Tags: []string{"stack"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, _ *struct{}) (*memcachedOutput, error) {
		return &memcachedOutput{Body: s.memcachedSettings(ctx)}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "stack-memcached-update", Method: http.MethodPut, Path: "/stack/memcached", Summary: "Change memcached settings; applied at once when it is installed", Tags: []string{"stack"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *memcachedInput) (*memcachedOutput, error) {
		p := principalFrom(ctx)
		if err := s.db.SetSetting(ctx, settingMemcachedMemory, strconv.Itoa(in.Body.MemoryMB)); err != nil {
			return nil, err
		}
		if err := s.db.SetSetting(ctx, settingMemcachedConns, strconv.Itoa(in.Body.MaxConnections)); err != nil {
			return nil, err
		}
		st := s.memcachedSettings(ctx)
		if st.Installed {
			if err := s.writeMemcachedConfig(ctx, st); err != nil {
				return nil, err
			}
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "memcached.settings", IP: requestInfo(ctx).IP, Details: map[string]any{"memory_mb": st.MemoryMB, "max_connections": st.MaxConnections}})
		return &memcachedOutput{Body: st}, nil
	})
}
