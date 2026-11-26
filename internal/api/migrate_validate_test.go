package api

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/user"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"monopanel/internal/auth"
)

// No test of the package looks at the unix users and groups of the machine it
// runs on: the logins the tests migrate (alex, alex2, shop) may well exist
// there. fakeUnixAccounts says which ones a test wants to exist.
func init() { unixAccountExists = func(string) bool { return false } }

// fakeUnixAccounts makes the host look as if it had these unix accounts and
// no others: the import refuses a login that exists outside the panel, and a
// test must not depend on the accounts of the machine it runs on.
func fakeUnixAccounts(t *testing.T, logins ...string) {
	t.Helper()
	was := unixAccountExists
	have := map[string]bool{}
	for _, l := range logins {
		have[l] = true
	}
	unixAccountExists = func(login string) bool { return have[login] }
	t.Cleanup(func() { unixAccountExists = was })
}

// The real lookup: a name a group alone has is taken too, useradd -U would
// fail on it.
func TestUnixAccountExistsLooksAtGroups(t *testing.T) {
	if _, err := user.LookupGroup("users"); err != nil {
		t.Skip("no group users on this machine")
	}
	if _, err := user.Lookup("users"); err == nil {
		t.Skip("a user named users exists on this machine")
	}
	if !lookupUnixName("users") || !lookupUnixName("root") || lookupUnixName("mp-no-such-name-7f3a") {
		t.Errorf("users %v, root %v, a made-up name %v", lookupUnixName("users"), lookupUnixName("root"), lookupUnixName("mp-no-such-name-7f3a"))
	}
}

// The dump on its way to mysql: every DEFINER becomes the loading account,
// the binlog and GTID lines of a GTID source go, data passes untouched —
// including a line longer than the filter looks into.
func TestDumpFilter(t *testing.T) {
	long := "INSERT INTO `t` VALUES ('" + strings.Repeat("DEFINER=`x`@`y` ", 6000) + "');\n"
	in := "-- MySQL dump\n" +
		"SET @MYSQLDUMP_TEMP_LOG_BIN = @@SESSION.SQL_LOG_BIN;\n" +
		"SET @@SESSION.SQL_LOG_BIN= 0;\n" +
		"SET @@GLOBAL.GTID_PURGED=/*!80000 '+'*/ '3E11FA47-71CA-11E1-9E33-C80AA9429562:1-5,\n" +
		"4E11FA47-71CA-11E1-9E33-C80AA9429562:1-3';\n" +
		"/*!50001 CREATE ALGORITHM=UNDEFINED */\n" +
		"/*!50013 DEFINER=`root`@`localhost` SQL SECURITY DEFINER */\n" +
		"/*!50001 VIEW `v` AS select 1 AS `1` */;\n" +
		"/*!50003 CREATE*/ /*!50017 DEFINER='other'@'%'*/ /*!50003 TRIGGER `tr` BEFORE INSERT ON `t` FOR EACH ROW SET @x = 1 */;;\n" +
		"CREATE DEFINER=root@localhost PROCEDURE `p`()\n" +
		"INSERT INTO `t` VALUES ('DEFINER=`kept`@`as data`');\n" +
		long +
		"/*!50106 CREATE*/ /*!50117 DEFINER = CURRENT_USER */ /*!50106 EVENT `e` ON SCHEDULE EVERY 1 DAY DO SELECT 1 */ ;;\n" +
		"SET @@SESSION.SQL_LOG_BIN = @MYSQLDUMP_TEMP_LOG_BIN;\n" +
		"-- end, no newline"
	f := newDumpFilter(iotest.OneByteReader(strings.NewReader(in)))
	raw, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{
		"SET @MYSQLDUMP_TEMP_LOG_BIN = @@SESSION.SQL_LOG_BIN;\n",
		"/*!50013 DEFINER=CURRENT_USER SQL SECURITY DEFINER */\n",
		"/*!50017 DEFINER=CURRENT_USER*/ /*!50003 TRIGGER",
		"CREATE DEFINER=CURRENT_USER PROCEDURE `p`()\n",
		"INSERT INTO `t` VALUES ('DEFINER=`kept`@`as data`');\n",
		long,
		"/*!50117 DEFINER = CURRENT_USER */",
		"-- end, no newline",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lost %.80q", want)
		}
	}
	for _, gone := range []string{"SQL_LOG_BIN= 0", "GTID_PURGED", "4E11FA47", "`root`@`localhost`", "'other'@'%'", "SQL_LOG_BIN = @MYSQLDUMP"} {
		if strings.Contains(out, gone) {
			t.Errorf("%q passed the filter", gone)
		}
	}
	if f.definers != 3 || f.dropped != 3 {
		t.Errorf("counted %d definers, %d dropped lines", f.definers, f.dropped)
	}
}

// The hashes a bundle carries: only the schemes the panel writes, with
// parameters a sign-in can afford, nothing that adds a field or a line.
func TestMigrateSecretFormats(t *testing.T) {
	panel, _ := auth.HashPassword("a-password")
	if err := checkPanelHash(panel); err != nil {
		t.Errorf("the panel's own hash: %v", err)
	}
	for _, bad := range []string{
		"$argon2id$v=19$m=4194304,t=1,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g",
		"$argon2id$v=19$m=19456,t=2,p=0$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g",
		"$argon2i$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g",
		"$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA",
		"plain-text",
		panel + "\nroot:x",
	} {
		if checkPanelHash(bad) == nil {
			t.Errorf("panel hash %q passed", bad)
		}
	}
	for _, good := range []string{"$y$j9T$salt$hash1234", "$6$rounds=5000$saltsalt$hashhashhash", "$2b$10$" + strings.Repeat("a", 53)} {
		if err := checkShadowHash(good); err != nil {
			t.Errorf("shadow %q: %v", good, err)
		}
	}
	for _, bad := range []string{"$6$salt$hash:0:0:root", "$1$md5$weakweak", "!", "*", "$y$j9T$salt\nroot:$y$x", "plain"} {
		if checkShadowHash(bad) == nil {
			t.Errorf("shadow %q passed", bad)
		}
	}
	box, _ := hashMailPassword("a-password")
	if err := checkMailHash(box); err != nil {
		t.Errorf("the panel's mailbox hash: %v", err)
	}
	for _, bad := range []string{
		"{BLF-CRYPT}x:0:0::/etc::userdb_uid=0",
		"{PLAIN}password",
		"{BLF-CRYPT}$2a$31$" + strings.Repeat("a", 53),
		strings.Replace(box, "$11$", "$11$:", 1),
		box + "\nother@example.com:{PLAIN}x",
	} {
		if checkMailHash(bad) == nil {
			t.Errorf("mailbox hash %q passed", bad)
		}
	}
	priv, pub, err := newDKIMKey()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := dkimPublicKey(priv); err != nil || got != pub || dkimPublicOK(pub) != nil {
		t.Errorf("DKIM key: %v", err)
	}
	for _, bad := range []string{"access_log /etc/cron.d/x;", priv + "\nmore text", "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"} {
		if _, err := dkimPublicKey(bad); err == nil {
			t.Errorf("DKIM key %.40q passed", bad)
		}
	}
}

// Paths of the old home follow the account to its new one; a longer name
// that only starts the same way does not.
func TestRewritePaths(t *testing.T) {
	r := [][2]string{{"/var/www/alex/", "/var/www/alex2/"}}
	for in, want := range map[string]string{
		"/var/www/alex/data/app/run --dir /var/www/alex/data": "/var/www/alex2/data/app/run --dir /var/www/alex2/data",
		"/var/www/alex":                   "/var/www/alex2",
		"cd '/var/www/alex' && php x.php": "cd '/var/www/alex2' && php x.php",
		"/var/www/alexander/data":         "/var/www/alexander/data",
	} {
		if got := rewritePaths(in, r); got != want {
			t.Errorf("rewritePaths(%q) = %q, want %q", in, got, want)
		}
	}
	if got := rewritePaths("/var/www/alex/x", nil); got != "/var/www/alex/x" {
		t.Errorf("no rewrites: %q", got)
	}
}

// What mysql or tar prints quotes the source's data: one entry of the job log
// each, without line breaks or terminal escapes.
func TestToolOutput(t *testing.T) {
	got := toolOutput("ERROR 1064 (42000) at line 3: near 'x\nrefused: site y.example.org'\x1b[2J\n\n")
	if got != "ERROR 1064 (42000) at line 3: near 'x / refused: site y.example.org'[2J" {
		t.Errorf("toolOutput: %q", got)
	}
}

// A comment the adapters build stays a label the panel takes: one line, at
// most what CronRequest allows, the adapter's own mark first.
func TestCronComment(t *testing.T) {
	if got := cronComment(cronSuspectMark, ""); got != cronSuspectMark {
		t.Errorf("mark alone: %q", got)
	}
	got := cronComment("from root's crontab", cronSuspectMark, strings.Repeat("ж", 200)+"\x1b[2J")
	if r := []rune(got); len(r) != cronCommentMax || !strings.HasPrefix(got, "from root's crontab; "+cronSuspectMark) || strings.ContainsRune(got, 0x1b) {
		t.Errorf("long comment: %d runes %q", len(r), got)
	}
}

// The files of the source reach tar -x (as root) only as files, directories
// and links inside the tree: a device node, a FIFO, a name or a hard link
// leading out, a member written through a symlink stay behind, and setuid bits
// go.
func TestMigrateTarKeepsOnlyFiles(t *testing.T) {
	for _, godebug := range []string{"", "tarinsecurepath=0"} {
		t.Run("GODEBUG="+godebug, func(t *testing.T) {
			t.Setenv("GODEBUG", godebug)
			testMigrateTar(t)
		})
	}
}

func testMigrateTar(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(h *tar.Header, body string) {
		t.Helper()
		h.Size = int64(len(body))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("%s: %v", h.Name, err)
		}
		if body != "" {
			tw.Write([]byte(body)) //nolint:errcheck // test
		}
	}
	add(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	add(&tar.Header{Name: "./data/www/site/index.php", Typeflag: tar.TypeReg, Mode: 0o644}, "<?php echo 1;\n")
	add(&tar.Header{Name: "./data/mem", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 1, Mode: 0o666}, "")
	add(&tar.Header{Name: "./data/sda", Typeflag: tar.TypeBlock, Devmajor: 8, Devminor: 0, Mode: 0o666}, "")
	add(&tar.Header{Name: "./data/fifo", Typeflag: tar.TypeFifo, Mode: 0o644}, "")
	add(&tar.Header{Name: "./data/suid", Typeflag: tar.TypeReg, Mode: 0o4755}, "#!/bin/sh\n")
	add(&tar.Header{Name: "./data/link", Typeflag: tar.TypeSymlink, Linkname: "/etc", Mode: 0o777}, "")
	// The source rewrote these targets to the home here: one inside it
	// arrives relative, one that only starts like it or climbs out does not.
	add(&tar.Header{Name: "./data/www/shop/bitrix", Typeflag: tar.TypeSymlink, Linkname: "/var/www/u/data/www/site/bitrix", Mode: 0o777}, "")
	add(&tar.Header{Name: "./data/near", Typeflag: tar.TypeSymlink, Linkname: "/var/www/uu/data", Mode: 0o777}, "")
	add(&tar.Header{Name: "./data/climb", Typeflag: tar.TypeSymlink, Linkname: "/var/www/u/../../../etc", Mode: 0o777}, "")
	add(&tar.Header{Name: "./data/link/cron.d/x", Typeflag: tar.TypeReg, Mode: 0o644}, "* * * * * root id\n")
	add(&tar.Header{Name: "./data/hard", Typeflag: tar.TypeLink, Linkname: "../../etc/shadow"}, "")
	add(&tar.Header{Name: "./data/hard2", Typeflag: tar.TypeLink, Linkname: "./data/www/site/index.php"}, "")
	add(&tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o644}, "x")
	add(&tar.Header{Name: "/etc/cron.d/abs", Typeflag: tar.TypeReg, Mode: 0o644}, "x")
	add(&tar.Header{Name: "./data/after", Typeflag: tar.TypeReg, Mode: 0o644}, "kept")
	tw.Close()

	clean, st, done := migrateTar(bytes.NewReader(buf.Bytes()), false, "/var/www/u")
	out, err := io.ReadAll(clean)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*tar.Header{}
	tr := tar.NewReader(bytes.NewReader(out))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "./data/www/site/index.php" {
			if body, _ := io.ReadAll(tr); string(body) != "<?php echo 1;\n" {
				t.Errorf("content: %q", body)
			}
		}
		got[h.Name] = h
	}
	for _, kept := range []string{"./", "./data/www/site/index.php", "./data/suid", "./data/hard2", "./data/after", "./data/www/shop/bitrix"} {
		if got[kept] == nil {
			t.Errorf("%s was left out", kept)
		}
	}
	// ./data/link leads out of the tree (to /etc): a later pass of tar -x,
	// root's, would write through it.
	for _, gone := range []string{"./data/mem", "./data/sda", "./data/fifo", "./data/link", "./data/link/cron.d/x", "./data/hard", "../escape", "/etc/cron.d/abs", "./data/near", "./data/climb"} {
		if got[gone] != nil {
			t.Errorf("%s passed the filter", gone)
		}
	}
	if h := got["./data/suid"]; h != nil && h.Mode != 0o755 {
		t.Errorf("setuid kept: %o", h.Mode)
	}
	if h := got["./data/www/shop/bitrix"]; h != nil && h.Linkname != "../site/bitrix" {
		t.Errorf("a link into the home arrives as %q, want ../site/bitrix", h.Linkname)
	}
	if st.dropped != 10 || !strings.Contains(st.first, "data/mem") {
		t.Errorf("stats: %+v", st)
	}
	for name, want := range map[string]bool{"data/x": true, "./data/x": true, "../x": false, "/etc/x": false, "a/../../x": false, "a/../b": false, "": false} {
		if _, ok := tarMemberPath(name); ok != want {
			t.Errorf("tarMemberPath(%q) = %v", name, ok)
		}
	}
	// A broken archive fails the stream instead of ending it quietly.
	bad, _, badDone := migrateTar(strings.NewReader(strings.Repeat("x", 1024)), false, "/var/www/u")
	if _, err := io.ReadAll(bad); err == nil {
		t.Error("a broken archive must fail the stream")
	}
	<-badDone
}

// A database-level grant names its database literally: _ and % are wildcards
// there, and the source picks the names.
func TestGrantDB(t *testing.T) {
	for in, want := range map[string]string{
		"shop":       "`shop`",
		"evil_db":    "`evil\\_db`",
		"_____":      "`\\_\\_\\_\\_\\_`", // would match every five-letter database, mysql included
		"a%b`c\\d_e": "`a\\%b``c\\\\d\\_e`",
	} {
		if got := grantDB(in); got != want {
			t.Errorf("grantDB(%q) = %s, want %s", in, got, want)
		}
	}
}

// In the mail store every Maildir is vmail's and dovecot follows links: a
// link there stays below its own directory, so a mailbox cannot be another
// domain's Maildir. In a home such links stay (nginx and the account's own
// permissions keep them in check there).
func TestMigrateTarMailLinksStayInside(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range []*tar.Header{
		{Name: "info/", Typeflag: tar.TypeDir, Mode: 0o700},
		{Name: "info/.dovecot.sieve", Typeflag: tar.TypeSymlink, Linkname: "sieve/managesieve.sieve"},
		{Name: "boss", Typeflag: tar.TypeSymlink, Linkname: "/var/mail/monopanel/victim.example.org/boss"},
		{Name: "sales", Typeflag: tar.TypeSymlink, Linkname: "../victim.example.org/sales"},
		{Name: "info/up", Typeflag: tar.TypeSymlink, Linkname: "cur/../../.."},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	for confined, want := range map[bool][]string{
		true: {"info/", "info/.dovecot.sieve"},
		// An account's tree keeps links that stay inside it, mail or not.
		false: {"info/", "info/.dovecot.sieve"},
	} {
		clean, st, done := migrateTar(bytes.NewReader(buf.Bytes()), confined, "/var/www/u")
		out, err := io.ReadAll(clean)
		<-done
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		tr := tar.NewReader(bytes.NewReader(out))
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, h.Name)
		}
		if strings.Join(got, " ") != strings.Join(want, " ") || st.dropped != 5-len(want) {
			t.Errorf("confined=%v: %v, dropped %d", confined, got, st.dropped)
		}
		if confined && !strings.Contains(st.first, "boss (a symlink to /var/mail/monopanel/victim.example.org/boss, outside its own directory)") {
			t.Errorf("first dropped: %q", st.first)
		}
	}
	for target, ok := range map[string]bool{"sieve/x.sieve": true, "./cur": true, "a/b/c": true, "": false, "/etc": false, "..": false, "a/../b": false, "a\x00b": false} {
		if tarConfinedLink(target) != ok {
			t.Errorf("tarConfinedLink(%q) != %v", target, ok)
		}
	}
}

// rawTarHeader builds a ustar header block by hand: Go's tar writer cannot
// write the sparse members a source may send.
func rawTarHeader(name string, flag byte, size int64) []byte {
	h := make([]byte, 512)
	copy(h[0:], name)
	copy(h[100:], "0000644\x00")
	copy(h[108:], "0000000\x00")
	copy(h[116:], "0000000\x00")
	copy(h[124:], fmt.Sprintf("%011o\x00", size))
	copy(h[136:], fmt.Sprintf("%011o\x00", 0))
	h[156] = flag
	copy(h[257:], "ustar\x0000")
	copy(h[148:], "        ")
	sum := 0
	for _, b := range h {
		sum += int(b)
	}
	copy(h[148:], fmt.Sprintf("%06o\x00 ", sum))
	return h
}

func padBlock(b []byte) []byte {
	if n := len(b) % 512; n != 0 {
		b = append(b, make([]byte, 512-n)...)
	}
	return b
}

// paxRecord is one "<length> key=value\n" record; the length counts itself.
func paxRecord(k, v string) string {
	rec := " " + k + "=" + v + "\n"
	n := len(rec) + 1
	for len(strconv.Itoa(n))+len(rec) != n {
		n = len(strconv.Itoa(n)) + len(rec)
	}
	return strconv.Itoa(n) + rec
}

// sparseTar is an archive with one GNU sparse file (PAX format 1.0): size
// bytes long, a hole, then data at its end.
func sparseTar(name string, size int64, data []byte) []byte {
	recs := paxRecord("GNU.sparse.major", "1") + paxRecord("GNU.sparse.minor", "0") +
		paxRecord("GNU.sparse.name", name) + paxRecord("GNU.sparse.realsize", strconv.FormatInt(size, 10))
	out := append(rawTarHeader("PaxHeaders/sparse", 'x', int64(len(recs))), padBlock([]byte(recs))...)
	body := append(padBlock([]byte(fmt.Sprintf("1\n%d\n%d\n", size-int64(len(data)), len(data)))), data...)
	out = append(out, rawTarHeader("GNUSparseFile.0/sparse", '0', int64(len(body)))...)
	out = append(out, padBlock(body)...)
	return append(out, make([]byte, 1024)...)
}

// Holes of sparse files arrive as zeros (Go's tar writer writes no sparse
// members), and only so many: a few bytes of the source's archive must not
// claim the disk. A plain file costs nothing against the budget.
func TestMigrateTarSparseBudget(t *testing.T) {
	was := migrateTarHoleBudget
	migrateTarHoleBudget = 1 << 20
	t.Cleanup(func() { migrateTarHoleBudget = was })
	data := []byte("the end of a sparse file")
	run := func(archive []byte) ([]byte, *migrateTarStats, error) {
		clean, st, done := migrateTar(bytes.NewReader(archive), false, "/var/www/u")
		out, err := io.ReadAll(clean)
		<-done
		return out, st, err
	}

	out, st, err := run(sparseTar("data/disk.img", 512<<10, data))
	if err != nil || st.err != nil {
		t.Fatalf("within the budget: %v %v", err, st.err)
	}
	tr := tar.NewReader(bytes.NewReader(out))
	h, err := tr.Next()
	if err != nil || h.Name != "data/disk.img" || h.Typeflag != tar.TypeReg || h.Size != 512<<10 {
		t.Fatalf("sparse member as written: %+v %v", h, err)
	}
	body, _ := io.ReadAll(tr)
	if hole := body[:len(body)-len(data)]; !bytes.HasSuffix(body, data) || len(bytes.Trim(hole, "\x00")) != 0 {
		t.Errorf("content: %d bytes, ends %q", len(body), body[len(body)-len(data):])
	}
	if st.holes != 512<<10-int64(len(data)) {
		t.Errorf("holes counted: %d", st.holes)
	}

	if _, st, err := run(sparseTar("data/huge.img", 1<<40, data)); err == nil || !strings.Contains(err.Error(), "sparse files would arrive as more than 1 MiB of zeros (data/huge.img)") || st.holes > 2<<20 {
		t.Errorf("a terabyte of holes: %v (holes %d)", err, st.holes)
	}

	var plain bytes.Buffer
	tw := tar.NewWriter(&plain)
	zeros := make([]byte, 2<<20)
	tw.WriteHeader(&tar.Header{Name: "data/zeros", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(zeros))}) //nolint:errcheck // test
	tw.Write(zeros)                                                                                              //nolint:errcheck // test
	tw.Close()
	if _, st, err := run(plain.Bytes()); err != nil || st.holes != 0 {
		t.Errorf("a plain file of zeros: %v (holes %d)", err, st.holes)
	}
}

// What a sign-in costs is the source's choice unless the hash is held to the
// parameters chpasswd writes.
func TestShadowHashCost(t *testing.T) {
	for _, good := range []string{
		"$y$j9T$saltsaltsalt$hashhashhash", "$y$jBT$salt$hash", "$gy$j9T$salt$hash",
		"$6$salt$hash", "$6$rounds=656000$salt$hash", "$5$rounds=5000$salt$hash",
		"$2b$12$" + strings.Repeat("a", 53),
	} {
		if err := checkShadowHash(good); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}
	for _, bad := range []string{
		"$y$jCT$salt$hash",  // 128 MiB at every attempt
		"$y$jFT$salt$hash",  // 1 GiB
		"$y$j9T.$salt$hash", // parameters libxcrypt does not write
		"$y$k9T$salt$hash",  // another flavour
		"$y$jzT$salt$hash",  // a multi-character N
		"$6$rounds=999999999$salt$hash",
		"$6$rounds=x$salt$hash",
		"$2b$15$" + strings.Repeat("a", 53),
		"$7$C6..../....SodiumChloride$kBGj9fHznVYFQMEn/qDCfrDevf9YDtcDdKvEqHJLV8D", // scrypt: no system here writes it
	} {
		if err := checkShadowHash(bad); err == nil {
			t.Errorf("%s passed", bad)
		}
	}
}

// Files arrive in more than one pass, each extracted by root: a link that
// leads out of the tree, made by one pass, would carry the next pass's files
// to /etc. Links in an account's tree must stay inside it.
func TestMigrateTarKeepsLinksInsideTheTree(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, l := range []struct{ name, target string }{
		{"data/x", "/etc"},
		{"data/www/site/up", "../../../../etc"},
		{"data/www/site/public/storage", "../storage/app/public"},
		{"data/www/site/current", "releases/42"},
	} {
		tw.WriteHeader(&tar.Header{Name: l.name, Typeflag: tar.TypeSymlink, Linkname: l.target, Mode: 0o777}) //nolint:errcheck // test
	}
	tw.Close()
	clean, st, done := migrateTar(bytes.NewReader(buf.Bytes()), false, "/var/www/u")
	out, err := io.ReadAll(clean)
	<-done
	if err != nil || st.err != nil {
		t.Fatal(err, st.err)
	}
	tr := tar.NewReader(bytes.NewReader(out))
	var kept []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		kept = append(kept, h.Name)
	}
	if strings.Join(kept, ",") != "data/www/site/public/storage,data/www/site/current" || st.dropped != 2 {
		t.Errorf("kept %v, dropped %d (%s)", kept, st.dropped, st.first)
	}
}
