package cli

import (
	"errors"
	"fmt"
	"net/http"

	firstboot "github.com/firstboot-io/go-sdk"
)

// Exit codes, because a CLI is also a program.
//
// Half the reason this exists is to be called from a script, and a script's only
// way to branch is the exit code. One code for every failure makes `||` useless:
// "the region is full, try again in a minute" and "that plan does not exist" are
// the same shell condition, and the first is worth retrying while the second is
// never worth retrying.
//
// The codes are stable and documented. Adding one is fine; changing what one
// means is a breaking change for somebody's pipeline.
const (
	// ExitOK is success.
	ExitOK = 0
	// ExitError is anything with no better code.
	ExitError = 1
	// ExitUsage is a bad invocation: an unknown flag, a missing argument.
	ExitUsage = 2
	// ExitAuth is 401. The token is wrong, revoked or expired.
	ExitAuth = 3
	// ExitForbidden is 403. The token is real and this is not open to it, which
	// almost always means its scopes.
	ExitForbidden = 4
	// ExitNotFound is 404.
	ExitNotFound = 5
	// ExitConflict is a state conflict: the resource is busy, or already in the
	// state asked for. Retrying the same call rarely helps.
	ExitConflict = 6
	// ExitRefused is a refusal that waiting cannot fix: not enough balance, a
	// plan a region does not sell, a quota at its ceiling. Never retry.
	ExitRefused = 7
	// ExitTemporary is a refusal that waiting CAN fix: no capacity, a rate
	// ceiling, a 5xx. This is the one a retry loop should look for.
	ExitTemporary = 8
	// ExitTimeout is a --wait that ran out of budget. The work is still
	// running; the command stopped watching.
	ExitTimeout = 9
	// ExitCancelled is Ctrl-C.
	ExitCancelled = 130
)

// exitError carries a code out of a command to main.
type exitError struct {
	code int
	err  error
	// hint is the sentence after the error: what to do about it. Separate from
	// the error so the error stays quotable and the advice stays ours.
	hint string
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// fail wraps an error with an exit code and advice.
func fail(code int, hint string, err error) error {
	return &exitError{code: code, err: err, hint: hint}
}

// failf is fail for an error this CLI is raising itself.
func failf(code int, hint, format string, a ...any) error {
	return &exitError{code: code, err: fmt.Errorf(format, a...), hint: hint}
}

// classifyAPIError turns an API refusal into a code and a sentence.
//
// The codes the API publishes are the contract, and the difference between two
// of them is the whole value here: NO_CAPACITY means try another region,
// PLAN_NOT_OFFERED means that region will never have it, INSUFFICIENT_BALANCE
// means stop and tell somebody. A CLI that printed "HTTP 503" for all three
// would have thrown that away.
func classifyAPIError(action string, err error) error {
	if err == nil {
		return nil
	}

	var se *firstboot.StateError
	if errors.As(err, &se) {
		hint := "The resource EXISTS; it is the work that failed. Look at it before " +
			"creating another one."
		if se.Code != "" {
			hint = "Error code: " + se.Code + ". " + hint
		}
		return fail(ExitError, hint,
			fmt.Errorf("%s: the %s settled into %q", action, se.Kind, se.State))
	}

	var te *firstboot.TimeoutError
	if errors.As(err, &te) {
		return fail(ExitTimeout,
			"The resource was created and is still converging. Read it again in a "+
				"minute rather than creating a second one, or raise --timeout.",
			fmt.Errorf("%s: gave up after %s, last state %q", action, te.Waited, te.LastState))
	}

	var ae *firstboot.APIError
	if !errors.As(err, &ae) {
		return fail(ExitError, "", fmt.Errorf("%s: %w", action, err))
	}

	code, hint := adviceFor(ae)
	return fail(code, hint, fmt.Errorf("%s: %s", action, message(ae)))
}

func message(ae *firstboot.APIError) string {
	if ae.Detail != "" {
		return ae.Detail
	}
	if ae.Title != "" {
		return ae.Title
	}
	return fmt.Sprintf("HTTP %d", ae.Status)
}

func adviceFor(ae *firstboot.APIError) (int, string) {
	switch ae.Code {
	case "NO_CAPACITY_IN_REGION":
		return ExitTemporary, "Every host in that region that sells this plan is full. " +
			"This is temporary: try another region, or the same one later."
	case "PLAN_NOT_OFFERED_IN_REGION":
		return ExitRefused, "No host in that region carries this plan, so waiting will never " +
			"help. `firstboot catalog plans --region <slug>` shows what it does sell."
	case "INSUFFICIENT_BALANCE":
		return ExitRefused, "The first month is charged upfront and the wallet cannot cover it. " +
			"Nothing was created. Top up and run it again."
	case "CREATE_COOLDOWN":
		return ExitTemporary, "The account's create rate ceiling was reached. Wait and retry; " +
			"for a job that legitimately builds fleets, ask support to raise " +
			"`servers.create_rate` for the account."
	case "QUOTA_EXCEEDED":
		return ExitRefused, "An account quota is in the way. `firstboot account limits` shows " +
			"which one and what is left."
	case "ORGANIZATION_SUSPENDED":
		return ExitRefused, "The account is suspended, so nothing new is provisioned. " +
			"Existing resources are unaffected."
	case "TOKEN_SCOPE_FORBIDDEN":
		return ExitForbidden, "This profile's token does not have the scope this command needs.\n" +
			"A token's scopes are fixed when it is created, so this needs a NEW token from " +
			"the panel rather than a retry.\n" +
			"Each endpoint's required scope is in the API reference."
	case "TOKEN_OPERATION_FORBIDDEN":
		return ExitForbidden, "This operation is closed to every API token whatever its scopes. " +
			"Credentials, consoles, shells, account settings and billing stay in the panel."
	case "IDEMPOTENCY_KEY_REUSED":
		return ExitConflict, "The same idempotency key reached the API with a different body. " +
			"This is a bug in this CLI; please report it."
	}

	switch ae.Status {
	case http.StatusUnauthorized:
		return ExitAuth, "The token is invalid, revoked or expired. " +
			"Run `firstboot auth login` to replace it."
	case http.StatusForbidden:
		return ExitForbidden, "Your role in the organization, or the token's scopes, do not " +
			"cover this."
	case http.StatusNotFound:
		return ExitNotFound, ""
	case http.StatusConflict:
		return ExitConflict, "The resource is busy or already in the state asked for. " +
			"Read it and decide, rather than repeating the call."
	case http.StatusTooManyRequests:
		return ExitTemporary, "Rate limited. The CLI already honoured the server's Retry-After " +
			"up to its budget."
	}
	if ae.Status >= 500 {
		return ExitTemporary, "The API answered with a server error. This is usually temporary."
	}
	return ExitError, ""
}

// notFound is the shape every `get` uses when a name resolves to nothing. It is
// separate from the API's 404 because the CLI resolves names locally: "no server
// called web-1" is a better sentence than the 404 the API would give for a
// UUID that was never sent.
func notFound(kind, name string) error {
	return failf(ExitNotFound,
		fmt.Sprintf("`firstboot %s list` shows what there is.", kind),
		"no %s called %q", kind, name)
}

// apiError builds an APIError for the tests, so the code/advice mapping can be
// exercised without a live API.
func apiError(code string, status int) *firstboot.APIError {
	return &firstboot.APIError{Code: code, Status: status, Detail: code}
}
