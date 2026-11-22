package osprofile

// BlockedModules are kernel modules web hosting does not need and any local
// account can have the kernel load: socket() with their protocol loads the
// module on its own. SCTP had a local root with no kernel fix in November
// 2026; DCCP, RDS and TIPC are older ones of the same kind (CVE-2017-6074,
// CVE-2010-3904, CVE-2021-43267).
var BlockedModules = []string{"sctp", "dccp", "rds", "tipc"}

const (
	// ModprobePackageFile is the rule the package ships. ModprobeLocalFile,
	// a file of the same name in /etc, replaces it — that is how an
	// administrator lets a module load again — and is what mp setup writes
	// on an install without the package.
	ModprobePackageFile = "/usr/lib/modprobe.d/monopanel.conf"
	ModprobeLocalFile   = "/etc/modprobe.d/monopanel.conf"
)

// ModprobeConf is the rule, word for word packaging/modprobe.d/monopanel.conf.
// /bin/false is there on every family: in /bin itself where /usr is not
// merged (coreutils on Debian), through the /bin -> usr/bin link where it
// is (EL, Debian 12 and Ubuntu 22.04 on).
const ModprobeConf = `# MonoPanel: kernel modules web hosting does not need, kept from loading.
#
# Any local account (a site's PHP, an SSH user) makes the kernel load the
# module of a protocol by opening a socket of it, and SCTP had a local root
# with no kernel fix (November 2026); DCCP, RDS and TIPC are older ones of
# the same kind. "install <module> /bin/false" makes every load of the module
# fail, the automatic one and modprobe's alike. A module that is loaded
# already stays until a reboot or rmmod (mp doctor warns about it).
#
# The package ships this file as /usr/lib/modprobe.d/monopanel.conf; an
# install without the package gets it from mp setup as
# /etc/modprobe.d/monopanel.conf. A file of that name in /etc/modprobe.d
# replaces the one in /usr/lib, and neither the package nor mp setup changes
# it: to let a module load again, put a copy there without its line (an empty
# file lets all four load).
install sctp /bin/false
install dccp /bin/false
install rds /bin/false
install tipc /bin/false
`
