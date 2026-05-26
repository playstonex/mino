package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use: "sslcon",
	Long: `A CLI application that supports the OpenConnect SSL VPN protocol.
For more information, please visit https://github.com/tlslink/sslcon`,
	CompletionOptions: cobra.CompletionOptions{HiddenDefaultCmd: true},
	// Called before rootCmd.Execute() returns.
	Run: func(cmd *cobra.Command, args []string) { // This does not run for subcommands, help, or command errors.
		cmd.Help()
	},
}

func Execute() {
	rootCmd.SetHelpCommand(&cobra.Command{Hidden: true})
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
