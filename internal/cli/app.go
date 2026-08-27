package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	firstboot "github.com/firstboot-io/firstboot-go"
	"github.com/firstboot-io/firstboot-go/fbapi"
	"github.com/spf13/cobra"
)

func appCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "app",
		Aliases: []string{"apps"},
		Short:   "Container apps",
	}
	cmd.AddCommand(
		appListCmd(env), appGetCmd(env), appDeployCmd(env),
		appLogsCmd(env), appRollbackCmd(env), appScaleCmd(env),
	)
	return cmd
}

func appListCmd(env *Env) *cobra.Command {
	var tags []string
	var project string
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List apps",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			var opts []firstboot.AppListOption
			if len(tags) > 0 {
				opts = append(opts, firstboot.AppsWithTags(tags...))
			}
			if project != "" {
				opts = append(opts, firstboot.AppsInProject(project))
			}
			var apps []fbapi.AppBody
			for a, err := range env.Client.Apps(ctx, opts...) {
				if err != nil {
					return classifyAPIError("Listing apps", err)
				}
				apps = append(apps, a)
			}
			return env.Printer.Print(apps, func() Table {
				t := Table{
					Headers: []string{"name", "code", "desired", "observed", "replicas", "url"},
					Empty:   "No apps in this account.",
				}
				for _, a := range apps {
					t.Rows = append(t.Rows, []string{
						a.Name, a.Code, a.DesiredState, deref(a.ObservedState),
						fmt.Sprintf("%d/%d", a.ReplicasDesired, a.ReplicasMax),
						deref(a.Url),
					})
				}
				return t
			})
		},
	}
	addGroupingFlags(c, &tags, &project, "apps")
	return c
}

func appGetCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:     "get <code>",
		Aliases: []string{"status"},
		Short:   "Show an app, its releases and its recent builds",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			resp, err := env.Client.API.AppGetWithResponse(ctx, args[0])
			if err != nil {
				return classifyAPIError("Reading the app", err)
			}
			if resp.JSON200 == nil {
				return apiErr("Reading the app", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			a := resp.JSON200
			if err := env.Printer.Detail(a, func() [][2]string {
				rows := [][2]string{
					{"name", a.Name},
					{"code", a.Code},
					{"desired state", a.DesiredState},
					// The gap between desired and observed IS a deploy in
					// progress, and it is also what a stuck app looks like.
					{"observed state", deref(a.ObservedState) + driftNote(a)},
					{"replicas", fmt.Sprintf("%d running target, min %d, max %d (plan ceiling %d)",
						a.ReplicasDesired, a.ReplicasMin, a.ReplicasMax, a.PlanMaxReplicas)},
					{"plan", fmt.Sprintf("%s, %d MB, %d millicores, port %d",
						deref(a.Plan), a.MemoryMb, a.CpuMillis, a.Port)},
					{"url", deref(a.Url)},
					{"image", a.Image},
				}
				if a.SourceUrl != nil && *a.SourceUrl != "" {
					rows = append(rows, [2]string{"source",
						fmt.Sprintf("%s @ %s, auto-deploy %t", *a.SourceUrl, deref(a.GitRef), a.AutoDeploy)})
				}
				return rows
			}); err != nil {
				return err
			}
			if env.Printer.Format != FormatTable {
				return nil
			}

			// Releases and builds answer the two questions that follow "is it
			// healthy": what is it running, and did the last change work.
			if rel, err := env.Client.API.AppReleasesListWithResponse(ctx, args[0]); err == nil && rel.JSON200 != nil {
				env.Printer.Says("\ncurrent release: v%d", rel.JSON200.Current)
				if rel.JSON200.Releases != nil {
					for i, r := range *rel.JSON200.Releases {
						if i >= 5 {
							break
						}
						line := fmt.Sprintf("  v%d %s %s", r.Number, r.Trigger, shortSHA(deref(r.CommitSha)))
						if !r.Deployable {
							line += env.Printer.dim(" (image gone, cannot roll back to this)")
						}
						env.Printer.Says("%s", line)
					}
				}
			}
			if b, err := env.Client.API.AppBuildsListWithResponse(ctx, args[0]); err == nil && b.JSON200 != nil && b.JSON200.Builds != nil {
				builds := *b.JSON200.Builds
				if len(builds) > 0 {
					env.Printer.Says("\nrecent builds:")
					for i, bd := range builds {
						if i >= 3 {
							break
						}
						env.Printer.Says("  %s %s %s", buildColor(env.Printer, string(bd.State)),
							shortSHA(deref(bd.CommitSha)), ago(&bd.CreatedAt))
					}
				}
			}
			return nil
		},
	}
}

func appDeployCmd(env *Env) *cobra.Command {
	var gitRef string
	cmd := &cobra.Command{
		Use:   "deploy <code>",
		Short: "Build and deploy an app, and wait for the build",
		Long: strings.TrimSpace(`
Build an app from its repository and wait for the build to finish.

A build is a JOB with its own lifecycle. Watching the app tells you nothing,
because a running app stays running through a build that fails, so this waits on
the build and reports its outcome. A failed build leaves the app serving its
previous version and prints the tail of the log.
`),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := env.need(); err != nil {
				return err
			}
			body := fbapi.BuildCreateInputBody{}
			if gitRef != "" {
				body.GitRef = &gitRef
			}
			resp, err := env.Client.API.AppBuildCreateWithResponse(cmd.Context(), args[0], body)
			if err != nil {
				return classifyAPIError("Starting the build", err)
			}
			if resp.JSON202 == nil {
				return apiErr("Starting the build", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			p := env.Printer
			p.Says("%s Build %s queued for %s (ref %s).", p.Good("✓"), resp.JSON202.Id, args[0], resp.JSON202.GitRef)
			if !env.Wait {
				return nil
			}

			settled, err := env.Client.WaitForBuild(cmd.Context(), args[0], resp.JSON202.Id, env.waitOptions()...)
			if err != nil {
				return classifyAPIError("Waiting for the build", err)
			}
			switch settled.State {
			case fbapi.BuildBodyStateSucceeded:
				p.Says("%s Build succeeded. The new version is rolling out.", p.Good("✓"))
				return nil
			case fbapi.BuildBodyStateCanceled:
				p.Says("Build cancelled. The app is unchanged.")
				return nil
			default:
				if settled.Log != nil && *settled.Log != "" {
					fmt.Fprintln(p.Err, "\n"+tail(*settled.Log, 40))
				}
				return failf(ExitError,
					"The app is UNCHANGED and still serving its previous version.",
					"build failed: %s", firstLine(deref(settled.Error)))
			}
		},
	}
	cmd.Flags().StringVar(&gitRef, "ref", "", "branch, tag or commit to build; defaults to the app's own ref")
	return cmd
}

func appLogsCmd(env *Env) *cobra.Command {
	var (
		lines  int
		follow bool
	)
	cmd := &cobra.Command{
		Use:   "logs <code>",
		Short: "Read an app's logs",
		Long: strings.TrimSpace(`
Fetch recent log output from an app's containers.

The request travels to the node and back, so this asks and then waits for the
answer. With --follow it keeps asking and prints what is new, which is the
closest this API offers to a stream.
`),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := env.need(); err != nil {
				return err
			}
			if lines <= 0 {
				lines = 200
			}
			if !follow {
				out, err := fetchLogs(cmd.Context(), env, args[0], int64(lines))
				if err != nil {
					return err
				}
				fmt.Fprintln(env.Printer.Out, out)
				return nil
			}

			// --follow is polling, and says so rather than implying a stream.
			// Only the lines that are new since the last answer are printed, so
			// a screen does not repeat itself every two seconds.
			env.Printer.Says("%s", env.Printer.dim("following (polling every 2s; Ctrl-C to stop)"))
			var seen string
			for {
				out, err := fetchLogs(cmd.Context(), env, args[0], int64(lines))
				if err != nil {
					return err
				}
				if fresh := strings.TrimPrefix(out, seen); fresh != out || seen == "" {
					fmt.Fprint(env.Printer.Out, fresh)
					if !strings.HasSuffix(fresh, "\n") && fresh != "" {
						fmt.Fprintln(env.Printer.Out)
					}
				} else if out != seen {
					fmt.Fprintln(env.Printer.Out, out)
				}
				seen = out
				select {
				case <-cmd.Context().Done():
					return nil
				case <-time.After(2 * time.Second):
				}
			}
		},
	}
	cmd.Flags().IntVarP(&lines, "lines", "n", 200, "how many lines from the end")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep polling and print what is new")
	return cmd
}

func fetchLogs(ctx context.Context, env *Env, code string, n int64) (string, error) {
	resp, err := env.Client.API.AppLogsRequestWithResponse(ctx, code, fbapi.LogRequestInputBody{Lines: &n})
	if err != nil {
		return "", classifyAPIError("Requesting the logs", err)
	}
	if resp.JSON202 == nil {
		return "", apiErr("Requesting the logs", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
	}
	deadline := time.Now().Add(30 * time.Second)
	delay := 300 * time.Millisecond
	for {
		got, err := env.Client.API.AppLogsGetWithResponse(ctx, code, resp.JSON202.Id)
		if err != nil {
			return "", classifyAPIError("Collecting the logs", err)
		}
		if got.JSON200 == nil {
			return "", apiErr("Collecting the logs", got.StatusCode(), got.ApplicationproblemJSONDefault, got.HTTPResponse)
		}
		switch got.JSON200.State {
		case "succeeded", "done", "ready":
			return deref(got.JSON200.Output), nil
		case "failed", "error":
			return "", failf(ExitError, "", "the node could not answer: %s", deref(got.JSON200.Error))
		}
		if time.Now().After(deadline) {
			return "", failf(ExitTimeout,
				"A busy or offline node does this; the app itself may be fine.",
				"the node did not answer within 30s")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(delay):
		}
		if delay *= 2; delay > 3*time.Second {
			delay = 3 * time.Second
		}
	}
}

func appRollbackCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "rollback <code> <release>",
		Short: "Put an earlier release back into service",
		Long: strings.TrimSpace(`
Put an earlier release back into service, by its number (the 13 in v13).

This REPLACES what is currently serving traffic. ` + "`firstboot app get`" + ` lists the
releases and says which are still deployable: an image that has aged out of
retention cannot be rolled back to.
`),
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := env.need(); err != nil {
				return err
			}
			var n int32
			if _, err := fmt.Sscanf(args[1], "%d", &n); err != nil || n <= 0 {
				return failf(ExitUsage, "", "%q is not a release number", args[1])
			}
			resp, err := env.Client.API.AppRollbackWithResponse(cmd.Context(), args[0],
				fbapi.AppRollbackInputBody{Release: n})
			if err != nil {
				return classifyAPIError("Rolling back", err)
			}
			if resp.StatusCode() >= 400 {
				return apiErr("Rolling back", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			env.Printer.Says("%s Rolled %s back to v%d. The replacement rolls out in the background.",
				env.Printer.Good("✓"), args[0], n)
			return nil
		},
	}
}

func appScaleCmd(env *Env) *cobra.Command {
	var min, max int32
	cmd := &cobra.Command{
		Use:   "scale <code>",
		Short: "Set an app's replica bounds",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := env.need(); err != nil {
				return err
			}
			if min <= 0 || max <= 0 {
				return failf(ExitUsage, "", "--min and --max are both required and must be positive")
			}
			if max < min {
				return failf(ExitUsage, "", "--max (%d) is below --min (%d)", max, min)
			}
			resp, err := env.Client.API.AppScaleWithResponse(cmd.Context(), args[0],
				fbapi.AppScaleInputBody{ReplicasMin: min, ReplicasMax: max})
			if err != nil {
				return classifyAPIError("Scaling the app", err)
			}
			if resp.JSON200 == nil {
				return apiErr("Scaling the app", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			env.Printer.Says("%s %s now runs between %d and %d replicas.",
				env.Printer.Good("✓"), args[0], min, max)
			return nil
		},
	}
	cmd.Flags().Int32Var(&min, "min", 0, "fewest containers to keep running")
	cmd.Flags().Int32Var(&max, "max", 0, "most the platform may run, bounded by the plan")
	return cmd
}

func driftNote(a *fbapi.AppBody) string {
	observed := deref(a.ObservedState)
	switch {
	case observed == "-":
		return " (the node has not reported yet)"
	case observed == a.DesiredState:
		return ""
	default:
		return " (they DISAGREE: a deploy is rolling, or the app cannot reach the state it was asked for)"
	}
}

func buildColor(p *Printer, state string) string {
	switch state {
	case "succeeded":
		return p.Good(state)
	case "failed":
		return p.Bad(state)
	case "canceled":
		return state
	default:
		return p.Warn(state)
	}
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// tail keeps the END of a build log, which is where the error is. The head is
// reliably the dependency install of a build that failed at the last step.
func tail(s string, n int) string {
	ls := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(ls) <= n {
		return s
	}
	return strings.Join(ls[len(ls)-n:], "\n")
}

var _ = firstboot.Classify
