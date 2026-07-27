package commands

import "testing"

// TestParseM2cpUserStatus pins the tolerance we need from the status
// parser: the m2cp CLI has changed its output shape once already
// (older versions wrapped the payload in an "output" object, current
// ones emit it flat) and the builder has to keep working against both.
func TestParseM2cpUserStatus(t *testing.T) {
	const flatBody = `{
		"status": "logged in",
		"configFilePath": "/home/u/.m2cp/state.json",
		"session": {
			"store": "https://store.example.com/graphql",
			"tenant": {"id": "abc", "tenantName": "Example GmbH", "alias": "example"},
			"tenant-alias": "example",
			"tenant-name": "Example GmbH"
		}
	}`

	testCases := []struct {
		name         string
		raw          string
		wantLoggedIn bool
		wantTenant   string
		wantStore    string
		wantErr      bool
	}{
		{
			name:         "new_flat_format",
			raw:          flatBody,
			wantLoggedIn: true,
			wantTenant:   "example",
			wantStore:    "https://store.example.com/graphql",
		},
		{
			name:         "old_wrapped_format",
			raw:          `{"output": ` + flatBody + `}`,
			wantLoggedIn: true,
			wantTenant:   "example",
			wantStore:    "https://store.example.com/graphql",
		},
		{
			name:         "old_wrapped_format_capitalised_key",
			raw:          `{"Output": ` + flatBody + `}`,
			wantLoggedIn: true,
			wantTenant:   "example",
			wantStore:    "https://store.example.com/graphql",
		},
		{
			name:         "logged_out_flat",
			raw:          `{"status": "logged out", "session": {}}`,
			wantLoggedIn: false,
		},
		{
			name:         "logged_out_wrapped",
			raw:          `{"output": {"status": "logged out", "session": {}}}`,
			wantLoggedIn: false,
		},
		{
			// Cosmetic spelling changes of the status string must not
			// read as logged out.
			name:         "status_spelling_variants",
			raw:          `{"status": "Logged-In", "session": {"store": "https://s/graphql"}}`,
			wantLoggedIn: true,
			wantStore:    "https://s/graphql",
		},
		{
			// A format that drops the status field but reports a
			// store is still an active session.
			name:         "no_status_but_store",
			raw:          `{"session": {"store": "https://s/graphql", "tenant": {"alias": "example"}}}`,
			wantLoggedIn: true,
			wantTenant:   "example",
			wantStore:    "https://s/graphql",
		},
		{
			name:       "tenant_name_fallback",
			raw:        `{"status": "logged in", "session": {"store": "https://s/graphql", "tenant": {"tenantName": "Example GmbH"}}}`,
			wantTenant: "Example GmbH", wantLoggedIn: true, wantStore: "https://s/graphql",
		},
		{
			name:    "logged_in_without_store",
			raw:     `{"status": "logged in", "session": {}}`,
			wantErr: true,
		},
		{
			name:    "neither_status_nor_store",
			raw:     `{"session": {}}`,
			wantErr: true,
		},
		{
			name:    "not_json",
			raw:     "User Status\n  Status: logged in\n",
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseM2cpUserStatus([]byte(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.LoggedIn != tc.wantLoggedIn {
				t.Errorf("LoggedIn: expected %v, got %v", tc.wantLoggedIn, got.LoggedIn)
			}
			if got.Tenant != tc.wantTenant {
				t.Errorf("Tenant: expected %q, got %q", tc.wantTenant, got.Tenant)
			}
			if got.StoreURL != tc.wantStore {
				t.Errorf("StoreURL: expected %q, got %q", tc.wantStore, got.StoreURL)
			}
		})
	}
}
