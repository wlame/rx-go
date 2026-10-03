package config

// MaskingEnv is the variable that turns masking of secrets on: the
// signatures with `redact: true` then mask what they match in the text
// of every finding and in the findings cache. `--mask-secrets` on
// `rx trace` and `rx serve` wins over it.
const MaskingEnv = "RX_MASK_SECRETS"

// MaskSecrets reports whether RX_MASK_SECRETS turns masking on. It is
// off by default: rx runs on a trusted network, where the text of a
// finding is shown as the file holds it. The variable takes the
// boolean forms of GetBoolEnv; any other value is refused as for
// RX_HIDDEN and RX_NO_INDEX, so masking stays off.
func MaskSecrets() bool {
	return GetBoolEnv(MaskingEnv, false)
}
