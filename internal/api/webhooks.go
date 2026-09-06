package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/apitypes"
	"monopanel/internal/auth"
	"monopanel/internal/jobs"
	"monopanel/internal/store"
)

const settingWebhooks = "webhooks"

type webhooksOutput struct {
	Body []apitypes.Webhook
}

type webhookInput struct {
	Body apitypes.WebhookRequest
}

type webhookOutput struct {
	Status int
	Body   apitypes.Webhook
}

type webhookIDInput struct {
	ID string `path:"id"`
}

func (s *Server) loadWebhooks(ctx context.Context) []apitypes.Webhook {
	raw, err := s.db.GetSetting(ctx, settingWebhooks)
	var list []apitypes.Webhook
	if err == nil {
		_ = json.Unmarshal([]byte(raw), &list)
	}
	if list == nil {
		list = []apitypes.Webhook{}
	}
	return list
}

func (s *Server) saveWebhooks(ctx context.Context, list []apitypes.Webhook) error {
	b, _ := json.Marshal(list)
	return s.db.SetSetting(ctx, settingWebhooks, string(b))
}

func (s *Server) registerWebhooks() {
	huma.Register(s.api, huma.Operation{
		OperationID: "webhooks-list", Method: http.MethodGet, Path: "/webhooks", Summary: "Webhooks for job and certificate events", Tags: []string{"webhooks"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, _ *struct{}) (*webhooksOutput, error) {
		list := s.loadWebhooks(ctx)
		for i := range list {
			list[i].Secret = ""
		}
		return &webhooksOutput{Body: list}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "webhooks-create", Method: http.MethodPost, Path: "/webhooks", Summary: "Add a webhook (HMAC-SHA256 signed POST)", Tags: []string{"webhooks"}, Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *webhookInput) (*webhookOutput, error) {
		p := principalFrom(ctx)
		u, err := url.Parse(in.Body.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, huma.Error422UnprocessableEntity("url must be http(s)://host/path")
		}
		id, _ := auth.NewToken(6)
		secret := in.Body.Secret
		if secret == "" {
			secret, _ = auth.NewToken(24)
		}
		events := in.Body.Events
		if len(events) == 0 {
			events = []string{"*"}
		}
		hook := apitypes.Webhook{ID: id, URL: in.Body.URL, Secret: secret, Events: events, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
		list := append(s.loadWebhooks(ctx), hook)
		if err := s.saveWebhooks(ctx, list); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "webhook.create", Target: in.Body.URL, IP: requestInfo(ctx).IP})
		return &webhookOutput{Status: http.StatusCreated, Body: hook}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "webhooks-delete", Method: http.MethodDelete, Path: "/webhooks/{id}", Summary: "Remove a webhook", Tags: []string{"webhooks"}, Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *webhookIDInput) (*struct{}, error) {
		p := principalFrom(ctx)
		list := s.loadWebhooks(ctx)
		kept := list[:0]
		found := false
		for _, h := range list {
			if h.ID == in.ID {
				found = true
				continue
			}
			kept = append(kept, h)
		}
		if !found {
			return nil, huma.Error404NotFound("webhook not found")
		}
		if err := s.saveWebhooks(ctx, kept); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "webhook.delete", Target: in.ID, IP: requestInfo(ctx).IP})
		return nil, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "webhooks-test", Method: http.MethodPost, Path: "/webhooks/{id}/test", Summary: "Send a test event", Tags: []string{"webhooks"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, in *webhookIDInput) (*struct{}, error) {
		for _, h := range s.loadWebhooks(ctx) {
			if h.ID == in.ID {
				if err := deliverWebhook(ctx, h, "test", map[string]any{"message": "MonoPanel webhook test"}); err != nil {
					return nil, huma.Error502BadGateway(err.Error())
				}
				return nil, nil
			}
		}
		return nil, huma.Error404NotFound("webhook not found")
	})
}

func eventMatches(patterns []string, event string) bool {
	for _, p := range patterns {
		if p == "*" || p == event {
			return true
		}
		if strings.HasSuffix(p, ".*") && strings.HasPrefix(event, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}

func deliverWebhook(ctx context.Context, h apitypes.Webhook, event string, payload map[string]any) error {
	payload["event"] = event
	payload["time"] = time.Now().UTC().Format(time.RFC3339)
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, []byte(h.Secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*attempt) * 5 * time.Second)
		}
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		req, err := http.NewRequestWithContext(rctx, http.MethodPost, h.URL, bytes.NewReader(body))
		if err != nil {
			cancel()
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "MonoPanel-Webhook/1")
		req.Header.Set("X-MonoPanel-Event", event)
		req.Header.Set("X-MonoPanel-Signature", sig)
		res, err := http.DefaultClient.Do(req)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		res.Body.Close()
		if res.StatusCode < 300 {
			return nil
		}
		lastErr = errors.New("webhook returned " + res.Status)
		if res.StatusCode < 500 {
			return lastErr
		}
	}
	return lastErr
}

// webhookLoop forwards finished jobs to subscribed webhooks.
func (s *Server) webhookLoop(ctx context.Context) {
	ch, stop := s.jobs.Broker().Subscribe(0, 256)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-ch:
			if e.Type != "done" && e.Type != "failed" {
				continue
			}
			hooks := s.loadWebhooks(ctx)
			if len(hooks) == 0 {
				continue
			}
			job, err := s.db.GetJob(ctx, e.JobID)
			if err != nil {
				continue
			}
			job.Log = ""
			events := []string{"job." + e.Type, job.Type + "." + e.Type}
			for _, h := range hooks {
				for _, ev := range events {
					if eventMatches(h.Events, ev) {
						go func(h apitypes.Webhook, ev string, job *store.Job) {
							if err := deliverWebhook(context.Background(), h, ev, map[string]any{"job": job}); err != nil {
								s.log.Warn("webhook delivery failed", "url", h.URL, "event", ev, "err", err)
							}
						}(h, ev, job)
						break
					}
				}
			}
		}
	}
}

var _ = jobs.Event{}
