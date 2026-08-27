package cli

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	firstboot "github.com/firstboot-io/firstboot-go"
	"github.com/firstboot-io/firstboot-go/fbapi"
)

// Grouping in the CLI: `--tag` and `--project` on every list that has them, and
// one `tag` command group for reading and writing them.
//
// The filters are FLAGS on the existing lists rather than a `firstboot tag
// resources` command, for the same reason `--state` is a flag: a person
// narrowing a list is doing one thing, and a second command to do it would make
// them choose which of the two they wanted before they knew.
//
// Writing is the opposite shape: one `tag set <kind> <ref> ...` covering all
// eight kinds instead of a `tag` subcommand on eight command groups. A tag set
// is the same operation whatever it is on, and eight copies of it would be
// eight help texts to keep in step.

// addGroupingFlags puts `--tag` and `--project` on a list command.
//
// `--tag` is a StringArray rather than a StringSlice deliberately: a
// StringSlice splits on commas, and while a tag cannot contain one today that
// is a rule in another repository. Repeating the flag is unambiguous whatever
// the character set becomes.
func addGroupingFlags(cmd *cobra.Command, tags *[]string, project *string, plural string) {
	f := cmd.Flags()
	f.StringArrayVar(tags, "tag", nil,
		"only "+plural+" carrying this tag; repeat the flag to require several")
	f.StringVar(project, "project", "", "only "+plural+" in this project, or `none` for the ones in no project")
}

// tagCmd is `firstboot tag`: read the inventory, and set a resource's tags.
func tagCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tag",
		Aliases: []string{"tags"},
		Short:   "Tags",
		Long: strings.TrimSpace(`
Tags group resources across projects.

A resource is in at most one project and wears any number of tags, so a tag is
how you select a set: every server in the web tier, every database belonging to
one customer. Every list command takes --tag to filter by them.

There is no command that CREATES a tag. A tag exists because something carries
it and stops existing when the last resource drops it.
`),
	}
	cmd.AddCommand(tagListCmd(env), tagSetCmd(env))
	return cmd
}

func tagListCmd(env *Env) *cobra.Command {
	var kind string
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the tags in use, with how many resources carry each",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()

			params := &fbapi.TagsListParams{}
			if kind != "" {
				if !isTaggableKind(kind) {
					return failf(ExitUsage, "", "--kind takes one of: %s", strings.Join(taggableKinds(), ", "))
				}
				k := fbapi.TagsListParamsKind(kind)
				params.Kind = &k
			}
			resp, err := env.Client.API.TagsListWithResponse(ctx, params)
			if err != nil {
				return classifyAPIError("Listing tags", err)
			}
			if resp.JSON200 == nil {
				return apiErr("Listing tags", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			var out []fbapi.TagCountBody
			if resp.JSON200.Tags != nil {
				out = *resp.JSON200.Tags
			}
			return env.Printer.Print(out, func() Table {
				t := Table{Headers: []string{"tag", "resources"}, Empty: "No tags in use."}
				for _, row := range out {
					t.Rows = append(t.Rows, []string{row.Tag, fmt.Sprintf("%d", row.Count)})
				}
				return t
			})
		},
	}
	c.Flags().StringVar(&kind, "kind", "", "only tags used by this kind of resource")
	return c
}

func tagSetCmd(env *Env) *cobra.Command {
	return &cobra.Command{
		Use:   "set <kind> <id> [tag...]",
		Short: "Replace a resource's tags",
		Long: strings.TrimSpace(`
Replace the whole tag set on one resource.

REPLACE, not add: whatever you list is what the resource ends up with, and
listing nothing clears them. That is what the endpoint does, and an add/remove
pair would have to read the current set first and race with anyone else editing
it.

  firstboot tag set server web-1 env:prod role:web
  firstboot tag set volume 6f2… env:prod
  firstboot tag set app blog          # clears every tag on the app
`),
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := env.need(); err != nil {
				return err
			}
			kind, ref := args[0], args[1]
			if !isTaggableKind(kind) {
				return failf(ExitUsage, "", "%q cannot be tagged. Taggable kinds: %s",
					kind, strings.Join(taggableKinds(), ", "))
			}
			tags, err := normalizeTags(args[2:])
			if err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()

			// A server is addressed by name or code everywhere else in this
			// CLI, so it is here too; the rest take the id the list prints.
			if kind == "server" {
				srv, err := resolveServer(ctx, env.Client, ref)
				if err != nil {
					return err
				}
				ref = srv.Id
			}

			status, model, resp, err := setTagsOf(ctx, env.Client, kind, ref, tags)
			if err != nil {
				return classifyAPIError("Setting tags", err)
			}
			if status >= 400 {
				return apiErr("Setting tags", status, model, resp)
			}
			if len(tags) == 0 {
				env.Printer.Says("Cleared every tag on the %s.", kind)
			} else {
				env.Printer.Says("Tagged the %s: %s", kind, strings.Join(tags, ", "))
			}
			return nil
		},
	}
}

// normalizeTags puts the arguments into the form the API stores, and refuses
// what it would refuse — before the request, so the message names the tag.
func normalizeTags(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		t := strings.ToLower(strings.TrimSpace(raw))
		if t == "" {
			continue
		}
		if len(t) > 32 || !tagPattern.MatchString(t) {
			return nil, failf(ExitUsage, "",
				"%q is not a valid tag. A tag starts with a letter or a digit and uses only "+
					"a-z, 0-9, dot, underscore, colon and dash, up to 32 characters", raw)
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) > 10 {
		return nil, failf(ExitUsage, "", "at most 10 tags per resource; %d given", len(out))
	}
	sort.Strings(out)
	return out, nil
}

// setTagsOf dispatches to the right endpoint. One switch rather than eight
// commands; the generated client gives each endpoint its own response type,
// which is why every branch ends the same three lines rather than returning a
// value the caller could share.
func setTagsOf(ctx context.Context, c *firstboot.Client, kind, ref string, tags []string) (int, *fbapi.ErrorModel, *http.Response, error) {
	body := fbapi.TagsBody{Tags: &tags}
	switch kind {
	case "server":
		out, err := c.API.ServerTagsSetWithResponse(ctx, ref, body)
		if err != nil {
			return 0, nil, nil, err
		}
		return out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse, nil
	case "volume":
		out, err := c.API.VolumeTagsSetWithResponse(ctx, ref, body)
		if err != nil {
			return 0, nil, nil, err
		}
		return out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse, nil
	case "network":
		out, err := c.API.NetworkTagsSetWithResponse(ctx, ref, body)
		if err != nil {
			return 0, nil, nil, err
		}
		return out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse, nil
	case "database":
		out, err := c.API.DatabaseTagsSetWithResponse(ctx, ref, body)
		if err != nil {
			return 0, nil, nil, err
		}
		return out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse, nil
	case "load_balancer":
		out, err := c.API.LoadBalancerTagsSetWithResponse(ctx, ref, body)
		if err != nil {
			return 0, nil, nil, err
		}
		return out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse, nil
	case "dns_zone":
		out, err := c.API.DnsZoneTagsSetWithResponse(ctx, ref, body)
		if err != nil {
			return 0, nil, nil, err
		}
		return out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse, nil
	case "app":
		out, err := c.API.AppTagsSetWithResponse(ctx, ref, body)
		if err != nil {
			return 0, nil, nil, err
		}
		return out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse, nil
	case "domain":
		out, err := c.API.DomainTagsSetWithResponse(ctx, ref, body)
		if err != nil {
			return 0, nil, nil, err
		}
		return out.StatusCode(), out.ApplicationproblemJSONDefault, out.HTTPResponse, nil
	}
	return 0, nil, nil, fmt.Errorf("unknown kind %q", kind)
}

// taggableKinds is the list the platform's own grouping table publishes, in the
// order a person is most likely to want them.
func taggableKinds() []string {
	return []string{"server", "volume", "network", "database", "load_balancer", "dns_zone", "app", "domain"}
}

func isTaggableKind(k string) bool {
	for _, v := range taggableKinds() {
		if v == k {
			return true
		}
	}
	return false
}

// tagPattern mirrors the platform's `tag_array_valid` and the SDK-side rules.
var tagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,31}$`)
