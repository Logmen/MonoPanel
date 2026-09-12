package api

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"

	"monopanel/internal/jobs"
)

// 1C-Bitrix has no command-line installer: its web wizard is driven here the
// way a browser would, step by step, straight through the site's own vhost.
// Regular steps get the field overrides and "Next"; the AJAX steps (module
// installation, updates, demo data) are looped by following the Post({...})
// hints the wizard returns in each [response]. Everything the wizard asks
// for is known up front, so no step is left to guess at.

var (
	bxInputRe    = regexp.MustCompile(`(?is)<input\b([^>]*)>`)
	bxAttrRe     = regexp.MustCompile(`(\w+)\s*=\s*"([^"]*)"`)
	bxFormRe     = regexp.MustCompile(`(?is)<form\b[^>]*\baction="([^"]*)"`)
	bxAjaxMapRe  = regexp.MustCompile(`(?s)new CAjaxForm\([^{]*\{(.*?)\}`)
	bxPairRe     = regexp.MustCompile(`"(\w+)"\s*:\s*"([^"]+)"`)
	bxErrorRe    = regexp.MustCompile(`(?s)id="error_text"[^>]*>\s*(\S.*?)</div>`)
	bxPostMapRe  = regexp.MustCompile(`(?s)Post\(\s*\{(.*?)\}`)
	bxPostArgRe  = regexp.MustCompile(`'(\w+)'\s*:\s*'?([^',}\s]*)'?`)
	bxPostPosRe  = regexp.MustCompile(`Post\(\s*'([^']*)'\s*,\s*'([^']*)'`)
	bxSolutionRe = regexp.MustCompile(`SelectSolution\(this,\s*'([^'@]+)'\)`)
	bxScriptRe   = regexp.MustCompile(`(?is)<script.*?</script>`)
	bxTagRe      = regexp.MustCompile(`<[^>]+>`)
	bxSpaceRe    = regexp.MustCompile(`\s+`)
)

// bitrixWizard walks the installer at base (the site's address), resolving
// the site's name to ip so DNS is not needed.
type bitrixWizard struct {
	base      string
	overrides map[string]string
	client    *http.Client
	jc        *jobs.Context
	log       []string
}

func newBitrixWizard(base, domain, ip string, overrides map[string]string, jc *jobs.Context) *bitrixWizard {
	jar, _ := cookiejar.New(nil)
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	tr := &http.Transport{
		// the site's name goes to the site's own address, whatever DNS says
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if host, port, err := net.SplitHostPort(addr); err == nil && strings.EqualFold(host, domain) && ip != "" {
				addr = net.JoinHostPort(ip, port)
			}
			return dialer.DialContext(ctx, network, addr)
		},
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // a hop to our own nginx; the placeholder certificate is self-signed
		ResponseHeaderTimeout: 15 * time.Minute,
	}
	return &bitrixWizard{base: strings.TrimSuffix(base, "/"), overrides: overrides, jc: jc, client: &http.Client{Jar: jar, Transport: tr, Timeout: 20 * time.Minute}}
}

type bxField struct {
	typ, name, value string
	checked          bool
}

type bxPage struct {
	url     string
	html    string
	fields  []bxField
	action  string
	ajax    bool
	ajaxMap map[string]string
	step    string
}

func bxParse(pageURL, body string) *bxPage {
	p := &bxPage{url: pageURL, html: body, ajaxMap: map[string]string{}}
	for _, m := range bxInputRe.FindAllStringSubmatch(body, -1) {
		f := bxField{}
		for _, a := range bxAttrRe.FindAllStringSubmatch(m[1], -1) {
			switch strings.ToLower(a[1]) {
			case "type":
				f.typ = strings.ToLower(a[2])
			case "name":
				f.name = a[2]
			case "value":
				f.value = html.UnescapeString(a[2])
			}
		}
		f.checked = regexp.MustCompile(`(?i)\bchecked\b`).MatchString(m[1])
		if f.name == "" {
			continue
		}
		if f.name == "CurrentStepID" {
			p.step = f.value
		}
		p.fields = append(p.fields, f)
	}
	if m := bxFormRe.FindStringSubmatch(body); m != nil {
		p.action = html.UnescapeString(m[1])
	}
	if m := bxAjaxMapRe.FindStringSubmatch(body); m != nil {
		p.ajax = true
		for _, kv := range bxPairRe.FindAllStringSubmatch(m[1], -1) {
			p.ajaxMap[kv[1]] = kv[2]
		}
	}
	return p
}

// text is the page's visible words, for logs and errors.
func (p *bxPage) text() string {
	t := bxScriptRe.ReplaceAllString(p.html, " ")
	t = bxTagRe.ReplaceAllString(t, " ")
	return strings.TrimSpace(bxSpaceRe.ReplaceAllString(html.UnescapeString(t), " "))
}

// form is what a browser would submit: hidden and text values, checked
// choices, the overrides for the fields the page has, the first solution
// on the solution step.
func (p *bxPage) form(overrides map[string]string) url.Values {
	v := url.Values{}
	names := map[string]bool{}
	for _, f := range p.fields {
		names[f.name] = true
		switch f.typ {
		case "submit", "button", "image", "file":
			continue
		case "checkbox", "radio":
			if !f.checked {
				continue
			}
		}
		v.Set(f.name, f.value)
	}
	for k, val := range overrides {
		if names[k] {
			v.Set(k, val)
		}
	}
	if names["__wiz_selected_wizard"] && v.Get("__wiz_selected_wizard") == "" {
		if m := bxSolutionRe.FindStringSubmatch(p.html); m != nil {
			v.Set("__wiz_selected_wizard", m[1])
		}
	}
	return v
}

func (w *bitrixWizard) get(ctx context.Context, u string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "MonoPanel")
	res, err := w.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	return res.Request.URL.String(), string(b), err
}

func (w *bitrixWizard) post(ctx context.Context, u string, form url.Values) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "MonoPanel")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := w.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	return res.Request.URL.String(), string(b), err
}

func (w *bitrixWizard) logf(format string, args ...any) {
	if w.jc != nil {
		w.jc.Logf(format, args...)
	}
	w.log = append(w.log, fmt.Sprintf(format, args...))
}

// run walks the wizard to its end: the site answering with no wizard form.
func (w *bitrixWizard) run(ctx context.Context) error {
	pageURL, body, err := w.get(ctx, w.base+"/")
	if err != nil {
		return fmt.Errorf("wizard: %w", err)
	}
	last, same := "", 0
	for n := 0; n < 300; n++ {
		p := bxParse(pageURL, body)
		if p.action == "" || p.step == "" {
			if strings.Contains(body, "[response]") {
				return errors.New("wizard: lost its page after an AJAX step")
			}
			w.logf("wizard finished: %s", pageURL)
			return nil
		}
		if p.step != last {
			w.logf("wizard step: %s", p.step)
			last, same = p.step, 0
		} else {
			same++
			if same > 25 {
				return fmt.Errorf("wizard is stuck at %s: %s", p.step, snippet(p.text(), 400))
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		action, err := url.Parse(p.action)
		if err != nil {
			return err
		}
		base, _ := url.Parse(pageURL)
		actionURL := base.ResolveReference(action).String()
		form := p.form(w.overrides)
		if p.ajax {
			pageURL, body, err = w.ajaxLoop(ctx, actionURL, form, p)
		} else {
			form.Set("StepNext", "Next")
			pageURL, body, err = w.post(ctx, actionURL, form)
		}
		if err != nil {
			return fmt.Errorf("wizard at %s: %w", p.step, err)
		}
	}
	return errors.New("wizard: too many steps")
}

// ajaxLoop repeats the step's request until the wizard stops asking for
// another stage. A page rendered in between is an error box (retried, then
// skipped the way the button does) or the next step.
func (w *bitrixWizard) ajaxLoop(ctx context.Context, actionURL string, form url.Values, p *bxPage) (string, string, error) {
	field := func(k string) string {
		if f, ok := p.ajaxMap[k]; ok {
			return f
		}
		return "__wiz_" + k
	}
	retries := 0
	for k := 0; k < 1000; k++ {
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		pageURL, resp, err := w.post(ctx, actionURL, form)
		if err != nil {
			return "", "", err
		}
		if strings.Contains(strings.ToLower(resp), "<html") {
			q := bxParse(pageURL, resp)
			if m := bxErrorRe.FindStringSubmatch(resp); m != nil && q.step == p.step {
				retries++
				msg := snippet(strings.TrimSpace(bxTagRe.ReplaceAllString(m[1], " ")), 200)
				if retries > 6 {
					return "", "", fmt.Errorf("step %s keeps failing: %s", p.step, msg)
				}
				w.logf("wizard step %s: %s (%s)", p.step, msg, map[bool]string{true: "retrying", false: "skipping"}[retries < 3])
				if retries >= 3 && form.Get(field("nextStep")) != "main" {
					form.Set(field("nextStepStage"), "skip")
				}
				continue
			}
			return pageURL, resp, nil
		}
		next := map[string]string{}
		if m := bxPostMapRe.FindStringSubmatch(resp); m != nil {
			for _, kv := range bxPostArgRe.FindAllStringSubmatch(m[1], -1) {
				next[kv[1]] = kv[2]
			}
		} else if m := bxPostPosRe.FindStringSubmatch(resp); m != nil {
			// the solution wizard passes the step positionally: Post(step, stage, status)
			next["nextStep"], next["nextStepStage"] = m[1], m[2]
		}
		changed := false
		for kk, vv := range next {
			if form.Get(field(kk)) != vv {
				form.Set(field(kk), vv)
				changed = true
			}
		}
		if changed {
			continue
		}
		if strings.Contains(resp, "submit()") || strings.Contains(resp, "StopAjax") || len(next) > 0 {
			// the stage is done: submit the form as the page would, without the AJAX markers
			form.Del(field("nextStep"))
			form.Del(field("nextStepStage"))
			return w.post(ctx, actionURL, form)
		}
		return "", "", fmt.Errorf("unexpected answer at %s: %s", p.step, snippet(bxSpaceRe.ReplaceAllString(resp, " "), 300))
	}
	return "", "", fmt.Errorf("step %s never finished", p.step)
}

func snippet(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
