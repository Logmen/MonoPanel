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

// SphinxLayout is the full-text search server for 1C-Bitrix: Sphinx 2.2 from
// the distribution on Debian/Ubuntu (the version Bitrix documents), Manticore
// Search from its own repository on EL, which has no Sphinx package. Both
// speak SphinxQL on the same port, and Bitrix talks to either the same way.
type SphinxLayout struct {
	Engine       string   `json:"engine"` // sphinx | manticore
	RepoPackage  string   `json:"repo_package,omitempty"`
	Packages     []string `json:"packages"`
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
		return SphinxLayout{Engine: "manticore", RepoPackage: "https://repo.manticoresearch.com/manticore-repo.noarch.rpm", Packages: []string{"manticore"},
			Service: "manticore.service", ConfFile: "/etc/manticoresearch/manticore.conf", Template: "sphinx/manticore.conf.tmpl",
			User: "manticore", DataDir: "/var/lib/manticore", LogDir: "/var/log/manticore", PidFile: "/var/run/manticore/searchd.pid"}
	}
	return SphinxLayout{Engine: "sphinx", Packages: []string{"sphinxsearch"}, Service: "sphinxsearch.service", ConfFile: "/etc/sphinxsearch/sphinx.conf", Template: "sphinx/sphinx.conf.tmpl",
		DefaultsFile: "/etc/default/sphinxsearch", User: "sphinxsearch", DataDir: "/var/lib/sphinxsearch/data", LogDir: "/var/log/sphinxsearch", PidFile: "/run/sphinxsearch/searchd.pid"}
}
