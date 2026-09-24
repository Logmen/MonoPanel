package osprofile

// MemcachedLayout is where memcached keeps its settings on this OS.
type MemcachedLayout struct {
	Package  string `json:"package"`
	Service  string `json:"service"`
	ConfFile string `json:"conf_file"`
	// Template is the panel's template for ConfFile.
	Template string `json:"template"`
	User     string `json:"user"`
}

// Memcached returns the layout for a profile.
func Memcached(p Profile) MemcachedLayout {
	if p.Family() == FamilyRHEL {
		return MemcachedLayout{Package: "memcached", Service: "memcached.service", ConfFile: "/etc/sysconfig/memcached", Template: "memcached/sysconfig.tmpl", User: "memcached"}
	}
	return MemcachedLayout{Package: "memcached", Service: "memcached.service", ConfFile: "/etc/memcached.conf", Template: "memcached/memcached.conf.tmpl", User: "memcache"}
}

// ValkeyLayout is the key-value server behind the per-account cache and
// session instances: Valkey where the distribution ships it, Redis — the same
// protocol and configuration — where it does not (Ubuntu 22.04, Debian 12
// without backports). Service is the distribution's own shared instance, which
// the panel keeps off: it listens without a password for every account.
type ValkeyLayout struct {
	Engine  string `json:"engine"` // valkey | redis
	Package string `json:"package"`
	Binary  string `json:"binary"`
	Service string `json:"service"`
}

// ValkeyCandidates lists the choices for a profile, the preferred one first.
func ValkeyCandidates(p Profile) []ValkeyLayout {
	if p.Family() == FamilyRHEL {
		return []ValkeyLayout{
			{Engine: "valkey", Package: "valkey", Binary: "/usr/bin/valkey-server", Service: "valkey.service"},
			{Engine: "redis", Package: "redis", Binary: "/usr/bin/redis-server", Service: "redis.service"},
		}
	}
	return []ValkeyLayout{
		{Engine: "valkey", Package: "valkey-server", Binary: "/usr/bin/valkey-server", Service: "valkey-server.service"},
		{Engine: "redis", Package: "redis-server", Binary: "/usr/bin/redis-server", Service: "redis-server.service"},
	}
}

// ToolPackages names the packages of a small tool; EL pulls jpegoptim from EPEL.
func ToolPackages(p Profile, tool string) []string {
	switch tool {
	case "jpegoptim":
		if epel := p.EPELPackage(); epel != "" {
			return []string{epel, "jpegoptim"}
		}
		return []string{"jpegoptim"}
	case "git":
		return []string{"git"}
	}
	return nil
}

// SphinxLayout is the full-text search server for 1C-Bitrix. Debian and
// Ubuntu package Sphinx 2.2 (the version Bitrix documented for years); EL has
// no package, and Manticore's SHOW TABLES no longer answers the way Bitrix
// checks the index, so there the panel installs the sphinxsearch.com build
// of Sphinx 3 — the version Bitrix's own reference configuration targets.
type SphinxLayout struct {
	Engine       string   `json:"engine"` // sphinx (packaged 2.2) | sphinx3 (tarball)
	Packages     []string `json:"packages,omitempty"`
	InstallDir   string   `json:"install_dir,omitempty"` // tarball build
	UnitFile     string   `json:"unit_file,omitempty"`   // tarball build: the panel's unit
	Service      string   `json:"service"`
	ConfFile     string   `json:"conf_file"`
	Template     string   `json:"template"`
	DefaultsFile string   `json:"defaults_file,omitempty"` // Debian: START=yes lives here
	User         string   `json:"user"`
	DataDir      string   `json:"data_dir"`
	LogDir       string   `json:"log_dir"`
	PidFile      string   `json:"pid_file"`
}

// Sphinx returns the search server layout for a profile.
func Sphinx(p Profile) SphinxLayout {
	if p.Family() == FamilyRHEL {
		return SphinxLayout{Engine: "sphinx3", InstallDir: "/opt/monopanel/sphinx", UnitFile: "/etc/systemd/system/monopanel-sphinx.service",
			Service: "monopanel-sphinx.service", ConfFile: "/etc/sphinx/sphinx.conf", Template: "sphinx/sphinx3.conf.tmpl",
			User: "sphinx", DataDir: "/var/lib/sphinx", LogDir: "/var/log/sphinx", PidFile: "/run/sphinx/searchd.pid"}
	}
	return SphinxLayout{Engine: "sphinx", Packages: []string{"sphinxsearch"}, Service: "sphinxsearch.service", ConfFile: "/etc/sphinxsearch/sphinx.conf", Template: "sphinx/sphinx.conf.tmpl",
		DefaultsFile: "/etc/default/sphinxsearch", User: "sphinxsearch", DataDir: "/var/lib/sphinxsearch/data", LogDir: "/var/log/sphinxsearch", PidFile: "/run/sphinxsearch/searchd.pid"}
}
