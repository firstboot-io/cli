//go:build windows

package cli

import (
	"os"
	"os/exec"
)

// replaceProcess runs the program as a child, because Windows has no exec that
// replaces the running image. The exit code is propagated so a script sees what
// ssh said; the terminal quirks that exec avoids elsewhere are the price.
func replaceProcess(bin string, argv, env []string) error {
	cmd := exec.Command(bin, argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errorsAs(err, &ee) {
			return failf(ee.ExitCode(), "", "%s exited with status %d", bin, ee.ExitCode())
		}
		return fail(ExitError, "", err)
	}
	return nil
}
