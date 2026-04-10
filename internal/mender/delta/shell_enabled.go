//go:build shell_commands

package delta

func init() {
	// Enable shell commands when built with -tags shell_commands
	UseShellCommands = true
}
