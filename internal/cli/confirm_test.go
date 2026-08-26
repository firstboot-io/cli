package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The gate that keeps the confirmation list honest.
//
// `destroyScoped` is a copy of the platform's `destroy` scope set, and a copy
// drifts. It drifts SILENTLY, which is the problem: nothing about a missing
// confirmation prompt looks broken, and the way it is discovered is somebody
// deleting a machine they were not asked about.
//
// So the copy is held to the published table. The platform stamps
// `x-firstboot-scope` on every operation from the same map its middleware
// consults, and this reads it. If the API makes something irreversible that used
// to be reversible, the build breaks with the operation named.

func loadScopes(t *testing.T) map[string]string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "platform", "api", "openapi", "openapi.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("the platform checkout is not beside this one (%v); "+
			"CI checks out both for exactly that reason", err)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Scope       string `json:"x-firstboot-scope"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	out := map[string]string{}
	for _, item := range spec.Paths {
		for _, op := range item {
			if op.OperationID != "" && op.Scope != "" {
				out[op.OperationID] = op.Scope
			}
		}
	}
	if len(out) < 100 {
		t.Fatalf("only %d scoped operations parsed; the spec's shape changed", len(out))
	}
	return out
}

// The set this CLI confirms has to be exactly the set the API calls
// irreversible. Both directions matter: a missing entry is a delete nobody was
// asked about, and an extra one is a prompt in front of something that did not
// need it, which teaches people to reach for --yes.
func TestConfirmationSetMatchesTheDestroyScope(t *testing.T) {
	scopes := loadScopes(t)

	var missing, extra []string
	for id, scope := range scopes {
		if scope == "destroy" && !destroyScoped[id] {
			missing = append(missing, id)
		}
	}
	for id := range destroyScoped {
		scope, known := scopes[id]
		switch {
		case !known:
			extra = append(extra, id+" (not an operation in the spec)")
		case scope != "destroy":
			extra = append(extra, id+" (the API scopes it "+scope+")")
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		t.Errorf("the API calls these irreversible and this CLI would not confirm them:\n  %s\n"+
			"  Add each to destroyScoped in confirm.go.", strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("destroyScoped names operations the API does not scope `destroy`:\n  %s\n"+
			"  A prompt in front of something safe teaches people to reach for --yes.",
			strings.Join(extra, "\n  "))
	}
}

// Every command that declares a Danger has to name an operation the spec knows.
// A typo there disables the prompt silently, because an unknown operation is not
// `destroy`-scoped.
func TestDangerOperationsExist(t *testing.T) {
	scopes := loadScopes(t)
	// The operations the commands pass to Confirm. Listed rather than reflected
	// out of the command tree, because the point is that somebody wrote them
	// down and a reviewer can see all of them at once.
	declared := []string{
		"serverDelete",
		"databaseDelete",
		"volumeDelete",
		"sshKeyDelete",
	}
	for _, id := range declared {
		if _, ok := scopes[id]; !ok {
			t.Errorf("a command confirms on %q, which is not an operation in the spec", id)
			continue
		}
		if !NeedsConfirmation(id) {
			t.Errorf("a command confirms on %q, but NeedsConfirmation says it does not need one", id)
		}
	}
}

// A profile name reaches the filesystem, so the bound on it is a real check
// rather than a style rule: `../../.ssh/id_rsa` as a profile name would write a
// token over somebody's key.
func TestProfileNamesCannotEscapeTheConfigDirectory(t *testing.T) {
	for _, bad := range []string{
		"../escape", "a/b", "..", "", strings.Repeat("x", 65), "with space", "dot.dot",
	} {
		if validProfileName(bad) {
			t.Errorf("%q was accepted as a profile name", bad)
		}
	}
	for _, good := range []string{"default", "work", "ci-prod", "a_b", "x1"} {
		if !validProfileName(good) {
			t.Errorf("%q was rejected as a profile name", good)
		}
	}
}

// The exit codes are a contract with somebody's shell script. Changing what one
// MEANS is breaking, so this pins the mapping that matters most: the difference
// between "retry this" and "never retry this".
func TestExitCodesSeparateRetryableFromFinal(t *testing.T) {
	for _, c := range []struct {
		code string
		want int
	}{
		{"NO_CAPACITY_IN_REGION", ExitTemporary},
		{"CREATE_COOLDOWN", ExitTemporary},
		{"PLAN_NOT_OFFERED_IN_REGION", ExitRefused},
		{"INSUFFICIENT_BALANCE", ExitRefused},
		{"QUOTA_EXCEEDED", ExitRefused},
		{"TOKEN_SCOPE_FORBIDDEN", ExitForbidden},
		{"TOKEN_OPERATION_FORBIDDEN", ExitForbidden},
	} {
		got, hint := adviceFor(apiError(c.code, 0))
		if got != c.want {
			t.Errorf("%s exits %d, want %d", c.code, got, c.want)
		}
		if hint == "" {
			t.Errorf("%s has no advice; the code alone does not say what to do", c.code)
		}
	}
}
