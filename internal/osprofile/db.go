package osprofile

// DBLayout describes where a MySQL-compatible server lives on this OS.
type DBLayout struct {
	Engine   string `json:"engine"`
	Service  string `json:"service"`
	Socket   string `json:"socket"`
	ConfFile string `json:"conf_file"`
	SlowLog  string `json:"slow_log"`
	// ErrorLog is where the server writes its own log; on EL the package
	// leaves the temporary root password there.
	ErrorLog string `json:"error_log"`
	// IncludeFile, when set, is a file the server reads by default that has
	// to pull ConfFile's directory in: Percona's my.cnf on EL has no
	// !includedir, so /etc/my.cnf.d would otherwise be ignored.
	IncludeFile  string   `json:"include_file,omitempty"`
	Packages     []string `json:"packages"`
	ClientBinary string   `json:"client_binary"`
}

// DB returns the layout for an engine ("mysql" or "percona").
func DB(p Profile, engine string) DBLayout {
	l := DBLayout{Engine: engine, ClientBinary: "/usr/bin/mysql"}
	switch p.Family() {
	case FamilyRHEL:
		// The slow log lives in its own directory: mysqld may not create
		// files directly under /var/log, which root owns on EL.
		l.Service, l.Socket, l.ConfFile, l.SlowLog = "mysqld.service", "/var/lib/mysql/mysql.sock", "/etc/my.cnf.d/zz-monopanel.cnf", "/var/log/mysql/monopanel-slow.log"
		l.ErrorLog = "/var/log/mysqld.log"
		if engine == "percona" {
			l.Packages = []string{"percona-server-server"}
			// mysqld reads /etc/my.cnf, /etc/mysql/my.cnf, /usr/etc/my.cnf in
			// that order; the second does not exist on EL and belongs to no package.
			l.IncludeFile = "/etc/mysql/my.cnf"
		} else {
			l.Packages = []string{"mysql-community-server"}
		}
	default:
		l.Service, l.Socket, l.SlowLog = "mysql.service", "/var/run/mysqld/mysqld.sock", "/var/log/mysql/monopanel-slow.log"
		l.ErrorLog = "/var/log/mysql/error.log"
		if engine == "percona" {
			l.ConfFile = "/etc/mysql/conf.d/zz-monopanel.cnf"
			l.Packages = []string{"percona-server-server"}
		} else {
			l.ConfFile = "/etc/mysql/mysql.conf.d/zz-monopanel.cnf"
			l.Packages = []string{"mysql-community-server"}
		}
	}
	return l
}
