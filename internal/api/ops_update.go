package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/buildinfo"
	"monopanel/internal/jobs"
	"monopanel/internal/store"
	"monopanel/internal/updater"
)

const settingUpdate = "update"

// defaultCheckHours is how often the panel asks the repository for a new
// release when nothing else is configured.
const defaultCheckHours = 24

// updateConfig is the stored half of the update settings: where to look, how
// often, and the access token (encrypted, like every other secret).
type updateConfig struct {
	Repo       string     `json:"repo,omitempty"`
	API        string     `json:"api,omitempty"`
	Channel    string     `json:"channel,omitempty"`
	TokenEnc   string     `json:"token_enc,omitempty"`
	CheckHours *int       `json:"check_hours,omitempty"`
	AutoApply  bool       `json:"auto_apply,omitempty"`
	CheckedAt  *time.Time `json:"checked_at,omitempty"`
	LastError  string     `json:"last_error,omitempty"`
	// Latest caches the release found by the last check so the status page
	// does not depend on the repository being reachable.
	Latest *updater.Release `json:"latest,omitempty"`
}

func (c updateConfig) channel() string {
	if c.Channel == updater.ChannelBeta {
		return updater.ChannelBeta
	}
	return updater.ChannelStable
}

func (c updateConfig) hours() int {
	if c.CheckHours == nil {
		return defaultCheckHours
	}
	return *c.CheckHours
}

func (s *Server) loadUpdateConfig(ctx context.Context) updateConfig {
	var c updateConfig
	if raw, err := s.db.GetSetting(ctx, settingUpdate); err == nil {
		_ = json.Unmarshal([]byte(raw), &c)
	}
	return c
}

func (s *Server) saveUpdateConfig(ctx context.Context, c updateConfig) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.db.SetSetting(ctx, settingUpdate, string(b))
}

// updateClient builds a release client from the stored settings.
func (s *Server) updateClient(c updateConfig) (*updater.Client, error) {
	if c.Repo == "" {
		return nil, huma.Error422UnprocessableEntity("укажите репозиторий с релизами: mp update settings --repo owner/name")
	}
	cl := &updater.Client{Repo: c.Repo, API: c.API}
	if c.TokenEnc != "" {
		if s.secrets == nil {
			return nil, huma.Error500InternalServerError("ключ шифрования недоступен, токен репозитория прочитать нельзя")
		}
		tok, err := s.secrets.Decrypt(c.TokenEnc)
		if err != nil {
			return nil, huma.Error500InternalServerError("не удалось расшифровать токен репозитория")
		}
		cl.Token = tok
	}
	return cl, nil
}

func (s *Server) updateStatus(ctx context.Context, c updateConfig) apitypes.UpdateStatus {
	out := apitypes.UpdateStatus{
		Current:   buildinfo.Version,
		KeyPinned: strings.TrimSpace(s.cfg.Update.PublicKey) != "",
		CheckedAt: c.CheckedAt,
		LastError: c.LastError,
		Settings: apitypes.UpdateSettings{
			Repo: c.Repo, API: c.API, Channel: c.channel(), HasToken: c.TokenEnc != "",
			CheckHours: c.hours(), AutoApply: c.AutoApply,
		},
	}
	if c.Latest != nil {
		out.Latest, out.Tag, out.Notes = c.Latest.Version, c.Latest.Tag, c.Latest.Notes
		if !c.Latest.PublishedAt.IsZero() {
			t := c.Latest.PublishedAt
			out.PublishedAt = &t
		}
		out.Available = updater.Newer(buildinfo.Version, c.Latest.Version)
	}
	if st, err := updater.ReadState(s.cfg.UpdatesDir()); err == nil && st != nil {
		a := &apitypes.UpdateAttempt{Status: st.Status, From: st.From, To: st.To, Error: st.Error, Log: st.Log}
		if !st.Started.IsZero() {
			t := st.Started
			a.Started = &t
		}
		if !st.Finished.IsZero() {
			t := st.Finished
			a.Finished = &t
		}
		out.LastAttempt = a
	}
	_ = ctx
	return out
}

// check asks the repository for the newest release and caches the answer.
func (s *Server) checkForUpdate(ctx context.Context, c updateConfig) (updateConfig, *updater.Release, error) {
	cl, err := s.updateClient(c)
	if err != nil {
		return c, nil, err
	}
	now := time.Now()
	rel, err := cl.Latest(ctx, c.channel())
	c.CheckedAt = &now
	if err != nil {
		c.LastError = err.Error()
		if serr := s.saveUpdateConfig(ctx, c); serr != nil {
			s.log.Error("save update settings", "err", serr)
		}
		return c, nil, err
	}
	c.LastError, c.Latest = "", rel
	if serr := s.saveUpdateConfig(ctx, c); serr != nil {
		s.log.Error("save update settings", "err", serr)
	}
	return c, rel, nil
}

type updateStatusOutput struct {
	Body apitypes.UpdateStatus
}

type updateSettingsInput struct {
	Body apitypes.UpdateSettingsRequest
}

type updateApplyInput struct {
	Body apitypes.UpdateApplyRequest
}

func (s *Server) registerUpdate() {
	huma.Register(s.api, huma.Operation{
		OperationID: "update-status", Method: http.MethodGet, Path: "/system/update", Summary: "Panel version and the update found by the last check", Tags: []string{"system"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, _ *struct{}) (*updateStatusOutput, error) {
		return &updateStatusOutput{Body: s.updateStatus(ctx, s.loadUpdateConfig(ctx))}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "update-settings", Method: http.MethodPut, Path: "/system/update", Summary: "Where the panel looks for new versions", Tags: []string{"system"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *updateSettingsInput) (*updateStatusOutput, error) {
		p := principalFrom(ctx)
		c := s.loadUpdateConfig(ctx)
		if r := strings.TrimSpace(in.Body.Repo); r == "-" {
			c.Repo, c.Latest, c.CheckedAt, c.LastError = "", nil, nil, ""
		} else if r != "" {
			r = strings.TrimSuffix(strings.TrimPrefix(r, "https://github.com/"), ".git")
			if !updater.ValidRepo(r) {
				return nil, huma.Error422UnprocessableEntity("репозиторий указывается как owner/name")
			}
			if r != c.Repo {
				c.Latest, c.CheckedAt, c.LastError = nil, nil, ""
			}
			c.Repo = r
		}
		switch a := strings.TrimSpace(in.Body.API); {
		case a == "-":
			c.API = ""
		case a != "":
			u, err := url.Parse(a)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return nil, huma.Error422UnprocessableEntity("адрес API указывается как https://host/api/v3")
			}
			c.API = strings.TrimSuffix(a, "/")
		}
		if in.Body.Channel != "" {
			c.Channel = in.Body.Channel
		}
		switch {
		case in.Body.ClearToken:
			c.TokenEnc = ""
		case in.Body.Token != "":
			if s.secrets == nil {
				return nil, huma.Error500InternalServerError("ключ шифрования недоступен, токен сохранить нельзя")
			}
			enc, err := s.secrets.Encrypt(strings.TrimSpace(in.Body.Token))
			if err != nil {
				return nil, huma.Error500InternalServerError("не удалось зашифровать токен")
			}
			c.TokenEnc = enc
		}
		if in.Body.CheckHours != nil {
			h := *in.Body.CheckHours
			c.CheckHours = &h
		}
		if in.Body.AutoApply != nil {
			c.AutoApply = *in.Body.AutoApply
		}
		if err := s.saveUpdateConfig(ctx, c); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "update.settings", Target: c.Repo, IP: requestInfo(ctx).IP,
			Details: map[string]any{"channel": c.channel(), "auto_apply": c.AutoApply, "check_hours": c.hours()}})
		return &updateStatusOutput{Body: s.updateStatus(ctx, c)}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "update-check", Method: http.MethodPost, Path: "/system/update/check", Summary: "Ask the repository for a new release now", Tags: []string{"system"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, _ *struct{}) (*updateStatusOutput, error) {
		c, _, err := s.checkForUpdate(ctx, s.loadUpdateConfig(ctx))
		if err != nil {
			var he huma.StatusError
			if errors.As(err, &he) {
				return nil, err
			}
			return nil, huma.Error502BadGateway(err.Error())
		}
		return &updateStatusOutput{Body: s.updateStatus(ctx, c)}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "update-apply", Method: http.MethodPost, Path: "/system/update/apply", Summary: "Install a new version (async; the panel restarts itself)", Tags: []string{"system"}, Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *updateApplyInput) (*jobRefOutput, error) {
		p := principalFrom(ctx)
		c := s.loadUpdateConfig(ctx)
		if c.Repo == "" {
			return nil, huma.Error422UnprocessableEntity("укажите репозиторий с релизами")
		}
		job, err := s.jobs.Enqueue(ctx, "panel.update", panelUpdatePayload{Version: in.Body.Version},
			jobs.WithLockKey("panel:update"), jobs.WithRequestedBy(p.Login))
		if err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "update.apply", Target: in.Body.Version, IP: requestInfo(ctx).IP})
		return &jobRefOutput{Status: http.StatusAccepted, Body: apitypes.JobRef{JobID: job.ID}}, nil
	})
}

type panelUpdatePayload struct {
	Version string `json:"version,omitempty"`
}

// jobPanelUpdate downloads a release, verifies it and hands it to the agent.
// The install itself happens outside this process: it restarts the panel, so
// the job is finished the moment systemd takes the work.
func (s *Server) jobPanelUpdate(ctx context.Context, jc *jobs.Context) error {
	var p panelUpdatePayload
	if err := jc.Unmarshal(&p); err != nil {
		return err
	}
	c := s.loadUpdateConfig(ctx)
	cl, err := s.updateClient(c)
	if err != nil {
		return err
	}

	jc.Progress(5, "поиск релиза")
	var rel *updater.Release
	if p.Version != "" {
		rel, err = cl.ByTag(ctx, p.Version)
	} else {
		rel, err = cl.Latest(ctx, c.channel())
	}
	if err != nil {
		return err
	}
	// A restart that already happened means the job is a duplicate: the panel
	// came back on the new version and the queue replayed the request.
	if !updater.Newer(buildinfo.Version, rel.Version) && p.Version == "" {
		jc.Logf("установлена версия %s, обновление не требуется", buildinfo.Version)
		return nil
	}
	jc.Logf("релиз %s от %s", rel.Tag, rel.PublishedAt.Local().Format("2006-01-02"))

	asset, err := updater.Select(rel, string(s.profile.Family()), runtime.GOARCH)
	if err != nil {
		return err
	}

	jc.Progress(20, "проверка подписи")
	sums, sig, err := s.releaseSums(ctx, cl, rel)
	if err != nil {
		return err
	}
	want := updater.ParseSums(sums)[asset.Name]
	if want == "" {
		return fmt.Errorf("%s не указан в %s релиза", asset.Name, updater.SumsFile)
	}

	jc.Progress(35, "загрузка "+asset.Name)
	local := filepath.Join(s.cfg.DownloadsDir(), asset.Name)
	prunePackages(s.cfg.DownloadsDir(), asset.Name)
	sum, err := cl.SaveTo(ctx, asset, local)
	if err != nil {
		return fmt.Errorf("скачать %s: %w", asset.Name, err)
	}
	if !strings.EqualFold(sum, want) {
		os.Remove(local)
		return fmt.Errorf("контрольная сумма %s не совпала с релизом", asset.Name)
	}
	jc.Logf("%s: %d КБ, sha256 %s…", asset.Name, asset.Size/1024, sum[:16])

	jc.Progress(70, "установка")
	res, err := s.agent.InstallPanel(ctx, &agent.InstallPanelRequest{
		Package: local, SHA256: sum, Version: rel.Version, Sums: string(sums), Sig: string(sig),
	})
	if err != nil {
		return err
	}
	if !res.Signed {
		jc.Logf("релиз принят без подписи: в config.yaml не задан update.public_key")
	}
	jc.Progress(100, "панель перезапускается")
	jc.Logf("установка %s запущена в %s; панель перезапустится через несколько секунд", rel.Version, res.Unit)
	return nil
}

// prunePackages drops panel packages left by earlier updates; each is around
// ten megabytes and only the one being installed is of any use.
func prunePackages(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == keep || !strings.HasPrefix(name, "monopanel") {
			continue
		}
		if strings.HasSuffix(name, ".deb") || strings.HasSuffix(name, ".rpm") {
			os.Remove(filepath.Join(dir, name))
		}
	}
}

// releaseSums fetches the checksum list and verifies its signature when the
// host has a release key. Without a key the list is still used, but then it
// only proves the download was not corrupted.
func (s *Server) releaseSums(ctx context.Context, cl *updater.Client, rel *updater.Release) (sums, sig []byte, err error) {
	sa, ok := rel.Asset(updater.SumsFile)
	if !ok {
		return nil, nil, fmt.Errorf("в релизе %s нет %s", rel.Tag, updater.SumsFile)
	}
	if sums, err = cl.Bytes(ctx, sa); err != nil {
		return nil, nil, err
	}
	ga, ok := rel.Asset(updater.SigFile)
	if ok {
		if sig, err = cl.Bytes(ctx, ga); err != nil {
			return nil, nil, err
		}
	}
	key := strings.TrimSpace(s.cfg.Update.PublicKey)
	if key == "" {
		return sums, sig, nil
	}
	if len(sig) == 0 {
		return nil, nil, fmt.Errorf("релиз %s не подписан, а на этом сервере задан ключ обновлений", rel.Tag)
	}
	if err := updater.VerifySums(key, sums, sig); err != nil {
		return nil, nil, fmt.Errorf("подпись релиза: %w", err)
	}
	return sums, sig, nil
}

// updateLoop checks for a new release on a schedule and, when the panel is
// allowed to, installs it.
func (s *Server) updateLoop(ctx context.Context) {
	s.reportUpdateOutcome(ctx)
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c := s.loadUpdateConfig(ctx)
		if c.Repo == "" || c.hours() <= 0 {
			continue
		}
		if c.CheckedAt != nil && time.Since(*c.CheckedAt) < time.Duration(c.hours())*time.Hour {
			continue
		}
		c, rel, err := s.checkForUpdate(ctx, c)
		if err != nil {
			s.log.Warn("update check", "err", err)
			continue
		}
		if !updater.Newer(buildinfo.Version, rel.Version) {
			continue
		}
		s.log.Info("new panel version available", "current", buildinfo.Version, "latest", rel.Version, "auto", c.AutoApply)
		if !c.AutoApply {
			continue
		}
		if _, err := s.jobs.Enqueue(ctx, "panel.update", panelUpdatePayload{},
			jobs.WithLockKey("panel:update"), jobs.WithRequestedBy("scheduler")); err != nil {
			s.log.Error("enqueue update", "err", err)
		}
	}
}

// reportUpdateOutcome records how the update that restarted us went. The job
// that started it died with the old process, so this is the only place the
// result is ever written to the audit log.
func (s *Server) reportUpdateOutcome(ctx context.Context) {
	st, err := updater.ReadState(s.cfg.UpdatesDir())
	if err != nil || st == nil || st.Status == updater.StatusInstalling {
		return
	}
	seen, _ := s.db.GetSetting(ctx, "update.reported")
	stamp := st.Status + "@" + st.Finished.Format(time.RFC3339)
	if seen == stamp {
		return
	}
	result := "ok"
	if st.Status != updater.StatusDone {
		result = "error"
		s.log.Error("previous panel update failed", "status", st.Status, "err", st.Error)
	} else {
		s.log.Info("panel updated", "from", st.From, "to", st.To)
	}
	s.db.Audit(ctx, store.AuditEntry{Actor: "system", Action: "update." + st.Status, Target: st.To, Result: result,
		Details: map[string]any{"from": st.From, "error": st.Error}})
	s.db.SetSetting(ctx, "update.reported", stamp)
}
