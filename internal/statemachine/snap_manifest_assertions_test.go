// Tests for the prefetched-assertion index check: the build must stop
// at manifest preparation, naming the snaps involved, when the
// appstore leaves a prerequisite assertion unserved.
package statemachine

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/snapcore/snapd/asserts"

	"github.com/canonical/ubuntu-image/internal/commands"
)

// One RSA key for the whole test binary: the account-key assertion
// carries a real encoded public key (asserts refuses anything else),
// and its id is what every other fake assertion claims to be signed
// with.
var testKey = sync.OnceValue(func() (k struct {
	ID   string
	Body []byte
}) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	pub := asserts.RSAPublicKey(&priv.PublicKey)
	body, err := asserts.EncodePublicKey(pub)
	if err != nil {
		panic(err)
	}
	k.ID = pub.ID()
	k.Body = body
	return k
})

// fakeAssertion assembles an assertion from headers alone, with a
// placeholder signature. verifyAssertionIndex reads headers,
// prerequisites and the signing key id, so an unverifiable assertion
// exercises it exactly like a signed one -- and assembling avoids
// generating keys.
func fakeAssertion(headers map[string]any) asserts.Assertion {
	h := map[string]any{
		"sign-key-sha3-384": testKey().ID,
		"timestamp":         "2026-01-01T00:00:00Z",
	}
	var body []byte
	for k, v := range headers {
		if k == "body" {
			body = v.([]byte)
			continue
		}
		h[k] = v
	}
	if body != nil {
		h["body-length"] = strconv.Itoa(len(body))
	}
	a, err := asserts.Assemble(h, body, nil, []byte("AXNpZw=="))
	if err != nil {
		panic(err)
	}
	return a
}

// fakeSnapAssertions builds the pair m2cp returns for one snap: a
// snap-declaration naming its publisher and the matching
// snap-revision naming its developer.
func fakeSnapAssertions(name, snapID, publisher string, rev int) []asserts.Assertion {
	return []asserts.Assertion{
		fakeAssertion(map[string]any{
			"type":         "snap-declaration",
			"authority-id": "mlpa",
			"series":       "16",
			"snap-id":      snapID,
			"snap-name":    name,
			"publisher-id": publisher,
		}),
		fakeAssertion(map[string]any{
			"type":          "snap-revision",
			"authority-id":  "mlpa",
			"snap-sha3-384": strings.Repeat("A", 64),
			"snap-id":       snapID,
			"snap-size":     "1024",
			"snap-revision": strconv.Itoa(rev),
			"developer-id":  publisher,
		}),
	}
}

func fakeAccount(id string) asserts.Assertion {
	return fakeAssertion(map[string]any{
		"type":         "account",
		"authority-id": "mlpa",
		"account-id":   id,
		"display-name": id,
		"username":     id,
		"validation":   "unproven",
	})
}

// fakeSigningKey is the account-key every assertion here is signed
// with; the index needs it to satisfy the signing-key prerequisite.
func fakeSigningKey() asserts.Assertion {
	return fakeAssertion(map[string]any{
		"type":                "account-key",
		"authority-id":        "mlpa",
		"account-id":          "mlpa",
		"public-key-sha3-384": testKey().ID,
		"name":                "store",
		"since":               "2026-01-01T00:00:00Z",
		"body":                testKey().Body,
	})
}

func indexOf(assertions ...asserts.Assertion) map[string]asserts.Assertion {
	idx := map[string]asserts.Assertion{}
	for _, a := range assertions {
		idx[a.Ref().Unique()] = a
	}
	return idx
}

// stubFetcher replaces the appstore lookup for the duration of a test.
func stubFetcher(t *testing.T, fetch func(*asserts.Ref) (asserts.Assertion, error)) {
	t.Helper()
	restore := appstoreAssertionFetch
	appstoreAssertionFetch = fetch
	t.Cleanup(func() { appstoreAssertionFetch = restore })
}

func TestVerifyAssertionIndexMissingPublisherAccount(t *testing.T) {
	restore := sessionTenant
	sessionTenant = func() string { return "acme" }
	defer func() { sessionTenant = restore }()

	var asked []string
	stubFetcher(t, func(ref *asserts.Ref) (asserts.Assertion, error) {
		asked = append(asked, ref.Unique())
		return nil, fmt.Errorf("the appstore served no %s", ref)
	})

	all := []asserts.Assertion{fakeSigningKey(), fakeAccount("mlpa")}
	all = append(all, fakeSnapAssertions("core24", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "mlpa", 6)...)
	all = append(all, fakeSnapAssertions("kb-firewall-control", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "knorr", 7)...)

	err := verifyAssertionIndex(indexOf(all...))
	if err == nil {
		t.Fatal("expected an error for the unserved knorr account")
	}
	msg := err.Error()
	for _, want := range []string{
		"account (knorr)",
		"snap-declaration kb-firewall-control",
		"snap-revision kb-firewall-control (r7)",
		"store system assertion",
		"store snap assertion",
		`"acme"`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message does not mention %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "core24") {
		t.Errorf("error message blames a snap whose publisher is served:\n%s", msg)
	}
	if len(asked) != 1 || asked[0] != "account/knorr" {
		t.Errorf("expected exactly one appstore lookup for account/knorr, got %v", asked)
	}
	t.Log("\n" + msg)
}

// The direct GraphQL lookup is what unblocks a build whose publisher
// account the tenant-scoped m2cp calls leave out: once it answers, the
// account joins the index and reaches the seed through
// AssertionRetrieve.
func TestVerifyAssertionIndexFetchesMissingAccount(t *testing.T) {
	knorr := fakeAccount("knorr")
	stubFetcher(t, func(ref *asserts.Ref) (asserts.Assertion, error) {
		if ref.Unique() != "account/knorr" {
			return nil, fmt.Errorf("unexpected lookup for %s", ref)
		}
		return knorr, nil
	})

	all := []asserts.Assertion{fakeSigningKey(), fakeAccount("mlpa")}
	all = append(all, fakeSnapAssertions("kb-firewall-control", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "knorr", 7)...)
	idx := indexOf(all...)

	if err := verifyAssertionIndex(idx); err != nil {
		t.Fatalf("expected the fetched account to satisfy the index, got: %v", err)
	}
	if _, ok := idx["account/knorr"]; !ok {
		t.Error("the fetched account is absent from the index, so it would never reach the seed")
	}
}

func TestVerifyAssertionIndexAllPrerequisitesServed(t *testing.T) {
	all := []asserts.Assertion{fakeSigningKey(), fakeAccount("mlpa"), fakeAccount("knorr")}
	all = append(all, fakeSnapAssertions("kb-firewall-control", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "knorr", 7)...)

	if err := verifyAssertionIndex(indexOf(all...)); err != nil {
		t.Fatalf("expected the index to check out, got: %v", err)
	}
}

// TestAppstoreAccountFetchLive asks the configured appstore for one
// account assertion and reports what came back. It answers the
// question the stubbed tests cannot: whether this session's store
// serves the account assertion of a given publisher.
//
//	LIOT_LIVE_ACCOUNT=knorr go test ./internal/statemachine/ \
//	    -run TestAppstoreAccountFetchLive -v
func TestAppstoreAccountFetchLive(t *testing.T) {
	accountID := os.Getenv("LIOT_LIVE_ACCOUNT")
	if accountID == "" {
		t.Skip("set LIOT_LIVE_ACCOUNT=<account-id> to query the appstore of the active m2cp session")
	}
	if accountID == "*" {
		listLiveAccounts(t)
		return
	}
	ref := &asserts.Ref{Type: asserts.AccountType, PrimaryKey: []string{accountID}}
	a, err := appstoreAssertionFetch(ref)
	if err != nil {
		t.Fatalf("appstore did not serve %s: %v", ref, err)
	}
	acct, ok := a.(*asserts.Account)
	if !ok {
		t.Fatalf("appstore served a %s where an account was asked for", a.Type().Name)
	}
	t.Logf("account-id=%s display-name=%q validation=%s signed by key %s",
		acct.AccountID(), acct.DisplayName(), acct.Validation(), acct.SignKeyID())
}

// listLiveAccounts logs every account assertion the active session can
// read, which is how to tell a store that withholds another tenant's
// account from one that has no such account at all.
func listLiveAccounts(t *testing.T) {
	t.Helper()
	var resp struct {
		Assertions struct {
			Items []struct {
				AssertionBody string `json:"assertionBody"`
			} `json:"items"`
		} `json:"assertions"`
	}
	err := commands.M2cpGraphQL("assertions: all accounts", assertionByTypeQuery,
		map[string]any{"assertionType": "account", "filter": "account-id: "}, &resp)
	if err != nil {
		t.Fatalf("listing account assertions: %v", err)
	}
	t.Logf("the store served %d account assertion(s)", len(resp.Assertions.Items))
	for _, item := range resp.Assertions.Items {
		a, err := asserts.Decode(normalizeAssertionBody(item.AssertionBody))
		if err != nil {
			t.Logf("  undecodable assertion: %v", err)
			continue
		}
		if acct, ok := a.(*asserts.Account); ok {
			t.Logf("  account-id=%s display-name=%q validation=%s",
				acct.AccountID(), acct.DisplayName(), acct.Validation())
		}
	}
}
