package api

import "github.com/danielgtaylor/huma/v2"

// OpenAPI is the description of every registered operation. The parity check
// (internal/parity) walks it to prove that the web UI, the CLI and the TUI
// reach the same operations.
func (s *Server) OpenAPI() *huma.OpenAPI { return s.api.OpenAPI() }
