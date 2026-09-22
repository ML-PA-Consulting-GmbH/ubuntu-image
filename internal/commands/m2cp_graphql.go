package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// The appstore's GraphQL API answers questions the m2cp CLI has no
// command for yet -- fetching a publisher's account assertion, which
// only the generic `assertions` query serves. The call rides on the
// active session's own token, so the store applies the same
// permissions it applies to every m2cp command.
//
// This is the narrow exception to "the builder talks to the appstore
// through m2cp". It stays narrow: one query, one purpose, and it
// moves back behind the CLI once m2cp grows the command (see
// MODIFICATIONS.md, "Publisher accounts").

// M2cpGraphQL posts query and vars to the GraphQL endpoint the active
// m2cp session points at, and unmarshals the response's data into out.
// label names the call in the build log.
func M2cpGraphQL(label, query string, vars map[string]any, out any) error {
	session, err := M2cpSessionStatus()
	if err != nil {
		return err
	}
	if session.StoreURL == "" {
		return fmt.Errorf("%s reports no store URL (not logged in?)", M2cpCLI)
	}
	token, err := m2cpJWT(session)
	if err != nil {
		return err
	}
	return postGraphQL(session.StoreURL, token, label, query, vars, out)
}

// m2cpJWT returns the active session's bearer token, from the status
// payload that already carries it, otherwise from `m2cp user jwt`.
// That second call writes the token to stderr (Go's `println`), hence
// M2cpCapture: a passed-through stderr would put a bearer token in
// every build log. The token reaches an Authorization header and
// nothing else.
func m2cpJWT(session M2cpUserStatus) (string, error) {
	if session.JWT != "" {
		return session.JWT, nil
	}
	out, err := M2cpCapture("user", "jwt")
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(out.Stdout))
	if token == "" {
		token = strings.TrimSpace(string(out.Stderr))
	}
	if token == "" {
		return "", fmt.Errorf("%s user jwt returned no token", M2cpCLI)
	}
	return token, nil
}

func postGraphQL(url, token, label, query string, vars map[string]any, out any) error {
	payload, err := json.Marshal(struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables,omitempty"`
	}{Query: query, Variables: vars})
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "   $ POST %s (%s)\n", url, label)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "     FAILED after %s: %v\n", since(start), err)
		EchoFailedQuery(query, vars)
		return fmt.Errorf("posting %s to %s: %w", label, url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading %s response from %s: %w", label, url, err)
	}
	fmt.Fprintf(os.Stderr, "     %s %s, %d byte(s)\n", resp.Status, since(start), len(body))

	if resp.StatusCode != http.StatusOK {
		EchoFailedQuery(query, vars)
		return &GraphQLError{
			Label:    label,
			Status:   resp.StatusCode,
			Messages: []string{firstLine(body)},
		}
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message    string `json:"message"`
			Extensions struct {
				Code string `json:"code"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("parsing %s response: %w", label, err)
	}
	if len(envelope.Errors) > 0 {
		EchoFailedQuery(query, vars)
		queryErr := &GraphQLError{Label: label}
		for _, e := range envelope.Errors {
			queryErr.Messages = append(queryErr.Messages, e.Message)
			if e.Extensions.Code != "" {
				queryErr.Codes = append(queryErr.Codes, e.Extensions.Code)
			}
		}
		return queryErr
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("parsing %s data: %w", label, err)
	}
	return nil
}

// EchoFailedQuery prints a query and its variables, so the log carries
// what to hand a backend developer or paste into
// `m2cp store system gql` -- the exact request, rather than a
// description of it. postGraphQL calls it whenever the store declines;
// a caller that finds the answer unusable calls it too, since an empty
// result arrives as a perfectly successful 200.
func EchoFailedQuery(query string, vars map[string]any) {
	fmt.Fprintf(os.Stderr, "     the query that failed:\n")
	for _, line := range strings.Split(strings.TrimRight(query, "\n"), "\n") {
		fmt.Fprintf(os.Stderr, "       %s\n", line)
	}
	if len(vars) == 0 {
		return
	}
	encoded, err := json.Marshal(vars)
	if err != nil {
		return
	}
	fmt.Fprintf(os.Stderr, "     variables: %s\n", encoded)
}

func since(start time.Time) time.Duration {
	return time.Since(start).Round(10 * time.Millisecond)
}

// firstLine keeps an error page out of the build log beyond its first
// line, which is where the status text lives.
func firstLine(body []byte) string {
	s := strings.TrimSpace(string(body))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// GraphQLError is what the store said about a query it declined: its
// own messages, the extension codes that came with them, and the HTTP
// status when it refused before reaching the query at all.
type GraphQLError struct {
	Label    string
	Status   int
	Messages []string
	Codes    []string
}

func (e *GraphQLError) Error() string {
	detail := strings.Join(e.Messages, "; ")
	if len(e.Codes) > 0 {
		detail = fmt.Sprintf("%s [%s]", detail, strings.Join(e.Codes, ", "))
	}
	if e.Status != 0 && e.Status != http.StatusOK {
		return fmt.Sprintf("%s: store answered %d: %s", e.Label, e.Status, detail)
	}
	return fmt.Sprintf("%s: store reported %s", e.Label, detail)
}

// Unauthorized reports whether the store declined over access rather
// than over the query itself. The appstore answers a read the session
// may not make in either of two ways -- an error like this one, or an
// empty result -- so callers treat both as "ask about the scopes".
func (e *GraphQLError) Unauthorized() bool {
	if e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden {
		return true
	}
	for _, c := range e.Codes {
		switch strings.ToUpper(c) {
		case "AUTH_NOT_AUTHORIZED", "AUTH_NOT_AUTHENTICATED", "FORBIDDEN", "UNAUTHORIZED":
			return true
		}
	}
	for _, m := range e.Messages {
		lower := strings.ToLower(m)
		for _, marker := range []string{"not authorized", "not authorised", "unauthorized", "unauthorised", "forbidden", "not permitted", "access denied", "permission"} {
			if strings.Contains(lower, marker) {
				return true
			}
		}
	}
	return false
}
