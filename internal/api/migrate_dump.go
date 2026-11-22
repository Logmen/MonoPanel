package api

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"regexp"
)

// A dump is SQL of the source's choosing, and the mysql client that loads it
// runs as root here. Three things keep it to its own database:
//
//   - --binary-mode: the client's own commands (\! runs a shell, source reads
//     a file, tee writes one) stop working in piped input; only the delimiter
//     and charset commands of mysqldump's output remain;
//   - --local-infile=0: LOAD DATA LOCAL INFILE cannot have the client read a
//     file of this host into the database;
//   - the dump is loaded as an account of the database itself, which may do
//     anything inside it and nothing outside (see loadDump): no other
//     database (its grant names this one literally, see grantDB), no CREATE
//     USER, no GRANT, no SET GLOBAL.
//
// dumpFilter adapts what mysqldump writes to that account: a DEFINER naming
// another account (root, often) needs a privilege the account must not have,
// so every DEFINER becomes CURRENT_USER, which is the database's account; the
// session binlog and GTID lines a GTID-enabled source adds need SUPER and do
// nothing for a fresh database, so they are dropped. The filter is not what
// keeps the dump in its database — the account is — so a clause it misses only
// fails the load.

var (
	definerRe = regexp.MustCompile("(?i)\\bDEFINER\\s*=\\s*" +
		"(?:`(?:[^`]|``)*`|'(?:[^'\\\\]|\\\\.)*'|\"(?:[^\"\\\\]|\\\\.)*\"|[A-Za-z0-9_$.-]+)" +
		"\\s*@\\s*" +
		"(?:`(?:[^`]|``)*`|'(?:[^'\\\\]|\\\\.)*'|\"(?:[^\"\\\\]|\\\\.)*\"|[A-Za-z0-9_$.%:-]+)")
	binlogLineRe = regexp.MustCompile(`(?i)^\s*SET\s+@@(?:SESSION\.)?SQL_LOG_BIN\s*=`)
	gtidLineRe   = regexp.MustCompile(`(?i)^\s*SET\s+@@GLOBAL\.GTID_PURGED\s*=`)
	dataLineRe   = regexp.MustCompile(`(?i)^\s*(?:INSERT|REPLACE)\s`)
)

// dumpLineMax is the longest line the filter looks into: DDL is short, and a
// longer line is table data (an extended INSERT), passed through untouched.
const dumpLineMax = 64 << 10

type dumpFilter struct {
	r   *bufio.Reader
	buf []byte
	out []byte
	err error
	// long: inside a line longer than dumpLineMax.
	long bool
	// gtid: inside a SET @@GLOBAL.GTID_PURGED that spans lines.
	gtid bool
	// definers counts the DEFINER clauses rewritten, dropped the lines left out.
	definers, dropped int
}

func newDumpFilter(r io.Reader) *dumpFilter {
	return &dumpFilter{r: bufio.NewReaderSize(r, dumpLineMax)}
}

func (f *dumpFilter) Read(p []byte) (int, error) {
	for len(f.out) == 0 && f.err == nil {
		chunk, err := f.r.ReadSlice('\n')
		partial := errors.Is(err, bufio.ErrBufferFull)
		if err != nil && !partial {
			f.err = err
		}
		if len(chunk) > 0 {
			f.buf = append(f.buf[:0], chunk...)
			f.out = f.line(f.buf, partial)
		}
	}
	if len(f.out) == 0 {
		return 0, f.err
	}
	n := copy(p, f.out)
	f.out = f.out[n:]
	return n, nil
}

// line filters one line, or one piece of a line too long to look into.
func (f *dumpFilter) line(l []byte, partial bool) []byte {
	if f.long || partial {
		f.long = partial
		return l
	}
	if f.gtid {
		f.gtid = !bytes.HasSuffix(bytes.TrimRight(l, " \t\r\n"), []byte(";"))
		return nil
	}
	switch {
	case dataLineRe.Match(l):
		return l
	case binlogLineRe.Match(l):
		f.dropped++
		return nil
	case gtidLineRe.Match(l):
		f.dropped++
		f.gtid = !bytes.HasSuffix(bytes.TrimRight(l, " \t\r\n"), []byte(";"))
		return nil
	}
	if n := len(definerRe.FindAllIndex(l, -1)); n > 0 {
		f.definers += n
		return definerRe.ReplaceAll(l, []byte("DEFINER=CURRENT_USER"))
	}
	return l
}
