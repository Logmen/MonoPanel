package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

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
	// Sphinx 3 for EL: the sphinxsearch.com build against glibc 2.28, which
	// runs on EL9 and EL10 alike. Pinned with its checksum like Roundcube.
	sphinx3Version = "3.9.1"
	sphinx3URL     = "https://sphinxsearch.com/files/sphinx-3.9.1-141d2ea-linux-amd64-glibc2.28.tar.gz"
	settingSphinx  = "sphinx.version"
)

// sphinxDaemon is the server binary of the tarball build.
const sphinxDaemon = "searchd"

// sphinx3SHA256 is the checksum of the pinned tarball; tests pin their own.
var sphinx3SHA256 = "61e4feaa3fb4313fd37eb2dacf5b55c4fc744d09d09716f57c331319a3d9eaf0"

// sphinxDownload fetches the Sphinx 3 tarball; tests replace it.
var sphinxDownload = func(ctx context.Context, url string) ([]byte, error) {
	return fetchBytesN(ctx, url, 96<<20)
}

var toolNames = []string{"memcached", "jpegoptim", "git", "composer", "sphinx"}

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
	out = append(out, c)
	sx := osprofile.Sphinx(s.profile)
	c = apitypes.StackComponent{Name: "sphinx", Kind: "tool", Removable: true}
	if sx.Engine == "sphinx3" {
		if st, err := s.agent.Stat(ctx, sx.InstallDir+"/bin/"+sphinxDaemon); err == nil && len(st.Entries) == 1 && st.Entries[0].Exists {
			c.Installed = true
			v, _ := s.db.GetSetting(ctx, settingSphinx)
			c.Version = "sphinx " + v
		}
	} else if q, err := s.agent.Pkg(ctx, "query", sx.Packages[0]); err == nil {
		if v, ok := q.Installed[sx.Packages[0]]; ok {
			c.Installed, c.Version = true, "sphinx "+v
		}
	}
	if c.Installed {
		if st, err := s.agent.Service(ctx, sx.Service, "status"); err == nil {
			c.Service = &st.Status
		}
	}
	return append(out, c)
}

// installSphinx sets the search server up for 1C-Bitrix: the package (on EL
// after Manticore's repository), the panel's configuration with the bitrix
// real-time index and a local SphinxQL listener, the service.
func (s *Server) installSphinx(ctx context.Context, jc *jobs.Context) error {
	sx := osprofile.Sphinx(s.profile)
	version := ""
	if sx.Engine == "sphinx3" {
		v, err := s.installSphinx3Binaries(ctx, jc, sx)
		if err != nil {
			return err
		}
		version = v
	} else {
		jc.Progress(15, "installing "+sx.Engine)
		res, err := s.agent.Pkg(ctx, "install", sx.Packages...)
		if err != nil {
			return err
		}
		logTail(jc, res.Output, 2)
		version = res.Installed[sx.Packages[0]]
	}
	jc.Progress(60, "configuration")
	model := render.Sphinx{Index: "bitrix", Listen: "127.0.0.1", DataDir: sx.DataDir, LogDir: sx.LogDir, PidFile: sx.PidFile, User: sx.User, InstallDir: sx.InstallDir, ConfFile: sx.ConfFile}
	conf, err := s.render.Render(sx.Template, model)
	if err != nil {
		return err
	}
	files := []agent.FileSpec{{Path: sx.ConfFile, Content: conf, Mode: 0o644}}
	if sx.DefaultsFile != "" {
		// Debian ships the daemon switched off until START=yes.
		files = append(files, agent.FileSpec{Path: sx.DefaultsFile, Content: "# Generated by MonoPanel\nSTART=yes\n", Mode: 0o644})
	}
	if sx.UnitFile != "" {
		unit, err := s.render.Render("sphinx/monopanel-sphinx.service.tmpl", model)
		if err != nil {
			return err
		}
		files = append(files, agent.FileSpec{Path: sx.UnitFile, Content: unit, Mode: 0o644})
		s.agent.Service(ctx, "", "daemon-reload") //nolint:errcheck // a stale unit fails the restart below anyway
	}
	if _, err := s.agent.ApplyConfigSet(ctx, &agent.ApplyConfigSetRequest{Files: files, Restart: []string{sx.Service}, Force: true, Origin: "sphinx"}); err != nil {
		return err
	}
	// Debian's sphinxsearch is a generated SysV unit: systemd refuses to
	// enable it over D-Bus, and the package's rc links already start it.
	if _, err := s.agent.Service(ctx, sx.Service, "enable"); err != nil && !strings.Contains(err.Error(), "generated") {
		return err
	}
	status, err := s.agent.Service(ctx, sx.Service, "status")
	if err != nil {
		return err
	}
	if status.Status.ActiveState != "active" {
		return fmt.Errorf("%s is %s after install (journalctl -u %s)", sx.Engine, status.Status.ActiveState, sx.Service)
	}
	// A SysV unit is "active" even when the daemon died on a bad index, so ask
	// the daemon itself when a MySQL client is around to ask with.
	missing, err := s.sphinxIndexState(ctx, model.Index)
	switch {
	case errors.Is(err, errNoSphinxProbe):
		jc.Logf("no mysql client to probe SphinxQL with; check %s/searchd.log if Bitrix cannot connect", sx.LogDir)
	case err != nil:
		return fmt.Errorf("the search daemon does not serve the %s index on 127.0.0.1:9306 (%v); see %s/searchd.log", model.Index, err, sx.LogDir)
	case len(missing) > 0:
		// searchd keeps the schema an index has on disk ("attribute count
		// mismatch … EXISTING INDEX TAKES PRECEDENCE"), so an index left by an
		// older configuration is recreated; Bitrix fills it again on reindex.
		jc.Logf("the %s index on disk lacks %s; recreating it with the current schema", model.Index, strings.Join(missing, ", "))
		if err := s.recreateSphinxIndex(ctx, sx, model.Index); err != nil {
			return fmt.Errorf("recreate the %s index: %w", model.Index, err)
		}
		if missing, err = s.sphinxIndexState(ctx, model.Index); err != nil {
			return fmt.Errorf("the search daemon does not serve the %s index after recreating it (%v); see %s/searchd.log", model.Index, err, sx.LogDir)
		} else if len(missing) > 0 {
			return fmt.Errorf("the %s index still lacks %s after recreating it; see %s/searchd.log", model.Index, strings.Join(missing, ", "), sx.LogDir)
		}
		jc.Logf("index %s recreated: run the full reindex in Bitrix (Settings → Search)", model.Index)
	default:
		jc.Logf("SphinxQL answers on 127.0.0.1:9306, index %s is served with every column Bitrix expects", model.Index)
	}
	jc.Logf("sphinx %s: SphinxQL on 127.0.0.1:9306, index bitrix; in Bitrix: Settings → Search → Sphinx, connection 127.0.0.1:9306", version)
	jc.Progress(100, "sphinx ready")
	return nil
}

// sphinxColumns are the fields and attributes Bitrix's search module checks
// in the index (search/tools/sphinx.php); the id comes on its own.
var sphinxColumns = []string{"title", "body", "module_id", "module", "item_id", "item", "param1_id", "param1", "param2_id", "param2",
	"date_change", "date_to", "date_from", "custom_rank", "tags", "right", "site", "param"}

var errNoSphinxProbe = errors.New("no mysql client to probe SphinxQL with")

// sphinxQL runs one statement against the local SphinxQL listener through
// the MySQL client; without a client there is nothing to ask with.
func (s *Server) sphinxQL(ctx context.Context, stmt string) (*agent.ToolResponse, error) {
	res, err := s.agent.Tool(ctx, &agent.ToolRequest{Name: "mysql", Args: []string{"--protocol=tcp", "-h127.0.0.1", "-P9306", "--batch", "--skip-column-names", "-e", stmt}, TimeoutSeconds: 30})
	if err != nil {
		return nil, errNoSphinxProbe
	}
	return res, nil
}

// sphinxIndexState waits for searchd to answer (systemd reports the unit
// started before the daemon listens), checks that the index is served and
// reports which of Bitrix's columns it lacks.
func (s *Server) sphinxIndexState(ctx context.Context, index string) ([]string, error) {
	var res *agent.ToolResponse
	for attempt := 0; ; attempt++ {
		var err error
		if res, err = s.sphinxQL(ctx, "SHOW TABLES"); err != nil {
			return nil, err
		}
		if res.ExitCode == 0 || attempt >= 15 || !strings.Contains(res.Output, "Can't connect") {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if res.ExitCode != 0 {
		return nil, errors.New(strings.TrimSpace(res.Output))
	}
	served := false
	for _, line := range strings.Split(res.Output, "\n") {
		if name, _, _ := strings.Cut(line, "\t"); strings.TrimSpace(name) == index {
			served = true
		}
	}
	if !served {
		return nil, fmt.Errorf("index %s is not among the served ones: %s", index, strings.TrimSpace(res.Output))
	}
	desc, err := s.sphinxQL(ctx, "DESCRIBE "+index)
	if err != nil {
		return nil, err
	}
	if desc.ExitCode != 0 {
		return nil, errors.New(strings.TrimSpace(desc.Output))
	}
	have := map[string]bool{}
	for _, line := range strings.Split(desc.Output, "\n") {
		name, _, _ := strings.Cut(line, "\t")
		have[strings.TrimSpace(name)] = true
	}
	var missing []string
	for _, c := range sphinxColumns {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	return missing, nil
}

// recreateSphinxIndex drops the index files while searchd is stopped, so it
// starts again with the schema the configuration declares.
func (s *Server) recreateSphinxIndex(ctx context.Context, sx osprofile.SphinxLayout, index string) error {
	if _, err := s.agent.Service(ctx, sx.Service, "stop"); err != nil {
		return err
	}
	if _, err := s.removeSphinxIndexFiles(ctx, sx, index); err != nil {
		return err
	}
	_, err := s.agent.Service(ctx, sx.Service, "start")
	return err
}

// removeSphinxIndexFiles removes what searchd wrote for the index under the
// data directory: <index>.* and the binlog that would replay into it.
func (s *Server) removeSphinxIndexFiles(ctx context.Context, sx osprofile.SphinxLayout, index string) ([]string, error) {
	dir, err := s.agent.ListDir(ctx, sx.DataDir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range dir.Entries {
		if strings.HasPrefix(e.Name, index+".") || strings.HasPrefix(e.Name, "binlog.") {
			paths = append(paths, path.Join(sx.DataDir, e.Name))
		}
	}
	if len(paths) == 0 {
		return nil, nil
	}
	if _, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: paths}); err != nil {
		return nil, err
	}
	return paths, nil
}

// installSphinx3Binaries puts the sphinxsearch.com build under /opt/monopanel
// (checksum verified), with its own system user and directories; running it
// again refreshes the binaries.
func (s *Server) installSphinx3Binaries(ctx context.Context, jc *jobs.Context, sx osprofile.SphinxLayout) (string, error) {
	jc.Progress(5, "sphinx user and directories")
	if _, err := s.agent.EnsureUnixUser(ctx, &agent.EnsureUnixUserRequest{Login: sx.User, System: true, Home: sx.DataDir, Shell: s.profile.NologinShell()}); err != nil {
		return "", fmt.Errorf("user %s: %w", sx.User, err)
	}
	if _, err := s.agent.EnsureDirs(ctx, &agent.EnsureDirsRequest{Dirs: []agent.DirSpec{
		{Path: "/opt/monopanel", Mode: 0o755, Owner: "root", Group: "root"},
		{Path: sx.InstallDir, Mode: 0o755, Owner: "root", Group: "root"},
		{Path: sx.DataDir, Mode: 0o750, Owner: sx.User, Group: sx.User},
		{Path: sx.LogDir, Mode: 0o750, Owner: sx.User, Group: sx.User},
	}}); err != nil {
		return "", err
	}
	jc.Progress(15, "downloading Sphinx "+sphinx3Version)
	tarball, err := sphinxDownload(ctx, sphinx3URL)
	if err != nil {
		return "", fmt.Errorf("download sphinx: %w", err)
	}
	sum := sha256.Sum256(tarball)
	if got := hex.EncodeToString(sum[:]); got != sphinx3SHA256 {
		return "", fmt.Errorf("sphinx tarball checksum mismatch: got %s, expected %s", got, sphinx3SHA256)
	}
	jc.Progress(40, "unpacking into "+sx.InstallDir)
	res, err := s.agent.StreamIn(ctx, &agent.StreamRequest{Name: "tar", Args: []string{"-xzf", "-", "-C", sx.InstallDir, "--strip-components=1", "--no-same-owner"}, TimeoutSeconds: 600}, bytes.NewReader(tarball))
	if err != nil {
		return "", fmt.Errorf("unpack sphinx: %w", err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("unpack sphinx: tar exit %d: %s", res.ExitCode, strings.TrimSpace(res.Output))
	}
	if err := s.db.SetSetting(ctx, settingSphinx, sphinx3Version); err != nil {
		return "", err
	}
	jc.Logf("Sphinx %s (sphinxsearch.com build) in %s", sphinx3Version, sx.InstallDir)
	return sphinx3Version, nil
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
	case "sphinx":
		sx := osprofile.Sphinx(s.profile)
		jc.Progress(20, "stopping sphinx")
		s.agent.Service(ctx, sx.Service, "stop")    //nolint:errcheck // gone with the package anyway
		s.agent.Service(ctx, sx.Service, "disable") //nolint:errcheck // gone with the package anyway
		if sx.Engine == "sphinx3" {
			jc.Progress(50, "removing the binaries and the unit")
			if _, err := s.agent.RemovePaths(ctx, &agent.RemovePathsRequest{Paths: []string{sx.UnitFile, sx.InstallDir}, Recursive: true}); err != nil {
				return err
			}
			s.agent.Service(ctx, "", "daemon-reload") //nolint:errcheck // best effort
			s.db.SetSetting(ctx, settingSphinx, "")   //nolint:errcheck // the files are gone, that is what matters
		} else {
			jc.Progress(50, "removing the package")
			if _, err := s.agent.Pkg(ctx, "remove", sx.Packages...); err != nil {
				return err
			}
		}
		// The index is derived data: Bitrix rebuilds it after a reinstall, and
		// a stale one would otherwise dictate the schema of the next install.
		if removed, err := s.removeSphinxIndexFiles(ctx, sx, "bitrix"); err != nil {
			jc.Logf("the index files in %s are kept: %v", sx.DataDir, err)
		} else if len(removed) > 0 {
			jc.Logf("index files removed from %s", sx.DataDir)
		}
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
