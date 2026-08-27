package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	firstboot "github.com/firstboot-io/go-sdk"
	"golang.org/x/term"
)

// The confirmation gate, and why the list of what needs one is not written here.
//
// The API already decided which operations cannot be fixed by pressing the
// button again: they are the ones that cost the `destroy` scope, and the
// platform publishes that decision on every endpoint as `x-firstboot-scope`. So
// this CLI COPIES that set rather than re-deciding it. A command declares the
// operation it calls; a test reads the platform's own spec and fails if a
// `destroy`-scoped operation is behind a command with no confirmation.
//
// The alternative -- a hand-written list of dangerous commands -- drifts the
// first time the API makes something irreversible that used to be reversible,
// and drifts silently, because nothing about a missing prompt looks broken.

// Danger is what a command declares about itself.
type Danger struct {
	// Operation is the API operationId whose scope decides whether a
	// confirmation is required. Empty means the command changes nothing.
	Operation string
	// Confirm is what the person has to type back. A resource's NAME, not "yes":
	// typing a name proves they read which one, and `yes` proves only that they
	// were in a hurry.
	Confirm string
	// What is the sentence above the prompt, in the imperative: what is about
	// to happen and what cannot be taken back.
	What string
	// Extra is the second paragraph when there is one worth saying: the data
	// that goes with the machine, the mail that stops being delivered.
	Extra string
}

// destroyScoped is the set of operations that cost `destroy`, copied from the
// platform's published table.
//
// It is a literal rather than a spec read at runtime, because a CLI must work
// with no network and no sibling checkout. `confirm_test.go` is what makes the
// copy safe: it reads `../platform/api/openapi/openapi.json` and fails if this
// set and the published scopes disagree in either direction.
var destroyScoped = map[string]bool{
	"appDelete":                 true,
	"appDomainDelete":           true,
	"appAttachmentDelete":       true,
	"serverDelete":              true,
	"serverRebuild":             true,
	"serverNetworkDetach":       true,
	"serverSnapshotDelete":      true,
	"serverSnapshotRestore":     true,
	"serverBackupDelete":        true,
	"serverBackupRestore":       true,
	"serverAlertDelete":         true,
	"databaseAlertDelete":       true,
	"volumeDelete":              true,
	"networkDelete":             true,
	"floatingIpDelete":          true,
	"loadBalancerDelete":        true,
	"firewallDelete":            true,
	"dnsZoneDelete":             true,
	"dnsRecordDelete":           true,
	"rdnsClear":                 true,
	"databaseDelete":            true,
	"databaseUserDelete":        true,
	"projectDelete":             true,
	"sshKeyDelete":              true,
	"isoDelete":                 true,
	"serverRescueEnter":         true,
	"serverRescueExit":          true,
	"serverPasswordReset":       true,
	"databaseUserPasswordReset": true,
}

// NeedsConfirmation answers whether an operation is one the API calls
// irreversible.
func NeedsConfirmation(operation string) bool { return destroyScoped[operation] }

// Confirm stops and asks, unless --yes was given.
//
// A non-interactive run without --yes is REFUSED rather than assumed. A script
// that pipes nothing to stdin and did not say --yes has not consented to a
// delete, and treating silence as consent is how a CI job removes a production
// machine.
func (e *Env) Confirm(d Danger) error {
	if d.Operation != "" && !NeedsConfirmation(d.Operation) {
		return nil
	}
	p := e.Printer

	if e.Yes {
		return nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return failf(ExitUsage,
			"Pass --yes to say so explicitly. This CLI does not treat a closed stdin as "+
				"consent to something it cannot undo.",
			"%s, and this is not an interactive terminal", d.What)
	}

	fmt.Fprintln(p.Err)
	fmt.Fprintln(p.Err, p.Warn("This cannot be undone."))
	fmt.Fprintln(p.Err, d.What)
	if d.Extra != "" {
		fmt.Fprintln(p.Err, d.Extra)
	}
	fmt.Fprintf(p.Err, "\nType %s to confirm: ", p.Warn(d.Confirm))

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return failf(ExitCancelled, "", "cancelled")
	}
	if strings.TrimSpace(line) != d.Confirm {
		return failf(ExitCancelled, "", "that did not match %q; nothing was done", d.Confirm)
	}
	return nil
}

// signalContext cancels on Ctrl-C and on SIGTERM.
//
// Honouring it matters more here than in most CLIs: an interrupted create has
// already been SENT, so the right behaviour is to stop waiting and say the
// resource exists, not to pretend nothing happened. The commands that create
// print what they know before they start waiting for exactly that reason.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// The SDK's state classification, wrapped so output.go can colour a state
// without importing the SDK's vocabulary into every file. Using the SDK's table
// rather than a second opinion is what keeps the colours right when the API
// adds a state: an unknown value is "still working", which renders as amber
// rather than as an alarming red.
type outcome int

const (
	outcomeWorking outcome = iota
	outcomeReady
	outcomeSettled
	outcomeFailed
)

func classify(kind, state string) outcome {
	switch firstboot.Classify(kind, state) {
	case firstboot.Ready:
		return outcomeReady
	case firstboot.Failed:
		return outcomeFailed
	case firstboot.Settled:
		return outcomeSettled
	default:
		return outcomeWorking
	}
}
