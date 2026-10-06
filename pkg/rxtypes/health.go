package rxtypes

// HealthResponse is the body for GET /health.
//
// The Go backend drops Python's python_version and python_packages
// fields and replaces them with go_version and go_packages. The
// rx-viewer frontend does not consume the dropped fields.
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
	ContractVersion string `json:"contract_version" doc:"HTTP wire contract version, MAJOR.MINOR"`
	// Features names what this build serves, sorted: a client shows a
	// control that needs one only when its name is listed.
	Features        []string          `json:"features" nullable:"false" doc:"The names of the features this build serves, sorted. A client checks whether a name is listed before it uses the feature; the API documentation lists the names."`
	GoVersion       string            `json:"go_version"`
	OSInfo          map[string]string `json:"os_info"`
	SystemResources map[string]any    `json:"system_resources"`
	GoPackages      map[string]string `json:"go_packages"`
	Constants       map[string]any    `json:"constants"`
	Environment     map[string]string `json:"environment"`
	Hooks           map[string]any    `json:"hooks"`
	DocsURL         string            `json:"docs_url"`

	// SearchRoots mirrors search_roots in the Python response. Emitting
	// it here keeps field order stable with the Python runtime.
	SearchRoots []string `json:"search_roots"`
}
