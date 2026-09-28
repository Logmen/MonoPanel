package api

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// webWorkerRe names the processes a compromised site runs as: PHP under
// php-fpm or php-cgi, Apache and nginx workers. A request that reaches the
// local socket from one of them — directly, or through exec() a few processes
// down — is a site talking to the panel with its owner's rights, not a person.
var webWorkerRe = regexp.MustCompile(`^(php-fpm|php-cgi|php[0-9.]*-fpm|apache2|httpd|nginx)`)

// procRoot is /proc; tests point it at a fake tree.
var procRoot = "/proc"

// fromWebWorker reports whether pid or one of its ancestors is a web worker.
// The walk stops at init, after 64 steps, or at a process that has already
// gone (its status is unreadable): an unknown ancestry is not a reason to
// refuse, the group check on the socket has already happened.
func fromWebWorker(pid int) bool {
	for step := 0; step < 64 && pid > 1; step++ {
		name, ppid, ok := procStatus(pid)
		if !ok {
			return false
		}
		if webWorkerRe.MatchString(name) {
			return true
		}
		pid = ppid
	}
	return false
}

// procStatus reads the name and parent of a process from /proc/<pid>/status,
// which is readable whoever owns the process.
func procStatus(pid int) (name string, ppid int, ok bool) {
	f, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return "", 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, value, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "Name":
			name = value
		case "PPid":
			ppid, _ = strconv.Atoi(value)
		}
	}
	return name, ppid, name != ""
}
