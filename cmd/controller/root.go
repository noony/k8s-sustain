package controller

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/noony/k8s-sustain/internal/config"
)

var rootCmd = &cobra.Command{
	Use:   "k8s-sustain",
	Short: "Kubernetes sustainability operator",
	Long:  "k8s-sustain is a Kubernetes operator for workload right-sizing and sustainability policies.",
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
}
