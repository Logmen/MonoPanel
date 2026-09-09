package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// tools maps a tool name to the binaries it may resolve to (first found wins).
var tools = map[string][]string{
	"mysql":           {"/usr/bin/mysql"},
	"mysqldump":       {"/usr/bin/mysqldump"},
	"mysqladmin":      {"/usr/bin/mysqladmin"},
	"percona-release": {"/usr/bin/percona-release"},
	"restic":          {"/usr/local/bin/restic", "/usr/bin/restic"},
	"fail2ban-client": {"/usr/bin/fail2ban-client"},
	"nft":             {"/usr/sbin/nft"},
	"crontab":         {"/usr/bin/crontab"},
	"tar":             {"/usr/bin/tar", "/bin/tar"},
	"du":              {"/usr/bin/du", "/bin/du"},
	"php":             {},
	"phpenmod":        {"/usr/sbin/phpenmod"},
	"phpdismod":       {"/usr/sbin/phpdismod"},
	"sshd":            {"/usr/sbin/sshd"},
	"setquota":        {"/usr/sbin/setquota"},
	"repquota":        {"/usr/sbin/repquota"},
	"journalctl":      {"/usr/bin/journalctl"},
	"apt-get":         {"/usr/bin/apt-get"},
	"dnf":             {"/usr/bin/dnf"},
	"nginx":           {"/usr/sbin/nginx"},
	"gzip":            {"/usr/bin/gzip", "/bin/gzip"},
	"chpasswd":        {"/usr/sbin/chpasswd"},
	"systemctl":       {"/usr/bin/systemctl", "/bin/systemctl"},
	"apachectl":       {"/usr/sbin/apachectl", "/usr/sbin/apache2ctl"},
	"postfix":         {"/usr/sbin/postfix"},
	"semanage":        {"/usr/sbin/semanage"},
	"setsebool":       {"/usr/sbin/setsebool"},
	"restorecon":      {"/usr/sbin/restorecon"},
	"postmap":         {"/usr/sbin/postmap"},
	"postconf":        {"/usr/sbin/postconf"},
	"postqueue":       {"/usr/sbin/postqueue"},
	"postsuper":       {"/usr/sbin/postsuper"},
	"doveadm":         {"/usr/bin/doveadm"},
	"doveconf":        {"/usr/bin/doveconf"},
	"newaliases":      {"/usr/bin/newaliases", "/usr/sbin/newaliases"},
}

// envAllowed lists environment variable prefixes a tool may receive.
var envAllowed = []string{"RESTIC_", "AWS_", "B2_", "AZURE_", "GOOGLE_", "OS_", "SWIFT_", "RCLONE_", "TZ=", "MYSQL_PWD="}

func resolveTool(name string) (string, error) {
	cands, ok := tools[name]
	if !ok {
		return "", errors.New("tool not allowed: " + name)
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil && len(cands) == 0 {
		return p, nil
	}
	return "", errors.New("tool not installed: " + name)
}

func (s *Server) tool(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
	bin, err := resolveTool(req.Name)
	if err != nil {
		return nil, &Error{Status: http.StatusBadRequest, Message: err.Error()}
	}
	for _, a := range req.Args {
		if strings.ContainsRune(a, 0) {
			return nil, &Error{Status: http.StatusBadRequest, Message: "invalid argument"}
		}
	}
	timeout := 10 * time.Minute
	if req.TimeoutSeconds > 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(rctx, bin, req.Args...)
	cmd.Env = append(os.Environ(), "LANG=C.UTF-8", "LC_ALL=C.UTF-8")
	for _, e := range req.Env {
		ok := false
		for _, pre := range envAllowed {
			if strings.HasPrefix(e, pre) {
				ok = true
			}
		}
		if !ok || strings.ContainsAny(e, "\n\x00") {
			return nil, &Error{Status: http.StatusBadRequest, Message: "environment variable not allowed: " + strings.SplitN(e, "=", 2)[0]}
		}
		cmd.Env = append(cmd.Env, e)
	}
	cmd.WaitDelay = 5 * time.Second
	if req.Stdin != "" {
		cmd.Stdin = strings.NewReader(req.Stdin)
	}
	out := &tailBuffer{max: 512 * 1024}
	cmd.Stderr = out
	if req.OutputFile != "" {
		p := filepath.Clean(req.OutputFile)
		if !s.pathAllowed(p) && !s.dirAllowed(filepath.Dir(p)) {
			return nil, &Error{Status: http.StatusForbidden, Message: "output path not allowed: " + p}
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		cmd.Stdout = f
	} else {
		cmd.Stdout = out
	}
	err = cmd.Run()
	resp := &ToolResponse{Output: out.String()}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			resp.ExitCode = ee.ExitCode()
			return resp, nil
		}
		return nil, &Error{Message: req.Name + ": " + err.Error(), Output: out.String()}
	}
	return resp, nil
}
