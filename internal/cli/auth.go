package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	firstboot "github.com/firstboot-io/go-sdk"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Logging in is the one command that runs without being logged in, which is why
// the whole `auth` tree is annotated as needing no client and builds its own.

func authCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage profiles and their tokens",
		Long: strings.TrimSpace(`
Manage profiles and their tokens.

A profile IS a token IS an organization: an API token is pinned to one
organization for its whole life, so managing two organizations means two
profiles rather than a flag.

The token is stored in the operating system's secret store where there is one,
and in a 0600 file where there is not. Login says which happened.
`),
		Annotations: map[string]string{"client": "none"},
	}
	cmd.AddCommand(authLoginCmd(env), authWhoamiCmd(env), authLogoutCmd(env), authListCmd(env))
	return cmd
}

func authLoginCmd(env *Env) *cobra.Command {
	var (
		name     string
		endpoint string
		tokenIn  string
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store a token for a profile",
		Long: strings.TrimSpace(`
Store an API token for a profile and verify that it works.

Create the token in the panel under Account settings, API tokens, and give it
the narrowest scopes the work needs. Without the destroy scope this CLI cannot
delete anything, which for a token that lives in CI is usually the right setting
rather than a limitation.

The token is read from the terminal without echoing. To pipe one in for an
unattended setup, use --token-stdin.
`),
		Annotations: map[string]string{"client": "none"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p := env.Printer
			if !validProfileName(name) {
				return failf(ExitUsage, "", "%q is not a valid profile name: use letters, digits, dashes and underscores", name)
			}

			if endpoint == "" {
				endpoint = strings.TrimSpace(os.Getenv(envAPIURL))
			}
			if endpoint == "" {
				var err error
				endpoint, err = prompt(p, "API URL (e.g. https://api.example.com): ")
				if err != nil {
					return err
				}
			}
			if endpoint == "" {
				return failf(ExitUsage, "", "no API URL given")
			}

			token := strings.TrimSpace(tokenIn)
			if token == "" {
				var err error
				token, err = promptSecret(p, "API token (pat_…): ")
				if err != nil {
					return err
				}
			}
			if token == "" {
				return failf(ExitUsage, "", "no token given")
			}

			// Verified BEFORE it is stored. Storing an unusable token means the
			// next command fails with a 401 that looks like a platform problem,
			// and the person has no reason to suspect the thing they just did.
			client, err := firstboot.New(
				firstboot.WithBaseURL(endpoint),
				firstboot.WithToken(token),
				firstboot.WithUserAgent("firstboot-cli/"+cmd.Root().Version),
			)
			if err != nil {
				return fail(ExitUsage, "", err)
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			who, err := client.API.AccountGetWithResponse(ctx)
			if err != nil {
				return classifyAPIError("Checking the token", err)
			}
			if who.JSON200 == nil {
				return classifyAPIError("Checking the token",
					firstboot.ErrorFrom(who.StatusCode(), who.ApplicationproblemJSONDefault, header(who.HTTPResponse)))
			}

			store, err := StoreToken(name, token)
			if err != nil {
				return fail(ExitError, "", fmt.Errorf("storing the token: %w", err))
			}

			cfg := env.Config
			prof := cfg.Profiles[name]
			prof.APIURL = endpoint
			prof.Account = who.JSON200.Email
			prof.Organization = deref(who.JSON200.ActiveOrganizationId)
			cfg.Profiles[name] = prof
			if cfg.DefaultProfile == "" || len(cfg.Profiles) == 1 {
				cfg.DefaultProfile = name
			}
			if err := cfg.Save(); err != nil {
				return fail(ExitError, "", err)
			}

			p.Says("%s Logged in as %s on profile %q.", p.Good("✓"), who.JSON200.Email, name)
			if role := who.JSON200.OrganizationRole; role != nil && *role != "" {
				p.Says("  organization role: %s", *role)
			}
			p.Says("  token stored in: %s", store)
			if store == StoreFile {
				p.Warns("%s", FallbackWarning(name))
			}
			if cfg.DefaultProfile == name && len(cfg.Profiles) > 1 {
				p.Says("  this is now the default profile")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "profile", "default", "name for this profile")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "API base URL")
	cmd.Flags().StringVar(&tokenIn, "token", "", "the token, instead of being prompted (it lands in shell history; prefer --token-stdin)")
	cmd.Flags().BoolVar(new(bool), "token-stdin", false, "read the token from stdin")
	// --token-stdin is read from the flag set rather than bound, so that reading
	// stdin happens only when it was actually asked for: a bound bool would make
	// a piped-but-unflagged run look the same as an interactive one.
	cmd.PreRunE = func(c *cobra.Command, _ []string) error {
		if ok, _ := c.Flags().GetBool("token-stdin"); ok {
			raw, err := readAll(os.Stdin)
			if err != nil {
				return fail(ExitUsage, "", err)
			}
			tokenIn = strings.TrimSpace(raw)
		}
		return nil
	}
	return cmd
}

func authWhoamiCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show which account the current profile is",
		Long: strings.TrimSpace(`
Show which account and organization the current profile authenticates as.

It asks the API rather than reading the cached name from the config file: the
question is usually being asked because something is not behaving as expected,
and a cached answer would confirm the wrong thing.
`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			who, err := env.Client.API.AccountGetWithResponse(ctx)
			if err != nil {
				return classifyAPIError("Reading the account", err)
			}
			if who.JSON200 == nil {
				return classifyAPIError("Reading the account",
					firstboot.ErrorFrom(who.StatusCode(), who.ApplicationproblemJSONDefault, header(who.HTTPResponse)))
			}
			u := who.JSON200
			_, store, _ := TokenFor(env.Profile)

			return env.Printer.Detail(u, func() [][2]string {
				rows := [][2]string{
					{"profile", env.Profile},
					{"account", u.Email},
					{"name", u.FullName},
					{"organization", deref(u.ActiveOrganizationId)},
					{"role", deref(u.OrganizationRole)},
					{"api", env.Config.Profiles[env.Profile].APIURL},
					{"token stored in", store.String()},
				}
				if u.OrganizationSuspended != nil && *u.OrganizationSuspended {
					rows = append(rows, [2]string{"organization", env.Printer.Bad("SUSPENDED")})
				}
				if u.Impersonation != nil {
					// Worth surfacing loudly: a support session acting as this
					// account is not the same as being it.
					rows = append(rows, [2]string{"impersonation", env.Printer.Warn("active")})
				}
				return rows
			})
		},
	}
}

func authLogoutCmd(env *Env) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "logout [profile]",
		Short: "Forget a profile's token",
		Long: strings.TrimSpace(`
Forget a profile's token, removing it from both the secret store and the
fallback file.

This does NOT revoke the token: it stays valid and anybody else holding a copy
can still use it. Revoke it in the panel if it may have leaked.
`),
		Annotations: map[string]string{"client": "none"},
		Args:        cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := env.Printer
			cfg := env.Config

			targets := []string{}
			switch {
			case all:
				targets = cfg.Names()
			case len(args) == 1:
				targets = []string{args[0]}
			case env.Profile != "":
				targets = []string{env.Profile}
			default:
				name, _, err := cfg.Resolve("")
				if err != nil {
					return fail(ExitUsage, "Name one, or use --all.", err)
				}
				targets = []string{name}
			}
			if len(targets) == 0 {
				p.Says("No profiles to forget.")
				return nil
			}

			for _, name := range targets {
				if err := ForgetToken(name); err != nil {
					return fail(ExitError, "", fmt.Errorf("forgetting %q: %w", name, err))
				}
				delete(cfg.Profiles, name)
				if cfg.DefaultProfile == name {
					cfg.DefaultProfile = ""
				}
				p.Says("Forgot the token for %q.", name)
			}
			if cfg.DefaultProfile == "" && len(cfg.Profiles) == 1 {
				cfg.DefaultProfile = cfg.Names()[0]
			}
			if err := cfg.Save(); err != nil {
				return fail(ExitError, "", err)
			}
			p.Warns("The token was not revoked, only forgotten. Revoke it in the panel if it may have leaked.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "forget every profile")
	return cmd
}

func authListCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:         "list",
		Short:       "List configured profiles",
		Annotations: map[string]string{"client": "none"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := env.Config
			type row struct {
				Name          string `json:"name"`
				APIURL        string `json:"api_url"`
				Account       string `json:"account,omitempty"`
				Organization  string `json:"organization,omitempty"`
				Default       bool   `json:"default"`
				TokenStoredIn string `json:"token_stored_in"`
			}
			var data []row
			for _, n := range cfg.Names() {
				p := cfg.Profiles[n]
				_, store, err := TokenFor(n)
				where := store.String()
				if err != nil {
					where = "NO TOKEN"
				}
				data = append(data, row{
					Name: n, APIURL: p.APIURL, Account: p.Account,
					Organization: p.Organization, Default: n == cfg.DefaultProfile,
					TokenStoredIn: where,
				})
			}
			return env.Printer.Print(data, func() Table {
				t := Table{
					Headers: []string{"", "profile", "account", "api", "token"},
					Empty:   "No profiles configured. Run `firstboot auth login`.",
				}
				for _, r := range data {
					mark := " "
					if r.Default {
						mark = "*"
					}
					t.Rows = append(t.Rows, []string{mark, r.Name, orDash(r.Account), r.APIURL, r.TokenStoredIn})
				}
				return t
			})
		},
	}
}

func prompt(p *Printer, label string) (string, error) {
	fmt.Fprint(p.Err, label)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", failf(ExitCancelled, "", "cancelled")
	}
	return strings.TrimSpace(line), nil
}

// promptSecret reads without echoing. A token typed into a terminal that echoed
// it is a token in a screen recording and in whatever is scrolled back.
func promptSecret(p *Printer, label string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", failf(ExitUsage,
			"Use --token-stdin to pipe it in, or run this in a terminal.",
			"cannot read a token without echoing: stdin is not a terminal")
	}
	fmt.Fprint(p.Err, label)
	raw, err := term.ReadPassword(fd)
	fmt.Fprintln(p.Err)
	if err != nil {
		return "", failf(ExitCancelled, "", "cancelled")
	}
	return strings.TrimSpace(string(raw)), nil
}

func readAll(f *os.File) (string, error) {
	var b strings.Builder
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		b.WriteString(sc.Text())
	}
	return b.String(), sc.Err()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

var _ = context.Background
