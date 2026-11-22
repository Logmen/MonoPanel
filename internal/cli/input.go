package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Answers and secrets come from stdin. The TUI and scripts run commands with a
// pipe or nothing at all there, so nothing below may wait for a person who is
// not present: a closed stdin is an error that names the flag to use instead.
// Ctrl-C ends a wait at once (Main turns SIGINT into a cancelled context, which
// a blocked read of the terminal would not notice by itself). Tests replace
// these.
var (
	stdin           io.Reader = os.Stdin
	stdinIsTerminal           = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	readNoEcho                = readTerminalNoEcho
)

// stdinSource is stdin with its buffer. A wait given up on leaves its read
// running; the lock makes the next read wait for it instead of racing it.
type stdinSource struct {
	src io.Reader
	mu  sync.Mutex
	buf *bufio.Reader
}

var (
	stdinMu  sync.Mutex
	stdinCur *stdinSource
)

// currentStdin is stdin as a stdinSource, made anew when tests swap stdin.
func currentStdin() *stdinSource {
	stdinMu.Lock()
	defer stdinMu.Unlock()
	if stdinCur == nil || stdinCur.src != stdin {
		stdinCur = &stdinSource{src: stdin, buf: bufio.NewReader(stdin)}
	}
	return stdinCur
}

// stdinLine reads one line without its line break; io.EOF means stdin had
// nothing (more) to give, ctx.Err() that the wait was cancelled.
func stdinLine(ctx context.Context) (string, error) {
	in := currentStdin()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		in.mu.Lock()
		defer in.mu.Unlock()
		line, err := in.buf.ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || line == "") {
			ch <- result{"", err}
			return
		}
		ch <- result{strings.TrimRight(line, "\r\n"), nil}
	}()
	select {
	case res := <-ch:
		return res.line, res.err
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr)
		return "", ctx.Err()
	}
}

// readTerminalNoEcho reads a line from the terminal without echoing it. On
// Ctrl-C the terminal gets its echo back before the command ends.
func readTerminalNoEcho(ctx context.Context) (string, error) {
	fd := int(os.Stdin.Fd())
	state, err := term.GetState(fd)
	if err != nil {
		return "", err
	}
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := term.ReadPassword(fd)
		ch <- result{b, err}
	}()
	select {
	case res := <-ch:
		fmt.Fprintln(os.Stderr)
		return string(res.b), res.err
	case <-ctx.Done():
		term.Restore(fd, state) //nolint:errcheck // best effort: the command is ending anyway
		fmt.Fprintln(os.Stderr)
		return "", ctx.Err()
	}
}

// readSecret reads the value of a --password-stdin style flag: the first line
// of stdin, or a prompt without echo when stdin is a terminal. Nothing read is
// an error, not an empty password: a closed stdin must not, say, create an
// account without one.
func readSecret(ctx context.Context, flag, prompt string) (string, error) {
	if stdinIsTerminal() {
		fmt.Fprint(os.Stderr, prompt)
		s, err := readNoEcho(ctx)
		if errors.Is(err, io.EOF) || (err == nil && s == "") {
			return "", &exitError{code: 2, msg: T("ничего не введено", "nothing was entered")}
		}
		return s, err
	}
	s, err := stdinLine(ctx)
	if errors.Is(err, io.EOF) || (err == nil && s == "") {
		return "", &exitError{code: 2, msg: fmt.Sprintf(T("--%s: на stdin пусто", "--%s: nothing on stdin"), flag)}
	}
	return s, err
}

// confirm asks a yes/no question on stderr and reads the answer from stdin.
// Without an answer (stdin closed, as under the TUI or in a script) it fails at
// once and points at --yes instead of waiting.
func confirm(ctx context.Context, question string) error {
	fmt.Fprint(os.Stderr, question+" [y/N] ")
	line, err := stdinLine(ctx)
	if err != nil {
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(os.Stderr)
			return &exitError{code: 2, msg: T("ответа нет (stdin закрыт): подтвердите флагом --yes", "no answer (stdin is closed): confirm with --yes")}
		}
		return err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes", T("д", "y"), T("да", "yes"):
		return nil
	}
	return &exitError{code: 1, msg: T("отменено", "cancelled")}
}

// readKeyValues reads KEY=VALUE lines for a --cred-stdin style flag up to the
// end of stdin or an empty line, into dst (a key also given on the command
// line is overridden): credentials stay out of the process list.
func readKeyValues(ctx context.Context, flag string, dst map[string]string) error {
	if stdinIsTerminal() {
		fmt.Fprintln(os.Stderr, T("KEY=VALUE по одному в строке, пустая строка — конец:", "KEY=VALUE one per line, an empty line ends:"))
	}
	n := 0
	for {
		line, err := stdinLine(ctx)
		if errors.Is(err, io.EOF) || (err == nil && strings.TrimSpace(line) == "") {
			break
		}
		if err != nil {
			return err
		}
		k, v, ok := strings.Cut(line, "=")
		if k = strings.TrimSpace(k); !ok || k == "" {
			return &exitError{code: 2, msg: fmt.Sprintf(T("--%s: ожидается KEY=VALUE в каждой строке", "--%s: expected KEY=VALUE on every line"), flag)}
		}
		dst[k] = v
		n++
	}
	if n == 0 {
		return &exitError{code: 2, msg: fmt.Sprintf(T("--%s: на stdin пусто", "--%s: nothing on stdin"), flag)}
	}
	return nil
}

// shareStdin checks the *-stdin flags given to a command against what one
// stdin carries: one value (--password-stdin and the like) on its first line,
// then one list of KEY=VALUE lines (--env-stdin, --cred-stdin) up to the end
// or an empty line. The command reads them in that order — secretFlag first,
// readKeyValues after it — which is also the order the TUI pipes them in. Two
// values, or two lists, would leave which line is which to a guess.
func shareStdin(cmd *cobra.Command, values, lists []string) error {
	for _, names := range [][]string{values, lists} {
		var on []string
		for _, name := range names {
			if v, err := cmd.Flags().GetBool(name); err == nil && v {
				on = append(on, "--"+name)
			}
		}
		if len(on) > 1 {
			return &exitError{code: 2, msg: fmt.Sprintf(T("%s читают одну и ту же строку stdin: выберите одно", "%s read the same line of stdin: pick one"), strings.Join(on, T(" и ", " and ")))}
		}
	}
	return nil
}

// secretFlag reads a secret from stdin when the command's --<stdinFlag> (e.g.
// password-stdin) is set; otherwise dst keeps the value of the plain flag
// (plainFlag, e.g. password or admin-password; "" when there is none). A
// secret on the command line is visible to every local user in the process
// list, which is why the stdin twins exist.
func secretFlag(cmd *cobra.Command, stdinFlag, plainFlag, prompt string, dst *string) error {
	on, err := cmd.Flags().GetBool(stdinFlag)
	if err != nil {
		return err // the command has no such flag: a mistake in the code
	}
	if !on {
		return nil
	}
	if plainFlag != "" && cmd.Flags().Changed(plainFlag) {
		return &exitError{code: 2, msg: fmt.Sprintf(T("--%s и --%s вместе не задаются", "--%s and --%s cannot be used together"), plainFlag, stdinFlag)}
	}
	s, err := readSecret(cmd.Context(), stdinFlag, prompt)
	if err != nil {
		return err
	}
	*dst = s
	return nil
}
