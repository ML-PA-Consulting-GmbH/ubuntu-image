package commands

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestSnapStoreAssertionEndpointLive asks the appstore's snap-store
// API for an account assertion the way snapd itself would --
// GET <store>/v2/assertions/account/<id> -- unauthenticated and then
// with the session token, and reports both answers. It decides
// whether the builder can drop its GraphQL call for the endpoint
// snapd already speaks.
//
//	LIOT_LIVE_ACCOUNT=knorr go test ./internal/commands/ \
//	    -run TestSnapStoreAssertionEndpointLive -v
func TestSnapStoreAssertionEndpointLive(t *testing.T) {
	accountID := os.Getenv("LIOT_LIVE_ACCOUNT")
	if accountID == "" {
		t.Skip("set LIOT_LIVE_ACCOUNT=<account-id> to query the appstore of the active m2cp session")
	}
	session, err := M2cpSessionStatus()
	if err != nil {
		t.Fatalf("reading m2cp session: %v", err)
	}
	base := strings.TrimSuffix(session.StoreURL, "/graphql")
	url := base + "/v2/assertions/account/" + accountID
	token, err := m2cpJWT(session)
	if err != nil {
		t.Fatalf("reading session token: %v", err)
	}

	for _, auth := range []string{"", "Bearer " + token, "Macaroon root=\"" + token + "\""} {
		label := "no auth"
		if strings.HasPrefix(auth, "Bearer") {
			label = "bearer token"
		} else if auth != "" {
			label = "macaroon"
		}
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Accept", "application/x.ubuntu.assertion")
		req.Header.Set("Snap-Device-Series", "16")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("%-13s GET %s: %v", label, url, err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Logf("%-13s %s -> %s, %d byte(s): %s", label, url, resp.Status, len(body), firstLine(body))
	}
}
