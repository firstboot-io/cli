// Command firstboot manages Firstboot cloud resources from the terminal.
//
// It exists for the three things a panel cannot do: act on many resources in one
// command, hand its output to a program with --output json, and stay inside the
// terminal (`firstboot server ssh web-1` resolves a name and connects).
//
// A profile IS a token IS an organization. An API token is pinned to one
// organization for its whole life, so there is no --org flag: two organizations
// means two profiles.
package main

import (
	"os"

	"github.com/firstboot-io/firstboot-cli/internal/cli"
)

// version is stamped by the release build.
var version = "dev"

func main() {
	os.Exit(cli.Execute(version))
}
