//go:build !windows

package cli

import "syscall"

// replaceProcess hands the terminal to another program.
//
// exec rather than fork: from the moment ssh starts, it owns the terminal, and
// a parent process sitting in between breaks window resizing, Ctrl-C, and the
// exit code a script downstream is reading. There is nothing for this CLI to do
// after the handover, so there is no reason to stay alive for it.
func replaceProcess(bin string, argv, env []string) error {
	return syscall.Exec(bin, argv, env)
}
