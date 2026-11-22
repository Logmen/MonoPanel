package sysinfo

import (
	"errors"
	"slices"
	"testing"
)

func TestCollect(t *testing.T) {
	i := Collect(nil, "/nonexistent-mount")
	if i.Hostname == "" || i.CPUs == 0 || i.MemTotalBytes == 0 || len(i.Disks) != 1 || i.Disks[0].TotalBytes == 0 {
		t.Fatalf("incomplete info: %+v", i)
	}
}

// Whether the process runs in a container: systemd-detect-virt's word when
// it ran ("none" outside one), else systemd's file inside a container, else
// OpenVZ's /proc/vz without the host's /proc/bc.
func TestContainer(t *testing.T) {
	files := func(m map[string]string) func(string) ([]byte, error) {
		return func(p string) ([]byte, error) {
			if c, ok := m[p]; ok {
				return []byte(c), nil
			}
			return nil, errors.New("no such file")
		}
	}
	dirs := func(ds ...string) func(string) bool {
		return func(p string) bool { return slices.Contains(ds, p) }
	}
	ran := func(out string) func() (string, bool) { return func() (string, bool) { return out, true } }
	missing := func() (string, bool) { return "", false }
	for _, c := range []struct {
		name   string
		virt   func() (string, bool)
		read   func(string) ([]byte, error)
		exists func(string) bool
		want   string
	}{
		{"lxc", ran("lxc\n"), files(nil), dirs(), "lxc"},
		{"no container", ran("none\n"), files(map[string]string{"/run/systemd/container": "docker\n"}), dirs("/proc/vz"), ""},
		{"systemd's file", missing, files(map[string]string{"/run/systemd/container": "podman\n"}), dirs(), "podman"},
		{"an OpenVZ container", missing, files(nil), dirs("/proc/vz"), "openvz"},
		{"an OpenVZ host", missing, files(nil), dirs("/proc/vz", "/proc/bc"), ""},
		{"nothing to tell", missing, files(nil), dirs(), ""},
		{"a strange name", ran("lxc\x1b[31m-libvirt extra\n"), files(nil), dirs(), "lxc31m-libvirt"},
	} {
		if got := containerOf(c.virt, c.read, c.exists); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
