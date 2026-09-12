package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Чужие панели не умеют отдавать аккаунт по API, поэтому адаптеры читают
// старый сервер так, как это сделал бы администратор: по ssh от root. Всё,
// что здесь есть, — только чтение: cat, ls, tar -c, mysqldump.

// sshTarget is root@host[:port] as people write it.
type sshTarget struct {
	user, host, port string
}

func parseSSHTarget(s string) (sshTarget, error) {
	t := sshTarget{user: "root", port: "22"}
	s = strings.TrimSpace(strings.TrimPrefix(s, "ssh://"))
	if i := strings.LastIndex(s, "@"); i >= 0 {
		t.user, s = s[:i], s[i+1:]
	}
	if h, p, err := net.SplitHostPort(s); err == nil {
		s, t.port = h, p
	}
	t.host = strings.Trim(s, "[]")
	if t.host == "" || t.user == "" || strings.ContainsAny(t.host, " /") {
		return t, fmt.Errorf("адрес источника должен быть вида root@host или root@host:22, получено %q", s)
	}
	return t, nil
}

// sshConn is one authenticated connection; every command runs in its own
// session.
type sshConn struct {
	client      *ssh.Client
	target      sshTarget
	fingerprint string
}

// dialSSH connects with a private key (PEM) and/or a password. The host key
// is not checked against anything — the panel has no known_hosts of its own
// — but its fingerprint is kept and shown, so the administrator can compare
// it with what the old server prints.
func dialSSH(ctx context.Context, target, password, key string) (*sshConn, error) {
	t, err := parseSSHTarget(target)
	if err != nil {
		return nil, err
	}
	var auth []ssh.AuthMethod
	if strings.TrimSpace(key) != "" {
		signer, err := ssh.ParsePrivateKey([]byte(key))
		if err != nil {
			return nil, fmt.Errorf("приватный ключ не разобрать: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if password != "" {
		auth = append(auth, ssh.Password(password), ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
			answers := make([]string, len(questions))
			for i := range answers {
				answers[i] = password
			}
			return answers, nil
		}))
	}
	if len(auth) == 0 {
		return nil, errors.New("для доступа по ssh нужен пароль или приватный ключ")
	}
	c := &sshConn{target: t}
	cfg := &ssh.ClientConfig{
		User:    t.user,
		Auth:    auth,
		Timeout: 20 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			c.fingerprint = ssh.FingerprintSHA256(key)
			return nil
		},
	}
	addr := net.JoinHostPort(t.host, t.port)
	d := net.Dialer{Timeout: cfg.Timeout}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("источник %s недоступен: %w", addr, err)
	}
	conn, chans, reqs, err := ssh.NewClientConn(raw, addr, cfg)
	if err != nil {
		raw.Close() //nolint:errcheck // соединение и так не состоялось
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, fmt.Errorf("источник %s не принял пароль или ключ для %s", addr, t.user)
		}
		return nil, fmt.Errorf("ssh %s: %w", addr, err)
	}
	c.client = ssh.NewClient(conn, chans, reqs)
	return c, nil
}

func (c *sshConn) close() {
	if c != nil && c.client != nil {
		c.client.Close() //nolint:errcheck // закрываем на выходе, сказать об ошибке некому
	}
}

// exec runs a command and returns stdout, its exit code and the transport
// error, if any. Stderr rides along in the error text of a failed command.
func (c *sshConn) exec(ctx context.Context, cmd string) (string, int, error) {
	sess, err := c.client.NewSession()
	if err != nil {
		return "", -1, fmt.Errorf("ssh: %w", err)
	}
	defer sess.Close() //nolint:errcheck // сессия одноразовая
	var stdout, stderr bytes.Buffer
	sess.Stdout, sess.Stderr = &stdout, &limitedBuffer{max: 4096, buf: &stderr}
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case <-ctx.Done():
		sess.Close() //nolint:errcheck // отмена: результат уже не нужен
		return "", -1, ctx.Err()
	case err = <-done:
	}
	if err != nil {
		var exit *ssh.ExitError
		if errors.As(err, &exit) {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = fmt.Sprintf("код %d", exit.ExitStatus())
			}
			return stdout.String(), exit.ExitStatus(), errors.New(msg)
		}
		return stdout.String(), -1, fmt.Errorf("ssh: %w", err)
	}
	return stdout.String(), 0, nil
}

// readFile returns a file's content; a missing file is an error.
func (c *sshConn) readFile(ctx context.Context, path string) (string, error) {
	out, _, err := c.exec(ctx, "cat "+shq(path))
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}

// exists answers whether a path is there (a file, a directory, a symlink).
func (c *sshConn) exists(ctx context.Context, path string) bool {
	_, code, err := c.exec(ctx, "test -e "+shq(path))
	return err == nil && code == 0
}

// glob lists what a shell pattern expands to, one path per line, sorted.
func (c *sshConn) glob(ctx context.Context, pattern string) []string {
	out, _, _ := c.exec(ctx, "ls -1d "+pattern+" 2>/dev/null")
	var paths []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			paths = append(paths, l)
		}
	}
	return paths
}

// stream starts a command and hands its stdout back as a reader. A remote
// failure surfaces as the reader's error instead of a clean EOF: an empty
// mysqldump must not turn into an empty database on this side.
func (c *sshConn) stream(ctx context.Context, cmd string) (io.ReadCloser, error) {
	sess, err := c.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("ssh: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close() //nolint:errcheck // сессия не понадобилась
		return nil, err
	}
	st := &sshStream{sess: sess, out: stdout, done: make(chan error, 1), closed: make(chan struct{})}
	sess.Stderr = &limitedBuffer{max: 4096, buf: &st.stderr}
	if err := sess.Start(cmd); err != nil {
		sess.Close() //nolint:errcheck // команда не запустилась
		return nil, fmt.Errorf("%s: %w", firstWord(cmd), err)
	}
	go func() { st.done <- sess.Wait() }()
	go func() {
		select {
		case <-ctx.Done():
			sess.Close() //nolint:errcheck // отмена: поток обрывается намеренно
		case <-st.closed:
		}
	}()
	return st, nil
}

type sshStream struct {
	sess   *ssh.Session
	out    io.Reader
	stderr bytes.Buffer
	done   chan error
	closed chan struct{}
	final  error
	ended  bool
}

func (s *sshStream) Read(p []byte) (int, error) {
	n, err := s.out.Read(p)
	if errors.Is(err, io.EOF) && !s.ended {
		s.ended = true
		if werr := <-s.done; werr != nil {
			msg := strings.TrimSpace(s.stderr.String())
			if msg == "" {
				msg = werr.Error()
			}
			s.final = errors.New(msg)
			return n, s.final
		}
	}
	if s.final != nil && errors.Is(err, io.EOF) {
		return n, s.final
	}
	return n, err
}

// Close releases the session. The http transport closes a request body
// after sending it, and a session whose channel the remote already closed
// answers EOF — that is not an error worth reporting.
func (s *sshStream) Close() error {
	if s.closed != nil {
		select {
		case <-s.closed:
		default:
			close(s.closed)
		}
	}
	s.sess.Close() //nolint:errcheck // после конца потока сессия уже закрыта
	return nil
}

// limitedBuffer keeps the head of stderr: enough for an error message, not
// enough to hold a runaway process's output in memory.
type limitedBuffer struct {
	max int
	buf *bytes.Buffer
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
		} else {
			l.buf.Write(p)
		}
	}
	return len(p), nil
}

// shq quotes one shell word.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func firstWord(cmd string) string {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return cmd
	}
	return f[0]
}
