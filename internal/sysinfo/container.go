package sysinfo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"
)

// Container names the container this process runs in — lxc, openvz,
// docker, podman, systemd-nspawn — and is empty on a host or in a virtual
// machine. In a container the kernel is the host's: it loads modules with
// the host's modprobe configuration, and what the container's own
// /etc/modprobe.d says changes nothing (the doctor's kernel modules check).
func Container() string {
	return containerOf(detectVirt, os.ReadFile, func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	})
}

// detectVirt is systemd-detect-virt --container, and whether it ran: it
// exits 1 with "none" where there is no container.
func detectVirt() (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/systemd-detect-virt", "--container").Output()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return "", false
	}
	return string(out), true
}

// containerOf asks systemd-detect-virt first; without it, systemd's own
// word inside a container (/run/systemd/container, which PID 1 writes when
// a container manager started it), then OpenVZ's /proc/vz, which its host
// has as well, but with /proc/bc beside it.
func containerOf(virt func() (string, bool), read func(string) ([]byte, error), exists func(string) bool) string {
	if out, ran := virt(); ran {
		return containerName(out)
	}
	if b, err := read("/run/systemd/container"); err == nil {
		if name := containerName(string(b)); name != "" {
			return name
		}
	}
	if exists("/proc/vz") && !exists("/proc/bc") {
		return "openvz"
	}
	return ""
}

// containerName is the first word of what names a container, shown by the
// doctor: a short name of letters, digits and dashes, "" for "none".
func containerName(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 || f[0] == "none" {
		return ""
	}
	name := strings.Map(func(r rune) rune {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.') {
			return r
		}
		return -1
	}, f[0])
	if len(name) > 32 {
		name = name[:32]
	}
	return name
}
