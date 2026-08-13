package rxtypes

// HealthResponse is the body for GET /health.
//
// Per Appendix A.1 of the spec, the Go port drops Python's
// python_version and python_packages fields and replaces them with
// go_version and go_packages. The rx-viewer frontend does not consume
// the dropped fields.
//
// os_info, system_resources, environment, and hooks use map types
// because their keys are environment-specific and not worth typing.
type HealthResponse struct {
	Status           string `json:"status"`
	RipgrepAvailable bool   `json:"ripgrep_available"`
	AppVersion       string `json:"app_version"`
	// ContractVersion is the HTTP wire contract this backend speaks,
	// MAJOR.MINOR. Both backends report the same value; a client that
	// does not know the major must refuse to run against it.
	ContractVersion string            `json:"contract_version" doc:"HTTP wire contract version, MAJOR.MINOR"`
	GoVersion       string            `json:"go_version"`
	OSInfo          map[string]string `json:"os_info"`
	SystemResources map[string]any    `json:"system_resources"`
	GoPackages      map[string]string `json:"go_packages"`
	Constants       map[string]any    `json:"constants"`
	Environment     map[string]string `json:"environment"`
	Hooks           map[string]any    `json:"hooks"`
	DocsURL         string            `json:"docs_url"`

	// SearchRoots is absent from the pre-Appendix shape but present in
	// the actual Python response (added after original spec draft).
	// Emitting it here keeps field order stable with the Python runtime.
	SearchRoots []string `json:"search_roots"`
}
