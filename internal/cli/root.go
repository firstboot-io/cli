package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	firstboot "github.com/firstboot-io/go-sdk"
	"github.com/spf13/cobra"
)

// The SDK's environment variables, re-exported rather than redefined. A CLI
// that invented its own names beside the library's would be two answers to
// "where does the token come from".
const (
	envToken  = firstboot.EnvToken
	envAPIURL = firstboot.EnvBaseURL
)

// Env is what every command is handed: a configured client, a printer that
// knows the output format, and the answers to the global flags.
//
// Built once in PersistentPreRunE rather than per command, and built LAZILY:
// `firstboot auth login`, `--help` and `--version` must work with no profile at
// all, and a root command that demanded a token before parsing would make the
// login command impossible to run.
type Env struct {
	Client  *firstboot.Client
	Printer *Printer
	Profile string
	Config  *Config

	Wait    bool
	Timeout time.Duration
	Yes     bool
}

// Wait options for an SDK waiter, from the global flags.
func (e *Env) waitOptions() []firstboot.WaitOption {
	if e.Timeout <= 0 {
		return nil
	}
	return []firstboot.WaitOption{firstboot.WithTimeout(e.Timeout)}
}

type globals struct {
	output   string
	profile  string
	noColor  bool
	yes      bool
	wait     bool
	noWait   bool
	timeout  time.Duration
	endpoint string
}

// Execute builds the command tree and runs it. It returns an exit code rather
// than calling os.Exit, so main stays testable and deferred cleanups run.
func Execute(version string) int {
	g := &globals{}
	env := &Env{}

	root := &cobra.Command{
		Use:   "firstboot",
		Short: "Manage Firstboot cloud resources from the terminal",
		Long: strings.TrimSpace(`
Manage Firstboot cloud resources from the terminal.

Three things this does that the panel cannot:

  Bulk work      list, filter and act on many resources in one command
  Piping         --output json feeds jq, a script, or a CI step
  Staying put    firstboot server ssh web-1 connects; app logs --follow streams

A profile IS a token IS an organization. A token is pinned to one organization
for its whole life, so there is no --org flag: two organizations means two
profiles. Start with:

  firstboot auth login
`),
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		// A command that runs without a subcommand should show help rather than
		// succeed silently, which is what cobra does by default.
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return setup(cmd, g, env)
		},
	}

	f := root.PersistentFlags()
	f.StringVarP(&g.output, "output", "o", "table", "output format: table, json or yaml")
	f.StringVarP(&g.profile, "profile", "p", "", "which configured profile (account) to use")
	f.BoolVar(&g.noColor, "no-color", false, "never colourise output")
	f.BoolVarP(&g.yes, "yes", "y", false, "skip the confirmation on destructive commands")
	f.BoolVar(&g.wait, "wait", true, "wait for asynchronous work to finish")
	f.BoolVar(&g.noWait, "no-wait", false, "return as soon as the work is accepted")
	f.DurationVar(&g.timeout, "timeout", 0, "how long to wait (default: per resource, see --help of the command)")
	f.String("endpoint", "", "API base URL, overriding the profile's")
	// The endpoint flag is read through the flag set rather than a variable so
	// that "was it given at all" is answerable: an empty string is a legitimate
	// thing to have not typed, and defaulting over the profile's URL would make
	// --endpoint "" mean something different from omitting it.
	f.Lookup("endpoint").NoOptDefVal = ""

	root.AddCommand(
		authCmd(env),
		catalogCmd(env),
		serverCmd(env),
		appCmd(env),
		databaseCmd(env),
		volumeCmd(env),
		networkCmd(env),
		firewallCmd(env),
		loadBalancerCmd(env),
		floatingIPCmd(env),
		dnsCmd(env),
		domainCmd(env),
		sshKeyCmd(env),
		projectCmd(env),
		tagCmd(env),
		isoCmd(env),
		walletCmd(env),
		accountCmd(env),
	)
	root.SetVersionTemplate("firstboot {{.Version}}\n")

	ctx, stop := signalContext()
	defer stop()

	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}
	return report(env, err, ctx)
}

// setup fills the Env. Commands that do not need a client say so with an
// annotation, which is how `auth login` runs before there is one.
func setup(cmd *cobra.Command, g *globals, env *Env) error {
	format := Format(strings.ToLower(g.output))
	switch format {
	case FormatTable, FormatJSON, FormatYAML:
	default:
		return failf(ExitUsage, "", "unknown --output %q: use table, json or yaml", g.output)
	}
	env.Printer = NewPrinter(format, g.noColor)
	env.Yes = g.yes
	env.Timeout = g.timeout
	// --no-wait beats --wait. Both can be present because --wait has a default
	// of true and cannot express "off" on its own.
	env.Wait = g.wait && !g.noWait

	cfg, err := LoadConfig()
	if err != nil {
		return fail(ExitError, "", err)
	}
	env.Config = cfg

	if cmd.Annotations["client"] == "none" {
		return nil
	}

	name, profile, err := cfg.Resolve(g.profile)
	if err != nil && !errors.Is(err, ErrNoProfile) {
		return fail(ExitUsage, "", err)
	}
	env.Profile = name

	token, _, tokErr := TokenFor(name)
	endpoint := profile.APIURL
	if v, _ := cmd.Flags().GetString("endpoint"); v != "" {
		endpoint = v
	}
	if v := strings.TrimSpace(os.Getenv(envAPIURL)); v != "" && endpoint == "" {
		endpoint = v
	}

	if tokErr != nil || token == "" {
		return failf(ExitAuth,
			"Run `firstboot auth login` to store a token, or set "+envToken+" and "+
				envAPIURL+" for a one-off.",
			"not logged in")
	}
	if endpoint == "" {
		return failf(ExitUsage,
			"Set it with `firstboot auth login`, the --endpoint flag, or "+envAPIURL+".",
			"no API URL for profile %q", name)
	}

	client, err := firstboot.New(
		firstboot.WithBaseURL(endpoint),
		firstboot.WithToken(token),
		firstboot.WithUserAgent("firstboot-cli/"+cmd.Root().Version),
	)
	if err != nil {
		return fail(ExitUsage, "", err)
	}
	env.Client = client
	return nil
}

// report prints a failure the way a person and a script both need it, and
// returns the exit code.
func report(env *Env, err error, ctx context.Context) int {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "\ncancelled")
		return ExitCancelled
	}

	p := env.Printer
	if p == nil {
		p = NewPrinter(FormatTable, false)
	}

	var ee *exitError
	if errors.As(err, &ee) {
		fmt.Fprintln(p.Err, p.Bad("error: ")+ee.err.Error())
		if ee.hint != "" {
			fmt.Fprintln(p.Err)
			fmt.Fprintln(p.Err, ee.hint)
		}
		return ee.code
	}

	// Anything cobra raised itself is a usage problem: an unknown flag, a
	// missing argument, a subcommand that does not exist.
	fmt.Fprintln(p.Err, p.Bad("error: ")+err.Error())
	return ExitUsage
}

// need is the guard every command that talks to the API starts with. It exists
// so a nil client is impossible rather than a panic: setup can legitimately
// leave one nil for the commands annotated as not needing it.
func (e *Env) need() error {
	if e.Client == nil {
		return failf(ExitAuth, "Run `firstboot auth login`.", "not logged in")
	}
	return nil
}

// ctxWithTimeout applies --timeout to a plain API call. The waiters take it
// through WaitOption instead; this is for the single requests, where an
// unbounded wait on a hung connection is the failure mode.
func (e *Env) ctxWithTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	if e.Timeout > 0 {
		return context.WithTimeout(parent, e.Timeout)
	}
	return context.WithCancel(parent)
}
