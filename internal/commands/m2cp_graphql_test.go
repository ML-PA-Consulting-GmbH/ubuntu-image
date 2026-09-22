package commands

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPostGraphQLDecodesData(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		io.WriteString(w, `{"data":{"assertions":{"items":[{"assertionBody":"type: account\n"}]}}}`)
	}))
	defer srv.Close()

	var resp struct {
		Assertions struct {
			Items []struct {
				AssertionBody string `json:"assertionBody"`
			} `json:"items"`
		} `json:"assertions"`
	}
	err := postGraphQL(srv.URL, "token-abc", "assertions: account (knorr)",
		"query Q($filter: String) { assertions }", map[string]any{"filter": "account-id: knorr"}, &resp)
	if err != nil {
		t.Fatalf("expected the query to succeed, got: %v", err)
	}
	if len(resp.Assertions.Items) != 1 || resp.Assertions.Items[0].AssertionBody != "type: account\n" {
		t.Errorf("unexpected decoded payload: %+v", resp)
	}
	if gotAuth != "Bearer token-abc" {
		t.Errorf("token reached the server as %q", gotAuth)
	}
	if vars, ok := gotBody["variables"].(map[string]any); !ok || vars["filter"] != "account-id: knorr" {
		t.Errorf("variables reached the server as %v", gotBody["variables"])
	}
}

func TestPostGraphQLReportsQueryErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"errors":[{"message":"The current user is not authorized to access this resource."}]}`)
	}))
	defer srv.Close()

	err := postGraphQL(srv.URL, "t", "assertions: account (knorr)", "query {}", nil, nil)
	if err == nil {
		t.Fatal("expected an error for a response carrying errors")
	}
	if !strings.Contains(err.Error(), "not authorized") {
		t.Errorf("error drops the store's message: %v", err)
	}
}

func TestGraphQLErrorRecognizesRefusedReads(t *testing.T) {
	cases := []struct {
		name         string
		response     string
		status       int
		unauthorized bool
	}{
		{
			name:         "authorization code",
			response:     `{"errors":[{"message":"The current user is not authorized to access this resource.","extensions":{"code":"AUTH_NOT_AUTHORIZED"}}]}`,
			unauthorized: true,
		},
		{
			name:         "message alone",
			response:     `{"errors":[{"message":"Access denied for assertions"}]}`,
			unauthorized: true,
		},
		{
			name:         "a query the store could not parse",
			response:     `{"errors":[{"message":"Unknown field \"assertionz\".","extensions":{"code":"HC0001"}}]}`,
			unauthorized: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tc.response)
			}))
			defer srv.Close()

			err := postGraphQL(srv.URL, "t", "assertions: account (knorr)", "query {}", nil, nil)
			var queryErr *GraphQLError
			if !errors.As(err, &queryErr) {
				t.Fatalf("expected a *GraphQLError, got %T: %v", err, err)
			}
			if queryErr.Unauthorized() != tc.unauthorized {
				t.Errorf("Unauthorized() = %v, want %v, for %v", queryErr.Unauthorized(), tc.unauthorized, err)
			}
		})
	}
}

func TestGraphQLErrorRecognizesRefusedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"statusCode":401}`)
	}))
	defer srv.Close()

	var queryErr *GraphQLError
	err := postGraphQL(srv.URL, "t", "assertions: account (knorr)", "query {}", nil, nil)
	if !errors.As(err, &queryErr) || !queryErr.Unauthorized() {
		t.Errorf("a 401 should read as a refused access, got %v", err)
	}
}

func TestPostGraphQLReportsHTTPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"statusCode":401}`)
	}))
	defer srv.Close()

	err := postGraphQL(srv.URL, "t", "assertions: account (knorr)", "query {}", nil, nil)
	if err == nil {
		t.Fatal("expected an error for a 401 response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error drops the status: %v", err)
	}
}
