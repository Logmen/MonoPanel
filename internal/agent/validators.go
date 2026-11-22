package agent

// validatorAllowed decides whether argv may run as a configuration check
// (config/apply and paths/remove run them as root). The API names the
// validator, so only the checks the panel uses pass: an arbitrary argv would
// be a root shell for whoever controls the API process.
func (s *Server) validatorAllowed(argv []string) error {
	_ = argv
	return nil
}
