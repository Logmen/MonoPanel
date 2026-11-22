package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// The panel's own settings: the ones an administrator changes from any client
// (the web UI, mp and the TUI through PUT /system/config, which hands them to
// the agent) or as root with mp config set --local. Everything else in
// config.yaml — the paths, the service accounts, the agent's write allow-list
// and above all the release key (update.public_key) — is root's to edit by
// hand: the panel process must not be able to move its own trust anchor.
const (
	KeyHostname    = "web.hostname"
	KeyListen      = "web.listen"
	KeyLogLevel    = "log.level"
	KeyLogFormat   = "log.format"
	KeyJobsWorkers = "jobs.workers"
)

// MaxJobsWorkers bounds jobs.workers: every worker may be running a package
// manager, a backup or a migration at the same time.
const MaxJobsWorkers = 64

// PanelKeys lists the panel's own settings in the order they are shown.
func PanelKeys() []string {
	return []string{KeyHostname, KeyListen, KeyLogLevel, KeyLogFormat, KeyJobsWorkers}
}

// IsPanelKey reports whether key is one of PanelKeys.
func IsPanelKey(key string) bool {
	for _, k := range PanelKeys() {
		if k == key {
			return true
		}
	}
	return false
}

var hostLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// checkHostName accepts a DNS name in lower case: letters, digits and inner
// hyphens in labels of up to 63 characters. A dotted string of digits is an
// address, not a name.
func checkHostName(name string) error {
	if len(name) > 253 {
		return errors.New("a host name is at most 253 characters long")
	}
	if net.ParseIP(name) != nil || strings.Trim(name, "0123456789.") == "" {
		return fmt.Errorf("%s is an address, not a host name", name)
	}
	for _, label := range strings.Split(name, ".") {
		if !hostLabelRe.MatchString(label) {
			return fmt.Errorf("%q is not a valid host name: use letters, digits, hyphens and dots", name)
		}
	}
	return nil
}

// NormalizePanelValue checks a value of one of the panel keys and returns it
// the way config.yaml keeps it: a host name in lower case without the final
// dot, an address with the IP written canonically, a log level in lower case.
// It checks the form only; whether this host can listen there is for the
// caller to find out.
func NormalizePanelValue(key, value string) (string, error) {
	v := strings.TrimSpace(value)
	switch key {
	case KeyHostname:
		v = strings.ToLower(strings.TrimSuffix(v, "."))
		if v == "" {
			return "", nil // the panel is reached by its IP address then
		}
		if err := checkHostName(v); err != nil {
			return "", fmt.Errorf("web.hostname: %w; leave it empty to reach the panel by IP", err)
		}
		return v, nil
	case KeyListen:
		host, port, err := net.SplitHostPort(v)
		if err != nil {
			return "", fmt.Errorf("web.listen must be host:port or :port, e.g. :8443 or 203.0.113.5:8443, not %q", value)
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || port != strconv.Itoa(n) {
			return "", fmt.Errorf("web.listen: the port must be a number from 1 to 65535, not %q", port)
		}
		switch ip := net.ParseIP(host); {
		case host == "":
		case ip != nil:
			host = ip.String()
		default:
			host = strings.ToLower(host)
			if err := checkHostName(host); err != nil {
				return "", fmt.Errorf("web.listen: the host must be an IP address of this server or its name: %w", err)
			}
		}
		return net.JoinHostPort(host, port), nil
	case KeyLogLevel:
		switch v = strings.ToLower(v); v {
		case "debug", "info", "warn", "error":
			return v, nil
		case "warning":
			return "warn", nil
		}
		return "", fmt.Errorf("log.level must be debug, info, warn or error, not %q", value)
	case KeyLogFormat:
		switch v = strings.ToLower(v); v {
		case "text", "json":
			return v, nil
		}
		return "", fmt.Errorf("log.format must be text or json, not %q", value)
	case KeyJobsWorkers:
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxJobsWorkers {
			return "", fmt.Errorf("jobs.workers must be a number from 1 to %d, not %q", MaxJobsWorkers, value)
		}
		return strconv.Itoa(n), nil
	}
	return "", fmt.Errorf("%s cannot be changed from the panel; the keys are %s", key, strings.Join(PanelKeys(), ", "))
}

// PanelValue is the value of one of the panel keys in c, written the way
// config.yaml keeps it.
func (c Config) PanelValue(key string) string {
	switch key {
	case KeyHostname:
		return c.Web.Hostname
	case KeyListen:
		return c.Web.Listen
	case KeyLogLevel:
		return c.Log.Level
	case KeyLogFormat:
		return c.Log.Format
	case KeyJobsWorkers:
		return strconv.Itoa(c.Jobs.Workers)
	}
	return ""
}

// SetPanelValue checks a value of one of the panel keys and puts it into c.
func (c *Config) SetPanelValue(key, value string) error {
	v, err := NormalizePanelValue(key, value)
	if err != nil {
		return err
	}
	c.setPanelValue(key, v)
	return nil
}

func (c *Config) setPanelValue(key, v string) {
	switch key {
	case KeyHostname:
		c.Web.Hostname = v
	case KeyListen:
		c.Web.Listen = v
	case KeyLogLevel:
		c.Log.Level = v
	case KeyLogFormat:
		c.Log.Format = v
	case KeyJobsWorkers:
		c.Jobs.Workers, _ = strconv.Atoi(v)
	}
}

// KeyPublicKey is the release key. It is not a panel key: only root changes
// it, with mp update trust, never the API through the agent.
const KeyPublicKey = "update.public_key"

// EditPanelKeys returns data — the content of config.yaml — with the given
// panel keys set, and the keys whose value it changed. Nothing else changes:
// comments, keys this version does not know, "audit_file: none" and
// update.public_key stay exactly as they were, which encoding the whole
// Config again would not guarantee (Save drops comments and turns audit_file:
// none back into the default). The result is parsed again and compared with
// the original, so a layout this editor does not understand is refused
// instead of written. Empty data stands for a file that does not exist yet.
func EditPanelKeys(data []byte, set map[string]string) (out []byte, changed []string, err error) {
	values := make(map[string]string, len(set))
	for k, v := range set {
		nv, err := NormalizePanelValue(k, v)
		if err != nil {
			return nil, nil, err
		}
		values[k] = nv
	}
	return editKeys(data, values)
}

// EditFileKeys is EditPanelKeys for root's own tools (mp config set --local,
// mp update trust, mp setup): the panel keys and also update.public_key, which
// the caller has checked.
func EditFileKeys(data []byte, set map[string]string) (out []byte, changed []string, err error) {
	values := make(map[string]string, len(set))
	for k, v := range set {
		if k == KeyPublicKey {
			values[k] = strings.TrimSpace(v)
			continue
		}
		nv, err := NormalizePanelValue(k, v)
		if err != nil {
			return nil, nil, err
		}
		values[k] = nv
	}
	return editKeys(data, values)
}

// fileKeys are the keys editKeys knows, in the order they are written.
func fileKeys() []string { return append(PanelKeys(), KeyPublicKey) }

func (c Config) fileValue(key string) string {
	if key == KeyPublicKey {
		return c.Update.PublicKey
	}
	return c.PanelValue(key)
}

func (c *Config) setFileValue(key, v string) {
	if key == KeyPublicKey {
		c.Update.PublicKey = v
		return
	}
	c.setPanelValue(key, v)
}

func editKeys(data []byte, values map[string]string) (out []byte, changed []string, err error) {
	defer func() {
		// The YAML library panics on some trees it builds itself; the agent
		// must answer with an error instead.
		if r := recover(); r != nil {
			out, changed, err = nil, nil, fmt.Errorf("config.yaml: the YAML editor failed (%v); change the file by hand or with mp config set --local", r)
		}
	}()
	// Compared as Load reads the file, defaults included: a key the file
	// leaves out already has its default, and setting that changes nothing.
	before := Default()
	if err := yaml.Unmarshal(data, &before); err != nil {
		return nil, nil, fmt.Errorf("config.yaml does not parse: %w", err)
	}
	before.applyDefaults()
	want := before
	for _, k := range fileKeys() {
		v, ok := values[k]
		if !ok {
			continue
		}
		if before.fileValue(k) != v {
			changed = append(changed, k)
		}
		want.setFileValue(k, v)
	}
	if len(changed) == 0 {
		return data, nil, nil
	}
	if out, err = editYAML(data, values, changed); err != nil {
		return nil, nil, err
	}
	after := Default()
	if err := yaml.Unmarshal(out, &after); err != nil {
		return nil, nil, fmt.Errorf("config.yaml: the edited file does not parse (%v); change the file by hand or with mp config set --local", err)
	}
	after.applyDefaults()
	if !reflect.DeepEqual(after, want) {
		return nil, nil, errors.New("config.yaml: the edit would change more than the requested keys; change the file by hand or with mp config set --local")
	}
	return out, changed, nil
}

// WriteFileKeys sets keys (EditFileKeys) in the configuration file at path as
// root does it: the rest of the file stays as it was, the file keeps its mode
// and owner and is replaced atomically. A file that does not exist yet is
// made root-owned with group gid (-1: root's) and mode 0640, as mp setup
// makes it — the unprivileged API reads it. A symlink is refused.
func WriteFileKeys(path string, set map[string]string, gid int) (changed []string, err error) {
	var old []byte
	mode, uid := os.FileMode(0o640), -1
	st, err := os.Lstat(path)
	switch {
	case err == nil:
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file (a symlink?): change it by hand", path)
		}
		if old, err = os.ReadFile(path); err != nil {
			return nil, err
		}
		mode = st.Mode().Perm()
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(sys.Uid), int(sys.Gid)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	out, changed, err := EditFileKeys(old, set)
	if err != nil || len(changed) == 0 {
		return changed, err
	}
	if err := writeAtomic(path, out, mode, uid, gid); err != nil {
		return nil, err
	}
	return changed, nil
}

func writeAtomic(path string, data []byte, mode os.FileMode, uid, gid int) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".mp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Chmod(mode); err != nil {
		return err
	}
	if uid >= 0 || gid >= 0 {
		if err = tmp.Chown(uid, gid); err != nil {
			return err
		}
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// editYAML sets keys (section.name) in the YAML document, leaving every other
// node alone.
func editYAML(data []byte, values map[string]string, keys []string) ([]byte, error) {
	file, err := parser.ParseBytes(data, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("config.yaml does not parse: %w", err)
	}
	if len(file.Docs) > 1 {
		return nil, errors.New("config.yaml holds more than one YAML document; change the file by hand or with mp config set --local")
	}
	var body ast.Node
	if len(file.Docs) == 1 {
		body = file.Docs[0].Body
	}
	if _, ok := body.(*ast.MappingNode); !ok {
		if body != nil && body.Type() != ast.CommentType {
			return nil, errors.New("config.yaml is not a mapping of settings; change the file by hand or with mp config set --local")
		}
		// An empty file, or comments only: the settings go after them.
		return appendSettings(data, values, keys), nil
	}
	for _, key := range keys {
		section, name, _ := strings.Cut(key, ".")
		scalar := yamlScalar(key, values[key])
		keyPath, err := yaml.PathString("$." + section + "." + name)
		if err != nil {
			return nil, err
		}
		if node, err := keyPath.FilterFile(file); err == nil {
			comment := node.GetComment()
			if err := keyPath.ReplaceWithReader(file, strings.NewReader(scalar)); err != nil {
				return nil, fmt.Errorf("config.yaml: cannot set %s: %w", key, err)
			}
			// A comment at the end of the line belongs to the value node the
			// replacement dropped; it was the administrator's note, keep it.
			if comment != nil {
				if repl, err := keyPath.FilterFile(file); err == nil {
					_ = repl.SetComment(comment)
				}
			}
			continue
		}
		sectionPath, err := yaml.PathString("$." + section)
		if err != nil {
			return nil, err
		}
		if node, err := sectionPath.FilterFile(file); err == nil {
			if _, ok := node.(*ast.MappingNode); !ok {
				return nil, fmt.Errorf("config.yaml: %s is not a mapping; change the file by hand or with mp config set --local", section)
			}
			if err := sectionPath.MergeFromReader(file, strings.NewReader(name+": "+scalar+"\n")); err != nil {
				return nil, fmt.Errorf("config.yaml: cannot add %s: %w", key, err)
			}
			continue
		}
		root, err := yaml.PathString("$")
		if err != nil {
			return nil, err
		}
		if err := root.MergeFromReader(file, strings.NewReader(section+":\n  "+name+": "+scalar+"\n")); err != nil {
			return nil, fmt.Errorf("config.yaml: cannot add %s: %w", key, err)
		}
	}
	out := file.String()
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return []byte(out), nil
}

// appendSettings writes the keys as new sections after data, which holds no
// settings at all (it is empty or only comments).
func appendSettings(data []byte, values map[string]string, keys []string) []byte {
	var b strings.Builder
	b.Write(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		b.WriteByte('\n')
	}
	for _, section := range []string{"web", "log", "jobs"} {
		opened := false
		for _, key := range keys {
			sec, name, _ := strings.Cut(key, ".")
			if sec != section {
				continue
			}
			if !opened {
				b.WriteString(section + ":\n")
				opened = true
			}
			b.WriteString("  " + name + ": " + yamlScalar(key, values[key]) + "\n")
		}
	}
	return []byte(b.String())
}

// yamlScalar writes a normalized value as a YAML scalar, quoted only where
// YAML needs it — the way Save writes the same value.
func yamlScalar(key, value string) string {
	if key == KeyJobsWorkers {
		return value
	}
	b, err := yaml.Marshal(value)
	if err != nil {
		return strconv.Quote(value)
	}
	return strings.TrimSuffix(string(b), "\n")
}
