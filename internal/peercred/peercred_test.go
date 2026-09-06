package peercred

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestPeerCred(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	wrapped := &Listener{Listener: ln, Allow: func(c *Cred) bool { return int(c.UID) == os.Getuid() }}
	done := make(chan *Cred, 1)
	go func() {
		c, err := wrapped.Accept()
		if err != nil {
			done <- nil
			return
		}
		defer c.Close()
		done <- c.(*Conn).Cred
	}()
	cl, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	cred := <-done
	if cred == nil || int(cred.UID) != os.Getuid() || int(cred.PID) != os.Getpid() {
		t.Fatalf("cred %+v", cred)
	}
}
