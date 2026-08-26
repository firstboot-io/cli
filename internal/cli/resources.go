package cli

import (
	"fmt"
	"strings"

	firstboot "github.com/firstboot-io/firstboot-go"
	"github.com/firstboot-io/firstboot-go/fbapi"
	"github.com/spf13/cobra"
)

// The rest of the surface: list, get, and the deletes.
//
// These are the resources a person reads more often than they change. Creating
// a load balancer or a database from a flag soup is worse than doing it in the
// panel or in Terraform, which is why the create side of them is deliberately
// not here: the CLI's reason to exist is bulk reading, piping and staying in the
// terminal, and a `--rule` flag repeated six times is none of those.
//
// The deletes ARE here, because "delete these four things" is exactly the bulk
// case, and every one of them goes through the same confirmation gate.

// deleteSpec is what the shared delete command needs to know about a resource.
type deleteSpec struct {
	kind      string
	operation string
	// resolve turns a reference into an id and a display name.
	resolve func(*Env, *cobra.Command, string) (id, name string, err error)
	// remove calls the API.
	remove func(*Env, *cobra.Command, string) error
	extra  string
}

func deleteCmd(env *Env, s deleteSpec) *cobra.Command {
	return &cobra.Command{
		Use:     "delete <name|id>...",
		Aliases: []string{"rm"},
		Short:   "Delete " + s.kind + "s",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := env.need(); err != nil {
				return err
			}
			var failed int
			for _, ref := range args {
				id, name, err := s.resolve(env, cmd, ref)
				if err != nil {
					env.Printer.Warns("%s: %v", ref, err)
					failed++
					continue
				}
				if err := env.Confirm(Danger{
					Operation: s.operation,
					Confirm:   name,
					What:      fmt.Sprintf("Delete the %s %s.", s.kind, name),
					Extra:     s.extra,
				}); err != nil {
					return err
				}
				if err := s.remove(env, cmd, id); err != nil {
					env.Printer.Warns("%s: %v", name, err)
					failed++
					continue
				}
				env.Printer.Says("%s Deleted %s %s.", env.Printer.Good("✓"), s.kind, name)
			}
			if failed > 0 {
				return failf(ExitError, "", "%d of %d failed", failed, len(args))
			}
			return nil
		},
	}
}

// ---------------------------------------------------------------- catalog

func catalogCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "What can be created, and what it costs",
	}

	var region string
	plans := &cobra.Command{
		Use:   "plans",
		Short: "Server plans and their prices",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()

			// The region's own list of plan slugs is what answers
			// PLAN_NOT_OFFERED_IN_REGION before it happens, so narrowing filters
			// the plans by what that region actually sells rather than by a
			// query parameter the endpoint does not have.
			sold := map[string]bool{}
			if region != "" {
				regions, err := env.Client.API.RegionsListWithResponse(ctx)
				if err != nil {
					return classifyAPIError("Reading the regions", err)
				}
				if regions.JSON200 == nil {
					return apiErr("Reading the regions", regions.StatusCode(), regions.ApplicationproblemJSONDefault, regions.HTTPResponse)
				}
				found := false
				if regions.JSON200.Regions != nil {
					for _, r := range *regions.JSON200.Regions {
						if !strings.EqualFold(r.Slug, region) {
							continue
						}
						found = true
						if r.AvailableSizeSlugs != nil {
							for _, s := range *r.AvailableSizeSlugs {
								sold[s] = true
							}
						}
					}
				}
				if !found {
					return notFound("region", region)
				}
			}

			resp, err := env.Client.API.PlansListWithResponse(ctx, &fbapi.PlansListParams{})
			if err != nil {
				return classifyAPIError("Reading the plans", err)
			}
			if resp.JSON200 == nil {
				return apiErr("Reading the plans", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			var out []fbapi.PlanBody
			if resp.JSON200.Plans != nil {
				for _, p := range *resp.JSON200.Plans {
					if region != "" && !sold[p.Slug] {
						continue
					}
					out = append(out, p)
				}
			}
			return env.Printer.Print(out, func() Table {
				empty := "No plans."
				if region != "" {
					empty = "No plans sold in " + region + "."
				}
				t := Table{Headers: []string{"slug", "vcpu", "memory", "disk", "traffic", "monthly"}, Empty: empty}
				for _, p := range out {
					t.Rows = append(t.Rows, []string{
						p.Slug, fmt.Sprint(p.Cores),
						fmt.Sprintf("%d MB", p.MemoryMb), fmt.Sprintf("%d GB", p.DiskGb),
						fmt.Sprintf("%d GB", p.TrafficGb),
						money(p.MonthlyPriceMinor, p.Currency),
					})
				}
				return t
			})
		},
	}
	plans.Flags().StringVar(&region, "region", "", "only the plans this region sells")

	regions := &cobra.Command{
		Use:   "regions",
		Short: "Regions and what each one sells",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			resp, err := env.Client.API.RegionsListWithResponse(ctx)
			if err != nil {
				return classifyAPIError("Reading the regions", err)
			}
			if resp.JSON200 == nil {
				return apiErr("Reading the regions", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			var out []fbapi.RegionBody
			if resp.JSON200.Regions != nil {
				out = *resp.JSON200.Regions
			}
			return env.Printer.Print(out, func() Table {
				t := Table{Headers: []string{"slug", "name", "country", "sells"}, Empty: "No active regions."}
				for _, r := range out {
					slugs := "-"
					if r.AvailableSizeSlugs != nil && len(*r.AvailableSizeSlugs) > 0 {
						slugs = strings.Join(*r.AvailableSizeSlugs, ", ")
					}
					t.Rows = append(t.Rows, []string{r.Slug, r.Name, r.CountryCode, slugs})
				}
				return t
			})
		},
	}

	images := &cobra.Command{
		Use:   "images",
		Short: "Images that can be installed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			resp, err := env.Client.API.ImagesListWithResponse(ctx, &fbapi.ImagesListParams{})
			if err != nil {
				return classifyAPIError("Reading the images", err)
			}
			if resp.JSON200 == nil {
				return apiErr("Reading the images", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			var out []fbapi.ImageBody
			if resp.JSON200.Images != nil {
				out = *resp.JSON200.Images
			}
			return env.Printer.Print(out, func() Table {
				t := Table{Headers: []string{"slug", "name", "kind", "min disk"}, Empty: "No images."}
				for _, i := range out {
					t.Rows = append(t.Rows, []string{i.Slug, i.Name, i.Kind, fmt.Sprintf("%d GB", i.MinDiskGb)})
				}
				return t
			})
		},
	}

	cmd.AddCommand(plans, regions, images)
	return cmd
}

// ---------------------------------------------------------------- database

func databaseCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "db", Aliases: []string{"database", "databases"}, Short: "Managed databases"}

	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List database instances", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			var out []fbapi.DatabaseBody
			for db, err := range env.Client.Databases(ctx) {
				if err != nil {
					return classifyAPIError("Listing databases", err)
				}
				out = append(out, db)
			}
			return env.Printer.Print(out, func() Table {
				t := Table{Headers: []string{"name", "code", "state", "engine", "plan", "public"}, Empty: "No managed databases."}
				for _, d := range out {
					t.Rows = append(t.Rows, []string{
						d.Name, d.Code, env.Printer.StateColor("database", string(d.State)),
						fmt.Sprintf("%s %s", d.Engine, d.EngineVersion),
						deref(d.PlanSlug), fmt.Sprint(d.PublicAccess),
					})
				}
				return t
			})
		},
	}
	get := &cobra.Command{
		Use: "get <code|id>", Short: "Show one database instance", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			resp, err := env.Client.API.DatabaseGetWithResponse(ctx, args[0])
			if err != nil {
				return classifyAPIError("Reading the database", err)
			}
			if resp.JSON200 == nil {
				return apiErr("Reading the database", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			d := resp.JSON200.Database
			return env.Printer.Detail(resp.JSON200, func() [][2]string {
				rows := [][2]string{
					{"name", d.Name}, {"code", d.Code}, {"id", d.Id},
					{"state", env.Printer.StateColor("database", string(d.State))},
					{"engine", fmt.Sprintf("%s %s", d.Engine, d.EngineVersion)},
					{"plan", deref(d.PlanSlug)}, {"region", deref(d.RegionSlug)},
					{"public access", fmt.Sprint(d.PublicAccess)}, {"ip", deref(d.Ip)},
					{"pending apply", fmt.Sprint(d.PendingApply)},
				}
				if resp.JSON200.TrustedSources != nil && len(*resp.JSON200.TrustedSources) > 0 {
					var src []string
					for _, s := range *resp.JSON200.TrustedSources {
						src = append(src, s.Cidr)
					}
					rows = append(rows, [2]string{"trusted sources", strings.Join(src, ", ")})
				}
				// Connection credentials are closed to API tokens, so this says
				// where they are rather than leaving the reader hunting.
				rows = append(rows, [2]string{"credentials", "not available to an API token; read them in the panel"})
				return rows
			})
		},
	}
	cmd.AddCommand(list, get, deleteCmd(env, deleteSpec{
		kind: "database", operation: "databaseDelete",
		extra: "The data is not recoverable.",
		resolve: func(e *Env, c *cobra.Command, ref string) (string, string, error) {
			resp, err := e.Client.API.DatabaseGetWithResponse(c.Context(), ref)
			if err != nil || resp.JSON200 == nil {
				return "", "", notFound("database", ref)
			}
			return resp.JSON200.Database.Id, resp.JSON200.Database.Name, nil
		},
		remove: func(e *Env, c *cobra.Command, id string) error {
			resp, err := e.Client.API.DatabaseDeleteWithResponse(c.Context(), id)
			if err != nil {
				return err
			}
			if resp.StatusCode() >= 400 {
				return apiErr("Deleting", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			return nil
		},
	}))
	return cmd
}

// ---------------------------------------------------------------- volume

func volumeCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "volume", Aliases: []string{"volumes", "vol"}, Short: "Block storage volumes"}
	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List volumes", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			var out []fbapi.VolumeBody
			for v, err := range env.Client.Volumes(ctx) {
				if err != nil {
					return classifyAPIError("Listing volumes", err)
				}
				out = append(out, v)
			}
			return env.Printer.Print(out, func() Table {
				t := Table{Headers: []string{"name", "id", "state", "size", "attached to"}, Empty: "No volumes."}
				for _, v := range out {
					attached := env.Printer.Warn("detached (still billed)")
					if v.ServerId != nil && *v.ServerId != "" {
						attached = *v.ServerId
					}
					t.Rows = append(t.Rows, []string{
						v.Name, v.Id, env.Printer.StateColor("volume", string(v.State)),
						fmt.Sprintf("%d GB", v.SizeGb), attached,
					})
				}
				return t
			})
		},
	}
	cmd.AddCommand(list, deleteCmd(env, deleteSpec{
		kind: "volume", operation: "volumeDelete",
		extra: "The data on it is not recoverable, and volumes are excluded from server backups.",
		resolve: func(e *Env, c *cobra.Command, ref string) (string, string, error) {
			resp, err := e.Client.API.VolumeGetWithResponse(c.Context(), ref)
			if err != nil || resp.JSON200 == nil {
				return "", "", notFound("volume", ref)
			}
			return resp.JSON200.Id, resp.JSON200.Name, nil
		},
		remove: func(e *Env, c *cobra.Command, id string) error {
			resp, err := e.Client.API.VolumeDeleteWithResponse(c.Context(), id)
			if err != nil {
				return err
			}
			if resp.StatusCode() >= 400 {
				return apiErr("Deleting", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			return nil
		},
	}))
	return cmd
}

// ---------------------------------------------------------------- networking

func networkCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "network", Aliases: []string{"networks", "net"}, Short: "Private networks"}
	cmd.AddCommand(&cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List private networks", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			var out []fbapi.NetworkBody
			for n, err := range env.Client.Networks(ctx) {
				if err != nil {
					return classifyAPIError("Listing networks", err)
				}
				out = append(out, n)
			}
			return env.Printer.Print(out, func() Table {
				t := Table{Headers: []string{"name", "id", "state", "cidr"}, Empty: "No private networks."}
				for _, n := range out {
					t.Rows = append(t.Rows, []string{n.Name, n.Id,
						env.Printer.StateColor("network", string(n.State)), n.Cidr})
				}
				return t
			})
		},
	})
	return cmd
}

func firewallCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "fw", Aliases: []string{"firewall", "firewalls"}, Short: "Firewalls"}
	cmd.AddCommand(&cobra.Command{
		Use: "get <id>", Short: "Show a firewall and its rules", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			resp, err := env.Client.API.FirewallGetWithResponse(ctx, args[0])
			if err != nil {
				return classifyAPIError("Reading the firewall", err)
			}
			if resp.JSON200 == nil {
				return apiErr("Reading the firewall", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			return env.Printer.Detail(resp.JSON200, func() [][2]string {
				rows := [][2]string{
					{"name", resp.JSON200.Firewall.Name},
					{"id", resp.JSON200.Firewall.Id},
				}
				if resp.JSON200.Rules != nil {
					for _, r := range *resp.JSON200.Rules {
						ports := "-"
						if r.PortFrom != nil {
							ports = fmt.Sprint(*r.PortFrom)
							if r.PortTo != nil && *r.PortTo != *r.PortFrom {
								ports += "-" + fmt.Sprint(*r.PortTo)
							}
						}
						rows = append(rows, [2]string{"rule",
							fmt.Sprintf("%s %s %s %s", r.Direction, r.Protocol, ports, r.Source)})
					}
				}
				if resp.JSON200.Servers != nil {
					rows = append(rows, [2]string{"attached to", fmt.Sprintf("%d server(s)", len(*resp.JSON200.Servers))})
				}
				return rows
			})
		},
	})
	return cmd
}

func loadBalancerCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "lb", Aliases: []string{"load-balancer", "load-balancers"}, Short: "Load balancers"}
	cmd.AddCommand(&cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List load balancers", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			var out []fbapi.LoadBalancerBody
			for lb, err := range env.Client.LoadBalancers(ctx) {
				if err != nil {
					return classifyAPIError("Listing load balancers", err)
				}
				out = append(out, lb)
			}
			return env.Printer.Print(out, func() Table {
				t := Table{Headers: []string{"name", "id", "state", "ip", "healthy", "rules"}, Empty: "No load balancers."}
				for _, lb := range out {
					t.Rows = append(t.Rows, []string{
						lb.Name, lb.Id, env.Printer.StateColor("load_balancer", string(lb.State)),
						deref(lb.Ip), fmt.Sprintf("%d/%d", lb.HealthyCount, lb.BackendCount),
						fmt.Sprint(lb.RuleCount),
					})
				}
				return t
			})
		},
	})
	return cmd
}

func floatingIPCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "fip", Aliases: []string{"floating-ip", "floating-ips"}, Short: "Floating IPs"}
	cmd.AddCommand(&cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List floating IPs", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			resp, err := env.Client.API.FloatingIpsListWithResponse(ctx)
			if err != nil {
				return classifyAPIError("Listing floating IPs", err)
			}
			if resp.JSON200 == nil {
				return apiErr("Listing floating IPs", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			var out []fbapi.FloatingIPBody
			if resp.JSON200.FloatingIps != nil {
				out = *resp.JSON200.FloatingIps
			}
			return env.Printer.Print(out, func() Table {
				t := Table{Headers: []string{"address", "id", "state", "attached to"}, Empty: "No floating IPs."}
				for _, ip := range out {
					where := env.Printer.Warn("unattached (still billed)")
					if ip.Server != nil {
						where = deref(ip.Server.Name)
					}
					t.Rows = append(t.Rows, []string{ip.Address, ip.Id, string(ip.State), where})
				}
				return t
			})
		},
	})
	return cmd
}

// ---------------------------------------------------------------- dns

func dnsCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "dns", Short: "DNS zones and records"}

	zones := &cobra.Command{
		Use: "zones", Aliases: []string{"zone"}, Short: "List DNS zones", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			resp, err := env.Client.API.DnsZonesListWithResponse(ctx, &fbapi.DnsZonesListParams{})
			if err != nil {
				return classifyAPIError("Listing DNS zones", err)
			}
			if resp.JSON200 == nil {
				return apiErr("Listing DNS zones", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			var out []fbapi.DnsZoneBody
			if resp.JSON200.Zones != nil {
				out = *resp.JSON200.Zones
			}
			return env.Printer.Print(out, func() Table {
				t := Table{Headers: []string{"name", "id"}, Empty: "No DNS zones."}
				for _, z := range out {
					t.Rows = append(t.Rows, []string{z.Name, z.Id})
				}
				return t
			})
		},
	}

	records := &cobra.Command{
		Use: "records <zone-id>", Aliases: []string{"record"}, Short: "List a zone's records", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			resp, err := env.Client.API.DnsZoneGetWithResponse(ctx, args[0])
			if err != nil {
				return classifyAPIError("Reading the zone", err)
			}
			if resp.JSON200 == nil {
				return apiErr("Reading the zone", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			var out []fbapi.DnsRecordBody
			if resp.JSON200.Records != nil {
				out = *resp.JSON200.Records
			}
			return env.Printer.Print(out, func() Table {
				t := Table{Headers: []string{"name", "type", "content", "ttl", "id"}, Empty: "No records in this zone."}
				for _, r := range out {
					t.Rows = append(t.Rows, []string{r.Name, r.Type, r.Content, fmt.Sprint(r.Ttl), r.Id})
				}
				return t
			})
		},
	}

	cmd.AddCommand(zones, records)
	return cmd
}

// ---------------------------------------------------------------- domain

func domainCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "domain", Aliases: []string{"domains"}, Short: "Domain registrations"}
	cmd.AddCommand(&cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List domains", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			var out []fbapi.DomainBody
			for d, err := range env.Client.Domains(ctx) {
				if err != nil {
					return classifyAPIError("Listing domains", err)
				}
				out = append(out, d)
			}
			return env.Printer.Print(out, func() Table {
				t := Table{Headers: []string{"name", "state", "expires", "auto-renew", "id"}, Empty: "No domain registrations."}
				for _, d := range out {
					expires := "-"
					if d.ExpiresAt != nil {
						expires = d.ExpiresAt.Format("2006-01-02")
						if d.Expiring {
							expires = env.Printer.Warn(expires + " (soon)")
						}
					}
					t.Rows = append(t.Rows, []string{
						d.Name, env.Printer.StateColor("domain", string(d.State)),
						expires, fmt.Sprint(d.AutoRenew), d.Id,
					})
				}
				return t
			})
		},
	})
	return cmd
}

// ---------------------------------------------------------------- ssh keys, projects, isos

func sshKeyCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "ssh-key", Aliases: []string{"ssh-keys", "key"}, Short: "SSH keys"}
	cmd.AddCommand(&cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List SSH keys", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			resp, err := env.Client.API.SshKeysListWithResponse(ctx)
			if err != nil {
				return classifyAPIError("Listing SSH keys", err)
			}
			if resp.JSON200 == nil {
				return apiErr("Listing SSH keys", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			var out []fbapi.SshKeyBody
			if resp.JSON200.Keys != nil {
				out = *resp.JSON200.Keys
			}
			return env.Printer.Print(out, func() Table {
				t := Table{Headers: []string{"name", "id", "fingerprint"}, Empty: "No SSH keys."}
				for _, k := range out {
					t.Rows = append(t.Rows, []string{k.Name, k.Id, k.Fingerprint})
				}
				return t
			})
		},
	}, deleteCmd(env, deleteSpec{
		kind: "ssh key", operation: "sshKeyDelete",
		extra: "Servers already built with it keep working: the key was injected at first boot.",
		resolve: func(e *Env, c *cobra.Command, ref string) (string, string, error) {
			resp, err := e.Client.API.SshKeyGetWithResponse(c.Context(), ref)
			if err != nil || resp.JSON200 == nil {
				return "", "", notFound("ssh key", ref)
			}
			return resp.JSON200.Id, resp.JSON200.Name, nil
		},
		remove: func(e *Env, c *cobra.Command, id string) error {
			resp, err := e.Client.API.SshKeyDeleteWithResponse(c.Context(), id)
			if err != nil {
				return err
			}
			if resp.StatusCode() >= 400 {
				return apiErr("Deleting", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			return nil
		},
	}))
	return cmd
}

func projectCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "project", Aliases: []string{"projects"}, Short: "Projects"}
	cmd.AddCommand(&cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List projects", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			resp, err := env.Client.API.ProjectsListWithResponse(ctx)
			if err != nil {
				return classifyAPIError("Listing projects", err)
			}
			if resp.JSON200 == nil {
				return apiErr("Listing projects", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			var out []fbapi.ProjectBody
			if resp.JSON200.Projects != nil {
				out = *resp.JSON200.Projects
			}
			return env.Printer.Print(out, func() Table {
				t := Table{Headers: []string{"name", "id"}, Empty: "No projects."}
				for _, p := range out {
					t.Rows = append(t.Rows, []string{p.Name, p.Id})
				}
				return t
			})
		},
	})
	return cmd
}

func isoCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "iso", Aliases: []string{"isos"}, Short: "Custom ISOs"}
	cmd.AddCommand(&cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List custom ISOs", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			resp, err := env.Client.API.IsoListWithResponse(ctx)
			if err != nil {
				return classifyAPIError("Listing ISOs", err)
			}
			if resp.JSON200 == nil {
				return apiErr("Listing ISOs", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			var out []fbapi.IsoBody
			if resp.JSON200.Isos != nil {
				out = *resp.JSON200.Isos
			}
			return env.Printer.Print(out, func() Table {
				t := Table{Headers: []string{"name", "id", "status", "size"}, Empty: "No custom ISOs."}
				for _, i := range out {
					t.Rows = append(t.Rows, []string{
						i.Name, i.Id, env.Printer.StateColor("iso", string(i.Status)),
						fmt.Sprintf("%d MB", i.SizeBytes/(1<<20)),
					})
				}
				return t
			})
		},
	})
	return cmd
}

// ---------------------------------------------------------------- wallet, account

func walletCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "wallet", Short: "Balance and spending"}
	cmd.AddCommand(&cobra.Command{
		Use: "balance", Short: "Balance, burn rate and runway", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			resp, err := env.Client.API.WalletGetWithResponse(ctx)
			if err != nil {
				return classifyAPIError("Reading the wallet", err)
			}
			if resp.JSON200 == nil {
				return apiErr("Reading the wallet", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			w := resp.JSON200
			return env.Printer.Detail(w, func() [][2]string {
				rows := [][2]string{
					{"balance", money(w.BalanceMinor, w.Currency)},
					{"burn", money(w.BurnPerHourMinor, w.Currency) + " per hour"},
				}
				if w.RunwayHours != nil && *w.RunwayHours > 0 {
					rows = append(rows, [2]string{"runway",
						fmt.Sprintf("about %d hours (%.1f days)", *w.RunwayHours, float64(*w.RunwayHours)/24)})
				}
				if w.NegativeSince != nil {
					rows = append(rows, [2]string{"negative since",
						env.Printer.Bad(ago(w.NegativeSince) + " — servers are suspended after a grace period")})
				}
				return rows
			})
		},
	})
	return cmd
}

func accountCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{Use: "account", Short: "Quotas and available products"}
	cmd.AddCommand(&cobra.Command{
		Use: "limits", Short: "Quotas with what is left, and which products are on", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := env.need(); err != nil {
				return err
			}
			ctx, cancel := env.ctxWithTimeout(cmd.Context())
			defer cancel()
			resp, err := env.Client.API.AccountLimitsWithResponse(ctx)
			if err != nil {
				return classifyAPIError("Reading the limits", err)
			}
			if resp.JSON200 == nil {
				return apiErr("Reading the limits", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.HTTPResponse)
			}
			var out []fbapi.CustomerQuotaBody
			if resp.JSON200.Quotas != nil {
				out = *resp.JSON200.Quotas
			}
			return env.Printer.Print(out, func() Table {
				t := Table{Headers: []string{"quota", "used", "limit", "left", "unit"}, Empty: "No quotas published for this account."}
				for _, q := range out {
					left := fmt.Sprint(q.Remaining)
					if q.Remaining <= 0 {
						left = env.Printer.Bad(left + " (at the ceiling)")
					}
					t.Rows = append(t.Rows, []string{q.Label, fmt.Sprint(q.Used), fmt.Sprint(q.Limit), left, q.Unit})
				}
				return t
			})
		},
	})
	return cmd
}

var _ = firstboot.EnvToken
