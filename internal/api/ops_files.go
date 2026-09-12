package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"monopanel/internal/agent"
	"monopanel/internal/apitypes"
	"monopanel/internal/store"
)

type filesListInput struct {
	User string `query:"user" doc:"owner login (admins; users see their own home)"`
	Path string `query:"path" default:"/"`
}

type filesListOutput struct {
	Body apitypes.FileList
}

type filesOpInput struct {
	Body apitypes.FileOpRequest
}

type filesOpOutput struct {
	Body apitypes.FileOpResult
}

type fileContentInput struct {
	User string `query:"user"`
	Path string `query:"path" minLength:"1"`
}

type fileContentOutput struct {
	ContentType        string `header:"Content-Type"`
	ContentDisposition string `header:"Content-Disposition"`
	Body               []byte
}

type fileUploadInput struct {
	User    string `query:"user"`
	Path    string `query:"path" minLength:"1"`
	RawBody []byte
}

// filesOwner resolves which client's home an operation targets.
func (s *Server) filesOwner(ctx context.Context, login string) (*store.User, error) {
	p := principalFrom(ctx)
	if p.Role != store.RoleAdmin {
		if login != "" && login != p.Login {
			return nil, huma.Error403Forbidden("not your account")
		}
		login = p.Login
	}
	if login == "" {
		return nil, huma.Error422UnprocessableEntity("user is required")
	}
	u, err := s.db.GetUserByLogin(ctx, login)
	if errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error404NotFound("user not found")
	}
	if err != nil {
		return nil, err
	}
	if u.UnixUID == nil {
		return nil, huma.Error422UnprocessableEntity("user has no unix account")
	}
	return u, nil
}

func (s *Server) fsop(ctx context.Context, u *store.User, stdin []byte, args ...string) ([]byte, error) {
	req := &agent.RunAsUserRequest{Login: u.Login, Args: args}
	if len(stdin) > 0 {
		req.StdinBase64 = base64.StdEncoding.EncodeToString(stdin)
	}
	res, err := s.agent.RunAsUser(ctx, req)
	if err != nil {
		return nil, huma.Error502BadGateway(err.Error())
	}
	out, _ := base64.StdEncoding.DecodeString(res.StdoutBase64)
	if res.ExitCode != 0 {
		msg := res.Stderr
		for _, pre := range []string{"Error: ", "error: "} {
			if i := strings.LastIndex(msg, pre); i >= 0 {
				msg = msg[i+len(pre):]
			}
		}
		return nil, huma.Error422UnprocessableEntity(strings.TrimSpace(msg))
	}
	return out, nil
}

func (s *Server) registerFiles() {
	huma.Register(s.api, huma.Operation{
		OperationID: "files-list", Method: http.MethodGet, Path: "/files", Summary: "List a directory in a client's home (runs as the client)", Tags: []string{"files"}, Security: secured,
	}, func(ctx context.Context, in *filesListInput) (*filesListOutput, error) {
		u, err := s.filesOwner(ctx, in.User)
		if err != nil {
			return nil, err
		}
		out, err := s.fsop(ctx, u, nil, "list", in.Path)
		if err != nil {
			return nil, err
		}
		var entries []apitypes.FileEntry
		if err := json.Unmarshal(out, &entries); err != nil {
			return nil, err
		}
		if entries == nil {
			entries = []apitypes.FileEntry{}
		}
		return &filesListOutput{Body: apitypes.FileList{User: u.Login, Home: u.Home, Path: in.Path, Entries: entries}}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "files-op", Method: http.MethodPost, Path: "/files/op", Summary: "mkdir, rm, mv, chmod, extract or size", Tags: []string{"files"}, Security: secured,
	}, func(ctx context.Context, in *filesOpInput) (*filesOpOutput, error) {
		u, err := s.filesOwner(ctx, in.Body.User)
		if err != nil {
			return nil, err
		}
		var args []string
		switch in.Body.Op {
		case "mkdir", "size":
			args = []string{in.Body.Op, in.Body.Path}
		case "touch":
			// Пустой файл: PUT с пустым телом huma не принимает. Создание идёт
			// через O_EXCL и не затирает существующий файл; force опустошает.
			if in.Body.Force {
				args = []string{"write", in.Body.Path}
			} else {
				args = []string{"touch", in.Body.Path}
			}
		case "rm":
			args = append([]string{"rm", in.Body.Path}, in.Body.Paths...)
		case "mv", "extract":
			if in.Body.Dest == "" {
				return nil, huma.Error422UnprocessableEntity("dest is required")
			}
			args = []string{in.Body.Op, in.Body.Path, in.Body.Dest}
		case "chmod":
			if in.Body.Mode == "" {
				return nil, huma.Error422UnprocessableEntity("mode is required")
			}
			args = []string{"chmod", in.Body.Mode, in.Body.Path}
		default:
			return nil, huma.Error422UnprocessableEntity("unknown op")
		}
		out, err := s.fsop(ctx, u, nil, args...)
		if err != nil {
			return nil, err
		}
		res := apitypes.FileOpResult{OK: true}
		if in.Body.Op == "size" || in.Body.Op == "extract" {
			res.Value, _ = strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		}
		if in.Body.Op == "extract" {
			// an archive may carry SELinux labels from another host
			s.relabel(ctx, nil, path.Join(s.homeOf(u), in.Body.Dest), true)
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: principalFrom(ctx).Login, Action: "files." + in.Body.Op, Target: u.Login + ":" + in.Body.Path, IP: requestInfo(ctx).IP})
		return &filesOpOutput{Body: res}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "files-download", Method: http.MethodGet, Path: "/files/content", Summary: "Download a file", Tags: []string{"files"}, Security: secured,
	}, func(ctx context.Context, in *fileContentInput) (*fileContentOutput, error) {
		u, err := s.filesOwner(ctx, in.User)
		if err != nil {
			return nil, err
		}
		out, err := s.fsop(ctx, u, nil, "read", in.Path)
		if err != nil {
			return nil, err
		}
		return &fileContentOutput{ContentType: "application/octet-stream", ContentDisposition: fmt.Sprintf("attachment; filename=%q", path.Base(in.Path)), Body: out}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "files-upload", Method: http.MethodPut, Path: "/files/content", Summary: "Upload a file (raw body)", Tags: []string{"files"}, Security: secured, MaxBodyBytes: 512 << 20,
	}, func(ctx context.Context, in *fileUploadInput) (*filesOpOutput, error) {
		u, err := s.filesOwner(ctx, in.User)
		if err != nil {
			return nil, err
		}
		if _, err := s.fsop(ctx, u, in.RawBody, "write", in.Path); err != nil {
			return nil, err
		}
		s.db.Audit(ctx, store.AuditEntry{Actor: principalFrom(ctx).Login, Action: "files.upload", Target: u.Login + ":" + in.Path, IP: requestInfo(ctx).IP, Details: map[string]any{"bytes": len(in.RawBody)}})
		return &filesOpOutput{Body: apitypes.FileOpResult{OK: true, Value: int64(len(in.RawBody))}}, nil
	})
}
