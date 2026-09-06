// Package sysinfo collects host metrics from /proc and statfs.
package sysinfo

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"monopanel/internal/osprofile"
)

// Info is a snapshot of the host.
type Info struct {
	Hostname          string            `json:"hostname"`
	Family            string            `json:"family,omitempty"`
	Release           osprofile.Release `json:"release"`
	Kernel            string            `json:"kernel"`
	UptimeSeconds     float64           `json:"uptime_seconds"`
	Load              [3]float64        `json:"load"`
	CPUs              int               `json:"cpus"`
	MemTotalBytes     uint64            `json:"mem_total_bytes"`
	MemAvailableBytes uint64            `json:"mem_available_bytes"`
	SwapTotalBytes    uint64            `json:"swap_total_bytes"`
	SwapFreeBytes     uint64            `json:"swap_free_bytes"`
	Disks             []Disk            `json:"disks"`
	Time              time.Time         `json:"time"`
}

// Disk is filesystem usage for one mount.
type Disk struct {
	Mount      string `json:"mount"`
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
}

// Collect gathers the snapshot. extraMounts are reported when they exist and
// live on a different filesystem than "/".
func Collect(p osprofile.Profile, extraMounts ...string) *Info {
	info := &Info{Time: time.Now(), CPUs: runtime.NumCPU()}
	info.Hostname, _ = os.Hostname()
	if p != nil {
		info.Release = p.Release()
		info.Family = string(p.Family())
	}
	var uts unix.Utsname
	if unix.Uname(&uts) == nil {
		info.Kernel = unix.ByteSliceToString(uts.Release[:])
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		fmt.Sscanf(string(b), "%f", &info.UptimeSeconds)
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		fmt.Sscanf(string(b), "%f %f %f", &info.Load[0], &info.Load[1], &info.Load[2])
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, ok := strings.Cut(sc.Text(), ":")
			if !ok {
				continue
			}
			fields := strings.Fields(v)
			if len(fields) == 0 {
				continue
			}
			n, _ := strconv.ParseUint(fields[0], 10, 64)
			n *= 1024
			switch k {
			case "MemTotal":
				info.MemTotalBytes = n
			case "MemAvailable":
				info.MemAvailableBytes = n
			case "SwapTotal":
				info.SwapTotalBytes = n
			case "SwapFree":
				info.SwapFreeBytes = n
			}
		}
		f.Close()
	}
	seen := map[uint64]bool{}
	for _, m := range append([]string{"/"}, extraMounts...) {
		var st unix.Statfs_t
		if err := unix.Statfs(m, &st); err != nil {
			continue
		}
		id := uint64(st.Fsid.Val[0])<<32 | uint64(uint32(st.Fsid.Val[1]))
		if seen[id] {
			continue
		}
		seen[id] = true
		info.Disks = append(info.Disks, Disk{Mount: m, TotalBytes: st.Blocks * uint64(st.Bsize), FreeBytes: st.Bavail * uint64(st.Bsize)})
	}
	return info
}
