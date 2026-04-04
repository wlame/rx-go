package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	rootCmd := &cobra.Command{
		Use:   "rx",
		Short: "rx — fast, parallel regex search over compressed and uncompressed files",
		RunE: func(cmd *cobra.Command, args []string) error {
			v, _ := cmd.Flags().GetBool("version")
			if v {
				fmt.Println("rx", version)
				return nil
			}
			return cmd.Help()
		},
	}

	rootCmd.Flags().Bool("version", false, "print version and exit")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
