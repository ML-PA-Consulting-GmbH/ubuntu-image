package commands

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// The single place that reads `m2cp user status --json`. Both the
// preflight (does the operator have a session at all?) and the build
// pipeline (which appstore do we point snapd at?) need this, and the
// output format has already changed once under us, so it lives here
// rather than being parsed twice with two different levels of
// tolerance.

// M2cpUserStatus is the slim view of `m2cp user status --json` the
// builder needs: whether a session is active, which tenant it belongs
// to, and the store's GraphQL endpoint.
type M2cpUserStatus struct {
	LoggedIn bool
	Tenant   string
	// StoreURL is the GraphQL endpoint, e.g.
	// https://host/graphql -- not the snap-store base.
	StoreURL string
}

// m2cpStatusBody is the subset of the status payload we read. Both
// known formats agree on these field names; they differ only in
// whether the payload sits at the top level or under "output" (see
// parseM2cpUserStatus). The flat `tenant-alias` / `tenant-name` keys
// sit alongside the nested `tenant` object in the current format and
// serve as fallbacks.
type m2cpStatusBody struct {
	Status  string `json:"status"`
	Session struct {
		Store  string `json:"store"`
		Tenant struct {
			TenantName string `json:"tenantName"`
			Alias      string `json:"alias"`
		} `json:"tenant"`
		TenantAlias string `json:"tenant-alias"`
		TenantName  string `json:"tenant-name"`
	} `json:"session"`
}

// M2cpSessionStatus runs `m2cp user status --json` and returns the
// parsed session state.
func M2cpSessionStatus() (M2cpUserStatus, error) {
	cmd := exec.Command(M2cpCLI, "user", "status", "--json")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return M2cpUserStatus{}, fmt.Errorf("%s user status --json: %w", M2cpCLI, err)
	}
	return parseM2cpUserStatus(stdout.Bytes())
}

// parseM2cpUserStatus reads either known shape of the status payload:
// the older one wrapped the whole body in an "output" object, the
// current one puts it at the top level. Rather than guessing from a
// missing field, we look for the wrapper key and unwrap when it's
// there -- so a payload that is merely incomplete produces a useful
// error instead of being silently reparsed as the other format.
func parseM2cpUserStatus(raw []byte) (M2cpUserStatus, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return M2cpUserStatus{}, fmt.Errorf("parsing %s user status JSON: %w", M2cpCLI, err)
	}
	// Case-insensitive on the key: the wrapper has been seen spelled
	// both "output" and "Output", and encoding/json would match
	// either against a struct tag, so the same tolerance applies here.
	payload := raw
	for k, v := range envelope {
		if strings.EqualFold(k, "output") {
			payload = v
			break
		}
	}

	var body m2cpStatusBody
	if err := json.Unmarshal(payload, &body); err != nil {
		return M2cpUserStatus{}, fmt.Errorf("parsing %s user status JSON: %w", M2cpCLI, err)
	}

	out := M2cpUserStatus{
		Tenant:   firstNonEmpty(body.Session.Tenant.Alias, body.Session.TenantAlias, body.Session.Tenant.TenantName, body.Session.TenantName),
		StoreURL: body.Session.Store,
	}

	switch {
	case body.Status != "":
		out.LoggedIn = normalizeStatus(body.Status) == "loggedin"
	case out.StoreURL != "":
		// No status field in this payload at all, but m2cp is
		// reporting an active store: treat that as a session. Keeps
		// us working if a future format drops the field.
		out.LoggedIn = true
	default:
		return M2cpUserStatus{}, fmt.Errorf("could not read a session status or store URL from %s user status output", M2cpCLI)
	}

	if out.LoggedIn && out.StoreURL == "" {
		return out, errors.New("m2cp reports logged in but no store URL")
	}
	return out, nil
}

// normalizeStatus folds the status string so the comparison survives
// cosmetic changes: "logged in", "logged-in" and "loggedIn" all mean
// the same thing, while "logged out" still doesn't match.
func normalizeStatus(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "")
	return strings.ReplaceAll(s, "-", "")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
