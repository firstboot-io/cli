package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"

	firstboot "github.com/firstboot-io/go-sdk"
	"github.com/firstboot-io/go-sdk/fbapi"
	"github.com/spf13/cobra"
)

func header(r *http.Response) http.Header {
	if r == nil {
		return nil
	}
	return r.Header
}

// apiErr is the shape every command uses for a non-2xx: the SDK turns the
// problem document into a typed error, and classifyAPIError turns that into an
// exit code and a sentence.
func apiErr(action string, status int, model *fbapi.ErrorModel, r *http.Response) error {
	return classifyAPIError(action, firstboot.ErrorFrom(status, model, header(r)))
}

func serverCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "server",
		Aliases: []string{"servers", "s"},
		Short:   "Virtual servers",
	}
	cmd.AddCommand(
		serverListCmd(env), serverGetCmd(env), serverCreateCmd(env),
		serverDeleteCmd(env), serverPowerCmd(env), serverResizeCmd(env),
		serverSSHCmd(env),
	)
	return cmd
}

// resolveServer turns whatever a person typed into something the API accepts.
//
// The API's detail endpoint takes a UUID or the short code, and a person almost
// always has the NAME. So a name is resolved by walking the account, which is
// one extra request and is the difference between a CLI somebody uses and one
// they look things up for first.
func resolveServer(ctx context.Context, c *firstboot.Client, ref string) (*fbapi.ServerBody, error) {
	if ref == "" {
		return nil, failf(ExitUsage, "", "no server given")
	}
	// Try it as an id or code first: that is one request, and it is what a
	// script that already holds an id will pass.
	if resp, err := c.API.ServerGetWithResponse(ctx, ref); err == nil && resp.JSON200 != nil {
		return resp.JSON200, nil
	}

	var matches []fbapi.ServerBody
	for srv, err := range c.Servers(ctx, firstboot.SearchServers(ref)) {
		if err != nil {
			return nil, classifyAPIError("Looking for the server", err)
		}
		if strings.EqualFold(srv.Name, ref) {
			matches = append(matches, srv)
		}
	}
	switch len(matches) {
	case 0:
		return nil, notFound("server", ref)
	case 1:
		return &matches[0], nil
	default:
		// Names are not unique in this API, so an ambiguous one is refused
		// rather than resolved to the first hit. Picking one would eventually
		// delete the wrong machine.
		var codes []string
		for _, m := range matches {
			codes = append(codes, m.Code)
		}
		return nil, failf(ExitUsage,
			"Use the code instead: "+strings.Join(codes, ", "),
			"%d servers are called %q", len(matches), ref)
	}
}

func serverListCmd(env *Env) *cobra.Command {
	var (
		search  string
		state   string
		project string
		tags    []string
		limit   int
	)
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List servers",
		Long: strings.TrimSpace(`
List the servers in this account.

This is one of the reasons the CLI exists: thirty servers is thirty clicks in
the panel and one command here. Combine it with --output json to feed the list
to jq, a script or a CI step.
`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()

			var opts []firstboot.ServerListOption
			if search != "" {
				opts = append(opts, firstboot.SearchServers(search))
			}
			if state != "" {
				switch state {
				case "running", "stopped", "other":
					opts = append(opts, firstboot.ServersInState(fbapi.ServersListParamsState(state)))
				default:
					return failf(ExitUsage, "",
						"--state takes running, stopped or other. It is a BUCKET rather than a "+
							"state value, so it cannot take error_provisioning")
				}
			}
			if project != "" {
				opts = append(opts, firstboot.ServersInProject(project))
			}
			if len(tags) > 0 {
				opts = append(opts, firstboot.ServersWithTags(tags...))
			}

			var out []fbapi.ServerBody
			for srv, err := range env.Client.Servers(ctx, opts...) {
				if err != nil {
					return classifyAPIError("Listing servers", err)
				}
				out = append(out, srv)
				if limit > 0 && len(out) >= limit {
					break
				}
			}

			return env.Printer.Print(out, func() Table {
				t := Table{
					Headers: []string{"name", "code", "state", "plan", "region", "ipv4", "created"},
					Empty:   "No servers in this account.",
				}
				for _, s := range out {
					t.Rows = append(t.Rows, []string{
						s.Name, s.Code,
						env.Printer.StateColor("server", string(s.State)),
						s.Plan.Slug, regionSlug(s.Region), deref(s.Ip),
						ago(&s.CreatedAt),
					})
				}
				return t
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&search, "search", "", "match a name, IP address or image, partially")
	f.StringVar(&state, "state", "", "running, stopped or other")
	f.StringVar(&project, "project", "", "only servers in this project, or `none`")
	f.StringArrayVar(&tags, "tag", nil,
		"only servers carrying this tag; repeat the flag to require several")
	f.IntVar(&limit, "limit", 0, "stop after this many")
	return cmd
}

func serverGetCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name|code|id>",
		Short: "Show one server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			s, err := resolveServer(ctx, env.Client, args[0])
			if err != nil {
				return err
			}
			return env.Printer.Detail(s, func() [][2]string {
				rows := [][2]string{
					{"name", s.Name},
					{"code", s.Code},
					{"id", s.Id},
					{"state", env.Printer.StateColor("server", string(s.State))},
					{"plan", fmt.Sprintf("%s (%d vCPU, %d MB, %d GB)",
						s.Plan.Slug, s.Plan.Cores, s.Plan.MemoryMb, s.Plan.DiskGb)},
					{"image", s.Image.Slug},
					{"region", regionSlug(s.Region)},
					{"ipv4", deref(s.Ip)},
					{"private ip", deref(s.PrivateIp)},
					{"monthly", money(s.MonthlyChargeMinor, s.ChargeCurrency)},
					{"created", ago(&s.CreatedAt)},
				}
				if s.ErrorCode != nil && *s.ErrorCode != "" {
					rows = append(rows, [2]string{"error", env.Printer.Bad(*s.ErrorCode)})
				}
				if s.RescueStartedAt != nil {
					rows = append(rows, [2]string{"rescue", env.Printer.Warn("active since " + ago(s.RescueStartedAt))})
				}
				return rows
			})
		},
	}
}

func serverCreateCmd(env *Env) *cobra.Command {
	var (
		plan, image, region string
		project, network    string
		sshKeys             []string
		userDataFile        string
	)
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a server",
		Long: strings.TrimSpace(`
Create a server and, unless --no-wait is given, wait until it is running.

A FULL MONTH is charged upfront and the unused part is refunded if the server is
deleted early. ` + "`firstboot catalog plans --region <slug>`" + ` shows what a region
actually sells: a plan a region does not carry is refused at create time by an
error that waiting cannot fix.

With no --ssh-key, a generated root password is emailed to the account. It is
not printed here.
`),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := env.need(); err != nil {
				return err
			}
			if plan == "" || image == "" {
				return failf(ExitUsage, "`firstboot catalog plans` and `firstboot catalog images` list them.",
					"--plan and --image are both required")
			}
			body := fbapi.CreateInputBody{Name: args[0], Plan: plan}
			body.Image = &image
			if region != "" {
				body.Region = &region
			}
			if project != "" {
				body.ProjectId = &project
			}
			if network != "" {
				body.NetworkId = &network
			}
			if len(sshKeys) > 0 {
				body.SshKeyIds = &sshKeys
			}
			if userDataFile != "" {
				raw, err := os.ReadFile(userDataFile)
				if err != nil {
					return fail(ExitUsage, "", fmt.Errorf("reading --user-data: %w", err))
				}
				s := string(raw)
				body.UserData = &s
			}

			// No --timeout on the create request itself: the SDK sets an
			// idempotency key and reuses it across its retries, so a slow
			// response is safe to wait for and cutting it short would leave the
			// caller not knowing whether a machine was bought.
			resp, err := env.Client.API.ServerCreateWithResponse(cmd.Context(),
				&fbapi.ServerCreateParams{}, body)
			if err != nil {
				return classifyAPIError("Creating the server", err)
			}
			if resp.JSON202 == nil {
				return apiErr("Creating the server", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			srv := &resp.JSON202.Server
			p := env.Printer

			// Printed BEFORE the wait. An interrupted apply must not leave the
			// person without the code of a machine they are now paying for.
			p.Says("%s Creating %s (code %s).", p.Good("✓"), srv.Name, srv.Code)
			if !env.Wait {
				p.Says("  It has no address yet. `firstboot server get %s` shows it come up.", srv.Code)
				return env.Printer.Print(srv, func() Table { return Table{} })
			}

			settled, err := env.Client.WaitForServer(cmd.Context(), srv.Id, env.waitOptions()...)
			if err != nil {
				return classifyAPIError("Waiting for the server", err)
			}
			p.Says("  %s at %s", env.Printer.StateColor("server", string(settled.State)), deref(settled.Ip))
			p.Says("  monthly charge: %s, charged upfront", money(settled.MonthlyChargeMinor, settled.ChargeCurrency))
			if len(sshKeys) == 0 {
				p.Says("  No SSH key was given, so a root password was emailed to the account.")
			}
			if env.Printer.Format != FormatTable {
				return env.Printer.Print(settled, func() Table { return Table{} })
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&plan, "plan", "", "plan slug, e.g. s1")
	f.StringVar(&image, "image", "", "image slug, e.g. ubuntu-24-04")
	f.StringVar(&region, "region", "", "region slug; defaults to the platform's first active region")
	f.StringVar(&project, "project", "", "project to group the server under")
	f.StringVar(&network, "network", "", "private network to join at creation")
	f.StringArrayVar(&sshKeys, "ssh-key", nil, "SSH key id to inject at first boot (repeatable)")
	f.StringVar(&userDataFile, "user-data", "", "file holding a cloud-init document")
	return cmd
}

func serverDeleteCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:     "delete <name|code|id>",
		Aliases: []string{"rm"},
		Short:   "Delete a server",
		Long: strings.TrimSpace(`
Permanently delete a server and its disks.

The unused part of the month is refunded and the address is held in quarantine
rather than reissued. The disks are NOT recoverable.

You are asked to type the server's name back. --yes skips that; a non-interactive
run without --yes is refused rather than assumed.
`),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			s, err := resolveServer(ctx, env.Client, args[0])
			if err != nil {
				return err
			}
			if err := env.Confirm(Danger{
				Operation: "serverDelete",
				Confirm:   s.Name,
				What: fmt.Sprintf("Delete %s (code %s, %s) and its disks.",
					s.Name, s.Code, deref(s.Ip)),
				Extra: "The unused part of the month is refunded. The disks are not recoverable.",
			}); err != nil {
				return err
			}

			resp, err := env.Client.API.ServerDeleteWithResponse(cmd.Context(), s.Id, &fbapi.ServerDeleteParams{})
			if err != nil {
				return classifyAPIError("Deleting the server", err)
			}
			if resp.StatusCode() >= 400 {
				return apiErr("Deleting the server", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			env.Printer.Says("%s Deleted %s (code %s).", env.Printer.Good("✓"), s.Name, s.Code)
			return nil
		},
	}
}

func serverPowerCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "power <start|stop|reboot|force-stop> <name|code|id>...",
		Short: "Start, stop or reboot servers",
		Long: strings.TrimSpace(`
Change the power state of one or more servers and wait for each action to
finish.

` + "`stop`" + ` is a graceful ACPI shutdown. ` + "`force-stop`" + ` cuts power without waiting and
loses whatever has not reached the disk.

A stopped server is STILL BILLED. Stopping saves nothing; deleting does.

Several servers can be named at once, which is the other half of why this exists:
powering six machines is one command rather than six visits to a page.
`),
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := env.need(); err != nil {
				return err
			}
			kind := fbapi.ActionInputBodyKind(strings.ReplaceAll(strings.ToLower(args[0]), "-", "_"))
			switch kind {
			case "start", "stop", "reboot", "force_stop":
			default:
				return failf(ExitUsage, "", "unknown power action %q: use start, stop, reboot or force-stop", args[0])
			}
			p := env.Printer

			var failed int
			for _, ref := range args[1:] {
				ctx, cancel := env.ctxWithTimeout(cmd.Context())
				s, err := resolveServer(ctx, env.Client, ref)
				cancel()
				if err != nil {
					p.Warns("%s: %v", ref, err)
					failed++
					continue
				}
				resp, err := env.Client.API.ServerActionCreateWithResponse(cmd.Context(), s.Id,
					fbapi.ActionInputBody{Kind: kind})
				if err != nil {
					p.Warns("%s: %v", s.Name, err)
					failed++
					continue
				}
				if resp.JSON202 == nil {
					p.Warns("%s: %v", s.Name, apiErr("", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse))
					failed++
					continue
				}
				if !env.Wait {
					p.Says("%s %s queued on %s", p.Good("✓"), kind, s.Name)
					continue
				}
				// The ACTION settles, not the server: a reboot leaves the state
				// `running` throughout, so watching the server could not tell
				// success from a request that was never applied.
				act, err := env.Client.WaitForServerAction(cmd.Context(), s.Id, uuidOf(resp.JSON202.Id), env.waitOptions()...)
				if err != nil {
					p.Warns("%s: %v", s.Name, err)
					failed++
					continue
				}
				mark := p.Good("✓")
				if act.State != "succeeded" {
					mark = p.Bad("✗")
					failed++
				}
				p.Says("%s %s on %s: %s", mark, kind, s.Name, act.State)
			}

			if kind == "stop" || kind == "force_stop" {
				p.Warns("A stopped server is still billed. Deleting is what stops the charge.")
			}
			if failed > 0 {
				return failf(ExitError, "", "%d of %d servers failed", failed, len(args)-1)
			}
			return nil
		},
	}
}

func serverResizeCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "resize <name|code|id> <plan>",
		Short: "Move a server to a bigger plan",
		Long: strings.TrimSpace(`
Move a server to a bigger plan and wait for it to come back.

Disk only ever grows and the API refuses a downgrade, so this is an upgrade. It
RESTARTS the machine, so whatever it serves goes down for the length of the
resize, and it raises the monthly charge.
`),
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			s, err := resolveServer(ctx, env.Client, args[0])
			cancel()
			if err != nil {
				return err
			}
			// Not `destroy`-scoped, so no typed confirmation; but it restarts a
			// live machine, and saying so before doing it costs nothing.
			env.Printer.Warns("This restarts %s. Whatever it serves goes down for the length of the resize.", s.Name)

			resp, err := env.Client.API.ServerResizeWithResponse(cmd.Context(), s.Id,
				fbapi.ResizeInputBody{Plan: args[1]})
			if err != nil {
				return classifyAPIError("Resizing the server", err)
			}
			if resp.StatusCode() >= 400 {
				return apiErr("Resizing the server", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			if !env.Wait {
				env.Printer.Says("%s Resize of %s queued.", env.Printer.Good("✓"), s.Name)
				return nil
			}
			settled, err := env.Client.WaitForServer(cmd.Context(), s.Id, env.waitOptions()...)
			if err != nil {
				return classifyAPIError("Waiting for the resize", err)
			}
			env.Printer.Says("%s %s is on plan %s and is %s. Monthly charge: %s.",
				env.Printer.Good("✓"), settled.Name, settled.Plan.Slug,
				env.Printer.StateColor("server", string(settled.State)),
				money(settled.MonthlyChargeMinor, settled.ChargeCurrency))
			return nil
		},
	}
}

func serverSSHCmd(env *Env) *cobra.Command {
	var user string
	cmd := &cobra.Command{
		Use:   "ssh <name|code|id> [-- ssh args...]",
		Short: "Open an SSH session to a server by name",
		Long: strings.TrimSpace(`
Resolve a server's name to its address and hand over to your own ssh.

This is the third reason the CLI exists: looking up an IP in the panel to paste
into a terminal is a browser round trip for something the terminal already knows.

Everything after -- is passed to ssh unchanged, so this works:

  firstboot server ssh web-1 -- -p 2222 -A
  firstboot server ssh web-1 -- systemctl status nginx

It uses YOUR ssh and YOUR keys. This CLI holds no key material and does not
manage known_hosts; a host key change is ssh's warning to give, not ours to
suppress.
`),
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			s, err := resolveServer(ctx, env.Client, args[0])
			cancel()
			if err != nil {
				return err
			}
			if s.Ip == nil || *s.Ip == "" {
				return failf(ExitConflict,
					"A server has no address until it has finished provisioning.",
					"%s has no public address yet (state %s)", s.Name, s.State)
			}
			if firstboot.Classify("server", string(s.State)) == firstboot.Working {
				env.Printer.Warns("%s is %s, so the connection may be refused.", s.Name, s.State)
			}

			bin, err := exec.LookPath("ssh")
			if err != nil {
				return failf(ExitError, "Install an ssh client, or use the address directly: "+*s.Ip,
					"no ssh in PATH")
			}
			argv := []string{"ssh", user + "@" + *s.Ip}
			argv = append(argv, args[1:]...)

			// Replaced rather than run as a child: ssh owns the terminal from
			// here, and a wrapper process in between breaks window resizing,
			// signal handling and the exit code.
			env.Printer.Says("%s %s@%s", env.Printer.dim("ssh"), user, *s.Ip)
			return replaceProcess(bin, argv, os.Environ())
		},
	}
	cmd.Flags().StringVarP(&user, "user", "u", "root", "the user to connect as")
	return cmd
}

func regionSlug(r *fbapi.RegionRef) string {
	if r == nil {
		return "-"
	}
	return r.Slug
}
