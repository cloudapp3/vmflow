package service

// Summary is a compact, cross-platform native service status.
type Summary struct {
	Installed bool
	Running   bool
	Enabled   bool
	State     string
	Detail    string
}

// Inspect returns native service state without printing the platform service
// manager's full diagnostic output.
func Inspect(cfg Config) (Summary, error) {
	cfg = normalize(cfg)
	return platformInspect(cfg)
}
