package osprofile

import "testing"

// apt-get update gives up at once on the lock of the lists another one
// holds, which the agent waits out; dpkg's lock it waits for itself
// (DPkg::Lock::Timeout), and dnf waits for its own.
func TestIndexLocked(t *testing.T) {
	const lists = "E: Could not get lock /var/lib/apt/lists/lock. It is held by process 9093 (python3)\nE: Unable to lock directory /var/lib/apt/lists/\n"
	if !(aptManager{}).IndexLocked(lists) {
		t.Error("apt: the lists' lock is not told")
	}
	for _, out := range []string{
		"E: Could not get lock /var/lib/dpkg/lock-frontend. It is held by process 7 (apt-get)\n",
		"W: Failed to fetch https://packages.sury.org/php/dists/noble/InRelease  Temporary failure resolving 'packages.sury.org'\n",
		"",
	} {
		if (aptManager{}).IndexLocked(out) {
			t.Errorf("apt: %q taken for the lists' lock", out)
		}
	}
	if (dnfManager{}).IndexLocked(lists) || (noPackages{}).IndexLocked(lists) {
		t.Error("dnf or none waits for the lists' lock of apt's")
	}
}
