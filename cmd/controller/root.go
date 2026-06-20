package controller

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/noony/k8s-sustain/internal/config"
	"github.com/noony/k8s-sustain/internal/version"
)

var rootCmd = &cobra.Command{
	Use:   "k8s-sustain",
	Short: "Kubernetes sustainability operator",
	Long:  "k8s-sustain is a Kubernetes operator for workload right-sizing and sustainability policies.",
	// Version enables the built-in `--version` flag.
	Version: version.Version,
	// Errors from RunE are printed once by Execute() below; cobra would also
	// print the usage wall on every error which is noise for a daemon.
	SilenceUsage:  true,
	SilenceErrors: true,
}

// versionCmd prints the build-time version. Handy for confirming which image is
// running in-cluster (`kubectl exec ... -- /k8s-sustain version`).
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the k8s-sustain version",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), version.Version)
		return err
	},
}

// Execute is the package entry point called from main.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// RootCmd returns the root cobra command so other packages can register subcommands.
func RootCmd() *cobra.Command { return rootCmd }

func init() {
	cobra.OnInitialize(config.InitViper)
	config.BindGlobalFlags(rootCmd)
	rootCmd.AddCommand(versionCmd)
}
