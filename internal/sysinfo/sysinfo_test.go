package sysinfo

import "testing"

func TestCollect(t *testing.T) {
	i := Collect(nil, "/nonexistent-mount")
	if i.Hostname == "" || i.CPUs == 0 || i.MemTotalBytes == 0 || len(i.Disks) != 1 || i.Disks[0].TotalBytes == 0 {
		t.Fatalf("incomplete info: %+v", i)
	}
}
