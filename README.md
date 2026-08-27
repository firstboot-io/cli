# firstboot

The command-line interface for the Firstboot cloud platform.

```
brew install firstboot-io/tap/firstboot
```

or a `.deb`, `.rpm` or a static binary from the releases page, or from source:

```
go install github.com/firstboot-io/cli/cmd/firstboot@latest
```

The command lives under `cmd/firstboot` for that last line's sake. A `main` at
the repository root would install as `cli`, because `go install` names the binary
after the directory it found it in and this repository is named for what it
holds rather than for what it produces.

```
firstboot auth login
firstboot server list
firstboot server ssh web-1
```

## Why it exists

Not to repeat the panel. Three things the panel cannot do:

**Bulk work.** Thirty servers is thirty clicks in a browser and one command
here.

```
firstboot server power stop web-1 web-2 web-3
firstboot server list --state other
```

Tags are what make bulk work address a SET rather than a list of names. Every
list command takes `--tag`, repeated to narrow:

```
firstboot server list --tag env:prod --tag role:web
firstboot tag set server web-1 env:prod role:web
firstboot tag list
```

`tag set` REPLACES the whole set, which is what the endpoint does; listing no
tags clears them.

**Piping.** `--output json` hands the API's own body to jq, a script or a CI
step. It is the API's body rather than a re-shaped one on purpose: a second
schema is a second thing to drift.

```
firstboot server list -o json | jq -r '.[] | select(.state=="running") | .ip'
```

**Staying in the terminal.** Looking up an IP in a browser to paste into a shell
is a round trip for something the terminal already knows.

```
firstboot server ssh web-1 -- -p 2222
firstboot app logs api --follow
```

## A profile is a token is an organization

An API token is pinned to one organization for its whole life. So there is no
`--org` flag and there must never be one: an organization is not something a
command chooses, it is a property of the credential. Two organizations means two
profiles.

```
firstboot auth login --profile work
firstboot auth login --profile personal
firstboot -p personal server list
```

`FIRSTBOOT_TOKEN` overrides every profile, which is the CI case.

## Where the token goes

Not in a dotfile. The operating system's own secret store: the Keychain on
macOS, the Secret Service on Linux, the Credential Manager on Windows.

Where there is none — a container, a headless runner — it goes to a file with
mode 0600 and **login says so**. Silently degrading to a plaintext file is how
somebody ends up believing their token is in the Keychain when it is in their
home directory.

In CI, set `FIRSTBOOT_TOKEN` from the runner's own secret store and it never
touches the disk.

## Destructive commands ask, and the list is not ours

`firstboot server delete` asks you to type the server's name back. `--yes` skips
it; a **non-interactive run without `--yes` is refused** rather than assumed,
because a script that pipes nothing to stdin has not consented to a delete.

Which commands need that is not decided here. The API already decided which
operations cannot be fixed by pressing the button again — they are the ones that
cost the `destroy` scope — and it publishes that on every endpoint. This CLI
copies the set, and `confirm_test.go` reads the platform's own spec and breaks
the build if the two disagree in either direction. A hand-written list of
dangerous commands drifts the first time the API makes something irreversible,
and drifts silently: nothing about a missing prompt looks broken.

## Exit codes

A CLI is also a program, and a program's only way to branch is the exit code.
One code for every failure makes `||` useless: "the region is full, try again"
and "that plan does not exist there" are the same shell condition, and only the
first is worth retrying.

| Code | Meaning |
| --- | --- |
| 0 | success |
| 1 | an error with no better code |
| 2 | bad invocation: unknown flag, missing argument |
| 3 | not logged in, or the token is invalid |
| 4 | forbidden: the token's scopes or your role |
| 5 | not found |
| 6 | state conflict; the resource is busy |
| 7 | **refused, and waiting will not help**: balance, quota, a plan a region does not sell |
| 8 | **temporary; retrying can work**: no capacity, a rate ceiling, a 5xx |
| 9 | a `--wait` ran out of budget; the work is still running |
| 130 | cancelled |

```bash
firstboot server create web-9 --plan s1 --image ubuntu-24-04
case $? in
  0) echo up ;;
  8) echo "retry later" ;;
  7) echo "never going to work; tell a human" ; exit 1 ;;
esac
```

## Global flags

| Flag | |
| --- | --- |
| `-o, --output` | `table` (default), `json`, `yaml` |
| `-p, --profile` | which account |
| `--no-wait` | return as soon as the work is accepted |
| `--timeout` | how long to wait |
| `-y, --yes` | skip the confirmation on destructive commands |
| `--no-color` | never colourise; also honours `NO_COLOR` and a non-terminal stdout |
| `--endpoint` | API base URL, overriding the profile's |

## What it deliberately does not do

- **No `create` for load balancers, databases or firewalls.** A resource whose
  shape needs six repeated flags is better made in the panel or in Terraform.
  The CLI's reasons to exist are bulk reading, piping and staying in the
  terminal, and a flag soup is none of them.
- **No DNS record deletion.** Removing DNS is how a site goes dark. The panel
  has it.
- **No password reset.** It returns the new root password in the response body,
  which in a terminal means shell history and, in CI, a log file.
- **No key material.** `server ssh` hands over to YOUR ssh with YOUR keys and
  does not touch `known_hosts`; a host key change is ssh's warning to give, not
  ours to suppress.
- **No credential reads, shells or consoles.** Those endpoints are closed to
  every API token, so this is not a policy here so much as a fact.

## Development

```
go build ./...
go test ./...
```

The SDK is consumed as a released module (`github.com/firstboot-io/go-sdk`),
not as a sibling checkout: the `replace` directive is gone. While that
repository is private the module proxy cannot serve it, so a build needs

```
go env -w GOPRIVATE='github.com/firstboot-io/*'
git config --global url."git@github.com:firstboot-io/".insteadOf "https://github.com/firstboot-io/"
```

once, and nothing after the repositories go public.

`confirm_test.go` reads `../platform/api/openapi/openapi.json`. Without the
platform checked out beside this repository that test SKIPS, which is why CI
checks it out and then fails if the skip happened.

## Releasing

```
git tag -a v0.1.0 -m v0.1.0 && git push origin v0.1.0
```

`.github/workflows/release.yml` runs goreleaser on the tag and produces every
way to install it at once: archives for five targets, a `.deb` and a `.rpm`, and
a formula pushed to `firstboot-io/homebrew-tap`.

Two secrets, both one-time. `HOMEBREW_TAP_TOKEN` needs write access to the tap
repository, because the default `GITHUB_TOKEN` is scoped to this one and cannot
push a formula anywhere else. `SDK_READ_TOKEN` needs read access to
`firstboot-io/go-sdk` while that repository is private, and becomes unnecessary
when it is not.

The tap has to be public before `brew install firstboot-io/tap/firstboot` works
for anybody: Homebrew clones it anonymously.

## Requirements

Go 1.25 or newer to build. The released binaries are static and need nothing.

## License

Apache License 2.0. See [LICENSE](LICENSE).
