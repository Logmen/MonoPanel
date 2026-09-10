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
