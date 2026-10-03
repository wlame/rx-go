package clicommand

import (
	"github.com/spf13/cobra"

	"github.com/wlame/rx-go/internal/config"
)

// maskSecretsFlag is the flag of `rx trace` and `rx serve` that turns
// masking of secrets on: the signatures with `redact: true` then mask
// what they match in the text of every finding and in the findings
// cache. Masking is off by default.
const maskSecretsFlag = "mask-secrets"

// bindMaskSecretsFlag declares --mask-secrets on cmd. Its default is a
// plain false, whatever RX_MASK_SECRETS holds: maskSecretsMode reads the
// variable when the flag is not given, and the table of equivalent
// commands (webapi.CLICommandOperations) takes "false" as the value rx
// uses without the flag.
func bindMaskSecretsFlag(cmd *cobra.Command) {
	cmd.Flags().Bool(maskSecretsFlag, false,
		"Mask what the signatures with redact: true find, in the text of every finding and in the findings cache "+
			"(default: RX_MASK_SECRETS, else off)")
}

// maskSecretsMode is the masking mode of a run of cmd: the value of
// --mask-secrets when the command line gives it (so --mask-secrets=false
// turns masking off even with RX_MASK_SECRETS=1), otherwise
// RX_MASK_SECRETS (config.MaskSecrets). cmd must have the flag
// (bindMaskSecretsFlag).
func maskSecretsMode(cmd *cobra.Command) bool {
	// Go note: Changed tells a flag the user wrote from one left at its
	// default, which the value alone cannot: an absent --mask-secrets
	// and --mask-secrets=false both read false.
	if cmd.Flags().Changed(maskSecretsFlag) {
		// GetBool fails only for a flag that is not a registered bool;
		// bindMaskSecretsFlag registered it, so the error is nil here.
		on, _ := cmd.Flags().GetBool(maskSecretsFlag)
		return on
	}
	return config.MaskSecrets()
}
