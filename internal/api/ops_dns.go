package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/acme"
	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

type dnsProvidersOutput struct {
	Body []*store.DNSProvider
}

type dnsProviderInput struct {
	Body apitypes.DNSProviderRequest
}

type dnsProviderOutput struct {
	Status int
	Body   *store.DNSProvider
}

type dnsNameInput struct {
	Name string `path:"name"`
}

type dnsTypesOutput struct {
	Body map[string][]string
}

// dnsCredentials decrypts a provider's credentials.
func (s *Server) dnsCredentials(ctx context.Context, name string) (typ string, creds map[string]string, err error) {
	if s.secrets == nil {
		return "", nil, errors.New("secret key unavailable")
	}
	p, err := s.db.GetDNSProvider(ctx, name)
	if err != nil {
		return "", nil, err
	}
	raw, err := s.secrets.Decrypt(p.CredentialsEnc)
	if err != nil {
		return "", nil, err
	}
	if err := json.Unmarshal([]byte(raw), &creds); err != nil {
		return "", nil, err
	}
	return p.Type, creds, nil
}

func (s *Server) registerDNS() {
	huma.Register(s.api, huma.Operation{
		OperationID: "dns-provider-types", Method: http.MethodGet, Path: "/dns-providers/types", Summary: "Supported DNS providers and their credential keys", Tags: []string{"ssl"}, Security: secured,
	}, func(ctx context.Context, _ *struct{}) (*dnsTypesOutput, error) {
		return &dnsTypesOutput{Body: acme.DNSProviderTypes}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "dns-providers-list", Method: http.MethodGet, Path: "/dns-providers", Summary: "DNS providers for DNS-01 challenges", Tags: []string{"ssl"}, Security: secured, Metadata: adminOnly,
	}, func(ctx context.Context, _ *struct{}) (*dnsProvidersOutput, error) {
		list, err := s.db.ListDNSProviders(ctx)
		if err != nil {
			return nil, err
		}
		return &dnsProvidersOutput{Body: list}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "dns-providers-create", Method: http.MethodPost, Path: "/dns-providers", Summary: "Add a DNS provider (credentials encrypted at rest)", Tags: []string{"ssl"}, Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *dnsProviderInput) (*dnsProviderOutput, error) {
		p := principalFrom(ctx)
		if s.secrets == nil {
			return nil, huma.Error422UnprocessableEntity("secret key unavailable")
		}
		expected, ok := acme.DNSProviderTypes[in.Body.Type]
		if !ok {
			types := make([]string, 0, len(acme.DNSProviderTypes))
			for t := range acme.DNSProviderTypes {
				types = append(types, t)
			}
			sort.Strings(types)
			return nil, huma.Error422UnprocessableEntity("unsupported type; use one of: " + joinStrings(types))
		}
		for _, k := range expected {
			if in.Body.Credentials[k] == "" && k != "AWS_REGION" && k != "OVH_ENDPOINT" && k != "RFC2136_TSIG_ALGORITHM" {
				return nil, huma.Error422UnprocessableEntity("missing credential " + k)
			}
		}
		raw, _ := json.Marshal(in.Body.Credentials)
		enc, err := s.secrets.Encrypt(string(raw))
		if err != nil {
			return nil, err
		}
		prov := &store.DNSProvider{Name: in.Body.Name, Type: in.Body.Type, CredentialsEnc: enc}
		if err := s.db.CreateDNSProvider(ctx, prov); err != nil {
			if errors.Is(err, store.ErrExists) {
				return nil, huma.Error409Conflict("provider name already used")
			}
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "dns.provider.create", Target: prov.Name, IP: requestInfo(ctx).IP})
		return &dnsProviderOutput{Status: http.StatusCreated, Body: prov}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "dns-providers-delete", Method: http.MethodDelete, Path: "/dns-providers/{name}", Summary: "Remove a DNS provider", Tags: []string{"ssl"}, Security: secured, Metadata: adminOnly, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *dnsNameInput) (*struct{}, error) {
		p := principalFrom(ctx)
		prov, err := s.db.GetDNSProvider(ctx, in.Name)
		if errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error404NotFound("provider not found")
		}
		if err != nil {
			return nil, err
		}
		if err := s.db.DeleteDNSProvider(ctx, prov.ID); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: p.Login, Action: "dns.provider.delete", Target: prov.Name, IP: requestInfo(ctx).IP})
		return nil, nil
	})
}

func joinStrings(list []string) string {
	out := ""
	for i, s := range list {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
