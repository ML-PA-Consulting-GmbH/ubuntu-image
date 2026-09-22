package statemachine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/snapcore/snapd/asserts"
	"github.com/snapcore/snapd/asserts/sysdb"
	"github.com/snapcore/snapd/image"
	"github.com/snapcore/snapd/seed/seedwriter"
	"github.com/snapcore/snapd/snap"
	"gopkg.in/yaml.v2"

	"github.com/canonical/ubuntu-image/internal/commands"
)

// manifestOutputBase returns the recipe basename (filename without
// extension) for naming build outputs -- the image and seed.manifest
// -- after the recipe rather than the gadget's internal volume name
// (e.g. "imx93-m2cp-uc-vtg" from "imx93-m2cp-uc-vtg.yaml"). Returns ""
// when not in manifest mode, so callers fall back to the default
// names.
func (snapStateMachine *SnapStateMachine) manifestOutputBase() string {
	p := snapStateMachine.Opts.Manifest
	if p == "" {
		return ""
	}
	b := filepath.Base(p)
	return strings.TrimSuffix(b, filepath.Ext(b))
}

// prepareFromManifest loads the online manifest and (eventually) drives
// m2cp to fetch the model assertion, bootstrap trust, and resolve snap
// versions to revisions. Every step narrates itself.
func (snapStateMachine *SnapStateMachine) prepareFromManifest() error {
	path := snapStateMachine.Opts.Manifest
	fmt.Printf("=> reading manifest: %s\n", path)
	m, err := commands.LoadOnlineManifest(path)
	if err != nil {
		return err
	}
	if err := m.ValidateForBuild(); err != nil {
		return err
	}
	fmt.Printf("   model:        %s\n", m.Model.Name)
	fmt.Printf("   architecture: %s\n", m.Model.Architecture)
	fmt.Printf("   base:         %s\n", m.Model.Base)
	fmt.Printf("   grade:        %s\n", m.Model.Grade)
	fmt.Printf("   model snaps:  %d\n", len(m.Snaps))
	for _, s := range m.Snaps {
		fmt.Printf("     - %-32s %-9s %s\n", s.Name, s.Type, s.Version)
	}
	if len(m.ExtraSnaps) > 0 {
		fmt.Printf("   extra snaps:  %d\n", len(m.ExtraSnaps))
		for _, s := range m.ExtraSnaps {
			fmt.Printf("     - %-32s %s\n", s.Name, s.Version)
		}
	}

	stageDir, err := snapStateMachine.manifestStageDir()
	if err != nil {
		return err
	}
	fmt.Printf("=> manifest staging dir: %s\n", stageDir)

	jsonPath := filepath.Join(stageDir, "model.json")
	if err := writeModelJSON(jsonPath, m); err != nil {
		return fmt.Errorf("generating model.json: %w", err)
	}
	revision, err := pushModelGetRevision(jsonPath, fmt.Sprintf("ubuntu-image build %s", m.Model.Name))
	if err != nil {
		return err
	}
	snapStateMachine.manifestModelRevision = revision

	fmt.Printf("=> fetching model assertion (%s revision %d)\n", m.Model.Name, revision)
	modelPath := filepath.Join(stageDir, "model.assert")
	if err := writeM2cpStdout(modelPath,
		"store", "model", "assertion", "m2cp", m.Model.Name, strconv.Itoa(revision),
	); err != nil {
		return fmt.Errorf("fetching model assertion: %w", err)
	}
	info, err := os.Stat(modelPath)
	if err != nil {
		return err
	}
	fmt.Printf("   wrote %s (%d bytes)\n", modelPath, info.Size())
	snapStateMachine.Args.ModelAssertion = modelPath

	deferredTrust, err := injectTrustFromM2cp()
	if err != nil {
		return err
	}

	allPins := append([]commands.OnlineManifestSnap{}, m.Snaps...)
	allPins = append(allPins, m.ExtraSnaps...)

	sm, err := resolveRevisionsViaM2cp(m.Model.Architecture, allPins)
	if err != nil {
		return err
	}
	snapStateMachine.manifestSeedManifest = sm

	// Inject extras into image.Options.Snaps via SnapOpts. The
	// seedwriter will treat these as additional snaps to include in
	// the seed beyond what the model declares. Revision pinning
	// already lives in the SeedManifest above, so we don't need to
	// pass channel info -- just the names.
	for _, s := range m.ExtraSnaps {
		snapStateMachine.Opts.Snaps = append(snapStateMachine.Opts.Snaps, s.Name)
	}

	storeURL, err := configureStoreURLFromM2cp()
	if err != nil {
		return err
	}
	snapStateMachine.manifestStoreURL = storeURL
	snapStateMachine.manifest = m

	builtBy, err := fetchM2cpBuiltBy()
	if err != nil {
		// Not fatal -- the image still builds; we just lose the
		// "who built it" line in build.yaml.
		fmt.Printf("=> warning: could not fetch builder identity: %v\n", err)
	} else {
		snapStateMachine.manifestBuiltBy = builtBy
		fmt.Printf("=> built by: %s\n", builtBy)
	}

	snapStateMachine.manifestSnapURL = buildSnapDownloadURL(m.Model.Architecture)
	fmt.Printf("=> snapd SnapDownloadURL hook wired (m2cp store app download --url)\n")

	retrieve, snapIDForName, err := prefetchAssertionsViaM2cp(m.Model.Architecture, allPins, sm, deferredTrust)
	if err != nil {
		return err
	}
	snapStateMachine.manifestAssertionRetrieve = retrieve
	snapStateMachine.manifestSnapIDForName = snapIDForName
	fmt.Printf("=> snapd AssertionRetrieve hook wired (m2cp store snap assertion)\n")

	// Manifest mode = explicit version pinning; the user is taking
	// responsibility for the kernel/snapd combination. Force the
	// snapd-version-vs-kernel check off so a deliberate pin doesn't
	// trip snapd's defensive check.
	if !snapStateMachine.Opts.AllowSnapdKernelMismatch {
		snapStateMachine.Opts.AllowSnapdKernelMismatch = true
		fmt.Printf("=> AllowSnapdKernelMismatch forced on (manifest mode)\n")
	}

	return nil
}

// prefetchAssertionsViaM2cp pulls the snap-revision + snap-declaration
// (and any prerequisite) assertions for every pinned snap via
// `m2cp store snap assertion <name> <arch> -r <rev>`, decodes them
// into an in-memory index, and returns a function suitable for
// image.Options.AssertionRetrieve. The appstore root is the snapd
// fork's compiled-in trust anchor, so the chain stops there exactly
// like Canonical's setup; intermediate signing keys come through
// this index and the seedwriter writes them to the seed.
func prefetchAssertionsViaM2cp(arch string, pins []commands.OnlineManifestSnap, sm *seedwriter.Manifest, extras []asserts.Assertion) (func(*asserts.Ref) (asserts.Assertion, error), func(string) (string, error), error) {
	fmt.Printf("=> prefetching %d snap-assertion bundles via m2cp (arch=%s)\n", len(pins), arch)
	idx := map[string]asserts.Assertion{}
	// snap-name -> snap-id, harvested from the snap-declaration
	// assertions below. Extra snaps (not in the model) have no
	// snap-id from the model nor in the .snap blob, so the snapd
	// URL-hook download path resolves it through this map.
	nameToID := map[string]string{}
	// Seed the index with the trust-bundle's non-anchor assertions
	// (intermediate signing keys and every non-root account -- e.g.
	// publisher brands like "knorr"). They aren't trust anchors, so
	// the seedwriter's fetcher walks to whichever ones the chain
	// references via this callback and writes them into the seed --
	// exactly what the device's snap-bootstrap needs to resolve the
	// chain at boot (it only has the compiled-in root + its account).
	for _, a := range extras {
		idx[a.Ref().Unique()] = a
	}
	for _, p := range pins {
		rev := sm.AllowedSnapRevision(p.Name)
		if rev.Unset() {
			return nil, nil, fmt.Errorf("internal: no allowed revision pinned for %q", p.Name)
		}
		out, err := commands.M2cpRun("store", "snap", "assertion", p.Name, arch, "-r", rev.String())
		if err != nil {
			return nil, nil, err
		}
		dec := asserts.NewDecoder(bytes.NewReader(out.Stdout))
		added := 0
		for {
			a, err := dec.Decode()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, nil, fmt.Errorf("decoding assertion bundle for %s: %w", p.Name, err)
			}
			idx[a.Ref().Unique()] = a
			if decl, ok := a.(*asserts.SnapDeclaration); ok {
				nameToID[decl.SnapName()] = decl.SnapID()
			}
			added++
		}
		fmt.Printf("   %-32s %-10s -> %d assertions indexed\n", p.Name, p.Version, added)
	}
	fmt.Printf("   total assertions in index: %d\n", len(idx))
	if err := verifyAssertionIndex(idx); err != nil {
		return nil, nil, err
	}
	retrieve := func(ref *asserts.Ref) (asserts.Assertion, error) {
		if a, ok := idx[ref.Unique()]; ok {
			return a, nil
		}
		fmt.Fprintf(os.Stderr, "   !! %s is absent from the prefetched index (%d assertions from m2cp) and no other source is consulted\n",
			ref, len(idx))
		return nil, &asserts.NotFoundError{Type: ref.Type, Headers: refPrimaryKeyHeaders(ref)}
	}
	snapIDForName := func(name string) (string, error) {
		if id, ok := nameToID[name]; ok {
			return id, nil
		}
		return "", fmt.Errorf("no prefetched snap-declaration for %q", name)
	}
	return retrieve, snapIDForName, nil
}

// verifyAssertionIndex walks the prerequisites of every prefetched
// assertion -- each one's signing key, plus the refs the assertion
// type itself declares (a snap-declaration names its publisher
// account, a snap-revision its developer account and declaration) --
// and collects the refs that neither the index nor snapd's trust
// anchors satisfy. Missing accounts and account-keys are then fetched
// from the appstore directly; whatever remains stops the build here.
//
// The check runs at preparation time because the fetcher hits the same
// wall three minutes later, after every blob has been downloaded, and
// reports it as a bare "account (knorr) not found" from deep inside
// snapd. Here the reference is still attached to the snaps that
// reached for it, so the message names them and says where the
// assertion has to come from.
func verifyAssertionIndex(idx map[string]asserts.Assertion) error {
	missing := unsatisfiedRefs(idx)
	if len(missing) == 0 {
		fmt.Printf("   every prerequisite of the indexed assertions resolves\n")
		return nil
	}
	if fetchMissingFromAppstore(idx, missing) > 0 {
		missing = unsatisfiedRefs(idx)
		if len(missing) == 0 {
			fmt.Printf("   every prerequisite of the indexed assertions resolves\n")
			return nil
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "the appstore did not supply %d assertion(s) the seed needs:\n\n", len(missing))
	for _, m := range missing {
		fmt.Fprintf(&b, "  %s\n    referenced by: %s\n", m.ref, strings.Join(dedupe(m.referrers), ", "))
	}
	b.WriteString("\n")
	b.WriteString(explainMissingAssertions(missing))
	return errors.New(b.String())
}

// missingRef is a reference nothing in reach satisfies, with the
// assertions that reached for it.
type missingRef struct {
	ref       *asserts.Ref
	referrers []string
}

// unsatisfiedRefs returns the references the index leaves open, sorted
// by reference so the report reads the same way twice.
func unsatisfiedRefs(idx map[string]asserts.Assertion) []missingRef {
	provided := map[string]bool{}
	for u := range idx {
		provided[u] = true
	}
	for _, a := range sysdb.Trusted() {
		provided[a.Ref().Unique()] = true
	}

	snapNameByID := map[string]string{}
	for _, a := range idx {
		if decl, ok := a.(*asserts.SnapDeclaration); ok {
			snapNameByID[decl.SnapID()] = decl.SnapName()
		}
	}

	byUnique := map[string]*missingRef{}
	for _, a := range idx {
		refs := append([]*asserts.Ref{
			{Type: asserts.AccountKeyType, PrimaryKey: []string{a.SignKeyID()}},
		}, a.Prerequisites()...)
		for _, ref := range refs {
			u := ref.Unique()
			if provided[u] {
				continue
			}
			m, seen := byUnique[u]
			if !seen {
				m = &missingRef{ref: ref}
				byUnique[u] = m
			}
			m.referrers = append(m.referrers, describeReferrer(a, snapNameByID))
		}
	}

	out := make([]missingRef, 0, len(byUnique))
	for _, m := range byUnique {
		sort.Strings(m.referrers)
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ref.Unique() < out[j].ref.Unique() })
	return out
}

// fetchMissingFromAppstore asks the appstore's GraphQL API for each
// missing account and account-key, and adds what comes back to the
// index. Whatever it adds reaches the seed the ordinary way, through
// AssertionRetrieve, so the device resolves the same chain at boot.
//
// A reference the store declines to serve is reported and left in
// place: the error that follows names it with the snaps that need it.
func fetchMissingFromAppstore(idx map[string]asserts.Assertion, missing []missingRef) int {
	added := 0
	for _, m := range missing {
		switch m.ref.Type {
		case asserts.AccountType, asserts.AccountKeyType:
		default:
			continue
		}
		fmt.Printf("=> %s is missing; asking the appstore for it directly\n", m.ref)
		a, err := appstoreAssertionFetch(m.ref)
		if err != nil {
			fmt.Printf("   %v\n", err)
			continue
		}
		idx[a.Ref().Unique()] = a
		describeAssertion(a, "-> seed (fetched from the appstore)")
		added++
	}
	return added
}

// assertionReadAllScope is the appstore access right that governs
// reading assertions beyond one's own tenant (the permission levels
// are documented in m2cp's go/graphql-examples.md as All / Tenant /
// Own).
const assertionReadAllScope = "Assertion.Read.All"

// assertionByTypeQuery is the appstore's generic assertion lookup, the
// only route to an account assertion: the tenant-scoped bundles
// (`store system assertion`) carry the caller's own account, and a
// snap bundle (`store snap assertion`) carries the snap-revision and
// snap-declaration alone. m2cp wraps this same query internally
// (`go/graphql/assertion.go`, AssertionsByTypeAndStringFilter) and
// uses it for models; until a CLI command exposes it for accounts,
// the builder asks for itself.
const assertionByTypeQuery = `query AssertionByType($assertionType: String, $filter: String) {
  assertions(where: {
    assertionType: { assertionTypeName: { eq: $assertionType } },
    and: { assertionBody: { contains: $filter } }
  }) {
    items { assertionBody }
  }
}`

// appstoreAssertionFetch resolves one assertion reference through the
// appstore's GraphQL API. Stubbed in tests.
var appstoreAssertionFetch = func(ref *asserts.Ref) (asserts.Assertion, error) {
	assertionType, filter, err := assertionQueryFor(ref)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Assertions struct {
			Items []struct {
				AssertionBody string `json:"assertionBody"`
			} `json:"items"`
		} `json:"assertions"`
	}
	err = commands.M2cpGraphQL(fmt.Sprintf("assertions: %s", ref), assertionByTypeQuery,
		map[string]any{"assertionType": assertionType, "filter": filter}, &resp)
	if err != nil {
		var queryErr *commands.GraphQLError
		if errors.As(err, &queryErr) && queryErr.Unauthorized() {
			return nil, fmt.Errorf("%w\n   %s", err, verifyScopeHint())
		}
		return nil, err
	}
	// The filter matches assertion bodies by substring, so the answer
	// can hold near misses (account-id "knorr-test" contains
	// "knorr"); the reference decides.
	for _, item := range resp.Assertions.Items {
		a, err := asserts.Decode(normalizeAssertionBody(item.AssertionBody))
		if err != nil {
			continue
		}
		if a.Ref().Unique() == ref.Unique() {
			return a, nil
		}
	}
	// An empty answer reads the same whether the store holds no such
	// account or filters it out of this session's reads, so the
	// message offers both and asks for one check rather than naming a
	// cause it cannot see. It arrives as a successful 200, so the
	// query is echoed here rather than by the transport.
	commands.EchoFailedQuery(assertionByTypeQuery, map[string]any{"assertionType": assertionType, "filter": filter})
	return nil, fmt.Errorf("the appstore served no %s (%d assertion(s) matched %q)\n   %s",
		ref, len(resp.Assertions.Items), filter, verifyScopeHint())
}

// verifyScopeHint asks the operator to check the one access right that
// governs this read, without asserting that it is what went wrong.
func verifyScopeHint() string {
	return fmt.Sprintf("please verify that your session may read assertions of other tenants "+
		"(the %s scope); %s user roles list prints this session's scopes",
		assertionReadAllScope, commands.M2cpCLI)
}

// assertionQueryFor maps a reference to the type name and body
// substring that find it.
func assertionQueryFor(ref *asserts.Ref) (assertionType, filter string, err error) {
	if len(ref.PrimaryKey) == 0 {
		return "", "", fmt.Errorf("reference %s carries no primary key", ref)
	}
	switch ref.Type {
	case asserts.AccountType:
		return "account", "account-id: " + ref.PrimaryKey[0], nil
	case asserts.AccountKeyType:
		return "account-key", "public-key-sha3-384: " + ref.PrimaryKey[0], nil
	default:
		return "", "", fmt.Errorf("no appstore query for %s assertions", ref.Type.Name)
	}
}

// normalizeAssertionBody trims trailing whitespace and leaves exactly
// one newline: snapd's decoder splits content from signature at the
// last "\n\n", so a blank line at the end would make it read the
// signature as the body.
func normalizeAssertionBody(body string) []byte {
	return append(bytes.TrimRight([]byte(body), "\r\n \t"), '\n')
}

// describeReferrer names an assertion the way the recipe author knows
// it -- by snap -- falling back to the assertion reference.
func describeReferrer(a asserts.Assertion, snapNameByID map[string]string) string {
	switch v := a.(type) {
	case *asserts.SnapDeclaration:
		return fmt.Sprintf("snap-declaration %s", v.SnapName())
	case *asserts.SnapRevision:
		name := snapNameByID[v.SnapID()]
		if name == "" {
			name = v.SnapID()
		}
		return fmt.Sprintf("snap-revision %s (r%d)", name, v.SnapRevision())
	default:
		return a.Ref().String()
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// sessionTenant names the tenant the active m2cp session belongs to,
// for error messages that turn on tenant scoping. A session lookup
// that fails leaves the message generic, so a broken session never
// masks the error being reported. Stubbed in tests.
var sessionTenant = func() string {
	st, err := commands.M2cpSessionStatus()
	if err != nil {
		return ""
	}
	return st.Tenant
}

// explainMissingAssertions writes the operator-facing half of the
// error: what the appstore serves, why that leaves a gap, and what
// closes it. Account assertions get the long form, because a
// cross-tenant publisher is the case that actually occurs and the one
// that costs an afternoon of guessing.
func explainMissingAssertions(missing []missingRef) string {
	var b strings.Builder
	accounts := []string{}
	for _, m := range missing {
		if m.ref.Type == asserts.AccountType {
			accounts = append(accounts, m.ref.PrimaryKey[0])
		}
	}
	if len(accounts) == 0 {
		fmt.Fprintf(&b, "Every assertion above is a prerequisite of one the appstore did serve,\n")
		fmt.Fprintf(&b, "and the build resolves assertions from the appstore alone. Ask the\n")
		fmt.Fprintf(&b, "appstore team why %s store snap assertion omits them.\n", commands.M2cpCLI)
		return b.String()
	}

	tenant := "this session's tenant"
	if t := sessionTenant(); t != "" {
		tenant = fmt.Sprintf("%q", t)
	}
	fmt.Fprintf(&b, "Account assertions reach the build from one place only, and it is\n")
	fmt.Fprintf(&b, "scoped to the tenant you are logged into (%s):\n\n", tenant)
	fmt.Fprintf(&b, "  %s store system assertion      -> account + account-keys of your own tenant\n", commands.M2cpCLI)
	fmt.Fprintf(&b, "  %s store snap assertion ...    -> snap-revision + snap-declaration only\n\n", commands.M2cpCLI)
	fmt.Fprintf(&b, "So a snap published by another tenant (here: %s) arrives with a\n", strings.Join(accounts, ", "))
	fmt.Fprintf(&b, "publisher the build cannot vouch for. The seed needs that account twice\n")
	fmt.Fprintf(&b, "over: this build writes it into the seed's assertions/, and the device\n")
	fmt.Fprintf(&b, "resolves it again at first boot (snap-bootstrap otherwise stops with\n")
	fmt.Fprintf(&b, "\"cannot resolve prerequisite assertion: account (%s)\").\n\n", accounts[0])
	fmt.Fprintf(&b, "The build also asked the store's GraphQL API for the account directly,\n")
	fmt.Fprintf(&b, "with the query and the store's answer logged above this message.\n\n")
	fmt.Fprintf(&b, "Two things are worth verifying before taking this further:\n")
	fmt.Fprintf(&b, "  - that the account exists in this store at all;\n")
	fmt.Fprintf(&b, "  - that your session may read assertions of other tenants (the %s\n", assertionReadAllScope)
	fmt.Fprintf(&b, "    scope) -- %s user roles list prints this session's scopes.\n\n", commands.M2cpCLI)
	fmt.Fprintf(&b, "To unblock the build, have the appstore serve the publisher account:\n")
	fmt.Fprintf(&b, "flagged globally shared the way the shared app itself is, or included\n")
	fmt.Fprintf(&b, "in the %s store snap assertion bundle.\n", commands.M2cpCLI)
	return b.String()
}

func refPrimaryKeyHeaders(ref *asserts.Ref) map[string]string {
	h := make(map[string]string, len(ref.PrimaryKey))
	for i, name := range ref.Type.PrimaryKey {
		if i < len(ref.PrimaryKey) {
			h[name] = ref.PrimaryKey[i]
		}
	}
	return h
}

// buildSnapDownloadURL returns the callback handed to
// image.Options.SnapDownloadURL: per requested snap, it shells out to
// `m2cp store app download <name> <ARCH> <revision> --url` and returns
// the temporary signed URL. snapd then HTTP-GETs that URL and runs the
// rest of the seed pipeline unchanged. m2cp is expected to be on PATH.
func buildSnapDownloadURL(arch string) func(name string, rev snap.Revision, snapID string) (string, error) {
	upperArch := strings.ToUpper(arch)
	return func(name string, rev snap.Revision, snapID string) (string, error) {
		revStr := rev.String()
		out, err := commands.M2cpRun("store", "app", "download", name, upperArch, revStr, "--url")
		if err != nil {
			return "", err
		}
		url := strings.TrimSpace(string(out.Stdout))
		if url == "" {
			return "", fmt.Errorf("m2cp returned an empty download URL for %s revision %s", name, revStr)
		}
		fmt.Printf("   -> %s\n", redactSignedURL(url))
		return url, nil
	}
}

// redactSignedURL trims the Azure Blob SAS query so logs don't carry
// a long-lived-looking signature. The path + start/end times are kept;
// the sig= parameter is masked.
func redactSignedURL(u string) string {
	q := strings.Index(u, "?")
	if q < 0 {
		return u
	}
	host := u[:q]
	params := strings.Split(u[q+1:], "&")
	for i, p := range params {
		if strings.HasPrefix(p, "sig=") {
			params[i] = "sig=<redacted>"
		}
	}
	return host + "?" + strings.Join(params, "&")
}

// configureStoreURLFromM2cp reads the active store URL from
// `m2cp user status --json` (it's the GraphQL endpoint, e.g.
// https://host/graphql), strips the /graphql suffix to land on the
// snap-store HTTP base, exports SNAPPY_FORCE_API_URL so snapd's
// store client targets this appstore for the rest of the build, and
// returns the snap-store base URL so it can be recorded in build.yaml.
func configureStoreURLFromM2cp() (string, error) {
	fmt.Printf("=> discovering store URL\n")
	session, err := commands.M2cpSessionStatus()
	if err != nil {
		return "", err
	}
	graphqlURL := session.StoreURL
	if graphqlURL == "" {
		return "", fmt.Errorf("m2cp user status returned empty store URL (not logged in?)")
	}
	if !strings.HasSuffix(graphqlURL, "/graphql") {
		return "", fmt.Errorf("expected store URL to end in /graphql, got %q", graphqlURL)
	}
	base := strings.TrimSuffix(graphqlURL, "/graphql")
	fmt.Printf("   graphql:    %s\n", graphqlURL)
	fmt.Printf("   snap-store: %s\n", base)
	if err := os.Setenv("SNAPPY_FORCE_API_URL", base); err != nil {
		return "", fmt.Errorf("setting SNAPPY_FORCE_API_URL: %w", err)
	}
	return base, nil
}

// fetchM2cpBuiltBy pulls the developer identity (from
// `m2cp user info --json`) so we can record who built the image in
// build.yaml. Format is "Display Name <email>" when both fields
// are present, else whichever one we have.
func fetchM2cpBuiltBy() (string, error) {
	out, err := commands.M2cpRun("user", "info", "--json")
	if err != nil {
		return "", err
	}
	var resp struct {
		Output struct {
			Email string `json:"developerAccountSshKeyEmail"`
			Name  string `json:"developerAccountSshKeyname"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out.Stdout, &resp); err != nil {
		return "", fmt.Errorf("parsing m2cp user info: %w", err)
	}
	name := strings.TrimSpace(resp.Output.Name)
	email := strings.TrimSpace(resp.Output.Email)
	switch {
	case name != "" && email != "":
		return fmt.Sprintf("%s <%s>", name, email), nil
	case email != "":
		return email, nil
	case name != "":
		return name, nil
	default:
		return "", fmt.Errorf("m2cp user info returned no name or email")
	}
}

// writeModelJSON renders the recipe's model definition (via
// commands.RenderModelJSON, the shared definition) and writes it to
// dst with narration.
func writeModelJSON(dst string, m *commands.OnlineManifest) error {
	body, err := commands.RenderModelJSON(m)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, body, 0o644); err != nil {
		return err
	}
	fmt.Printf("=> wrote model.json: %s (%d bytes, %d snaps, grade=%s)\n",
		dst, len(body), len(m.Snaps), m.Model.Grade)
	return nil
}

// modelPushResponse maps the --json output of
// `m2cp store system model push -i`. The -i (idempotent) flag
// guarantees a revision in the response whether the model was
// created, updated, or already at the requested content.
type modelPushResponse struct {
	Output struct {
		Revision int    `json:"revision"`
		Error    string `json:"error"`
	} `json:"output"`
}

// pushModelGetRevision drives the appstore's model push with the
// -i (idempotent) flag: a single call that always returns a
// revision when the requested state is achievable, regardless of
// whether the model was created, updated, or already at the
// requested content.
//
// Requires m2cp >= v5.7.x with the idempotent flag.
//
// m2cp returns a non-zero exit on errors but still writes a JSON
// envelope to stdout containing output.error -- so we always
// parse stdout, surfacing whichever of output.revision or
// output.error is present. That way the caller sees the actual
// server-side error message instead of a bare "exit status 1".
func pushModelGetRevision(jsonPath, msg string) (int, error) {
	fmt.Printf("=> pushing model (idempotent)\n")
	out, runErr := commands.M2cpRun("store", "system", "model", "push", "-i", "--json", jsonPath, msg)

	var resp modelPushResponse
	if err := json.Unmarshal(out.Stdout, &resp); err != nil {
		if runErr != nil {
			return 0, fmt.Errorf("m2cp push failed: %w (stdout=%q)",
				runErr, strings.TrimSpace(string(out.Stdout)))
		}
		return 0, fmt.Errorf("parsing m2cp push response: %w (stdout=%q)",
			err, strings.TrimSpace(string(out.Stdout)))
	}
	if resp.Output.Error != "" {
		return 0, fmt.Errorf("m2cp: %s", resp.Output.Error)
	}
	if resp.Output.Revision <= 0 {
		if runErr != nil {
			return 0, fmt.Errorf("m2cp push failed: %w (raw=%q)",
				runErr, strings.TrimSpace(string(out.Stdout)))
		}
		return 0, fmt.Errorf("m2cp returned no revision (raw=%q)",
			strings.TrimSpace(string(out.Stdout)))
	}
	fmt.Printf("   revision: %d\n", resp.Output.Revision)
	return resp.Output.Revision, nil
}

// buildYAML is the schema written to <seed>/systems/<label>/build.yaml
// at the end of a manifest-mode build. Field order is the order
// keys appear in the YAML output. Every field is always present;
// unknown values fall back to "not set" so the schema is stable
// regardless of env or linker flags.
type buildYAML struct {
	Builder        string `yaml:"builder"`
	BuilderVersion string `yaml:"builder-version"`
	BuiltBy        string `yaml:"built-by"`
	AppstoreURL    string `yaml:"appstore-url"`
	ModelName      string `yaml:"model-name"`
	ModelRevision  string `yaml:"model-revision"`
	Arch           string `yaml:"arch"`
	Date           string `yaml:"date"`
	Version        string `yaml:"version"`
	Release        string `yaml:"release"`
	Commit         string `yaml:"commit"`
	Repo           string `yaml:"repo"`
	Grade          string `yaml:"grade"`
}

const notSet = "not set"

// writeBuildYAML emits build.yaml into each /systems/<label>/ entry
// of the seedwriter's output. Called at the end of prepareImage in
// manifest mode so the resulting image carries machine-readable
// provenance (which builder, which store, which model, when).
func writeBuildYAML(snapStateMachine *SnapStateMachine) error {
	seedRoot := filepath.Join(snapStateMachine.tempDirs.unpack, "system-seed", "systems")
	entries, err := os.ReadDir(seedRoot)
	if err != nil {
		return fmt.Errorf("scanning %s for system labels: %w", seedRoot, err)
	}

	info := assembleBuildYAML(snapStateMachine)
	body, err := yaml.Marshal(info)
	if err != nil {
		return fmt.Errorf("marshalling build.yaml: %w", err)
	}

	wrote := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(seedRoot, e.Name(), "build.yaml")
		fmt.Printf("=> writing %s (%d bytes)\n", path, len(body))
		if err := os.WriteFile(path, body, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
		wrote++
	}
	if wrote == 0 {
		return fmt.Errorf("no /systems/<label>/ directories under %s; build.yaml not written", seedRoot)
	}
	return nil
}

func assembleBuildYAML(snapStateMachine *SnapStateMachine) buildYAML {
	builderVersion := commands.BuilderVersion
	if builderVersion == "" {
		builderVersion = notSet
	}
	storeURL := snapStateMachine.manifestStoreURL
	if storeURL == "" {
		storeURL = notSet
	}
	builtBy := snapStateMachine.manifestBuiltBy
	if builtBy == "" {
		builtBy = notSet
	}
	mb := snapStateMachine.manifest.Build
	return buildYAML{
		Builder:        "liot-image",
		BuilderVersion: builderVersion,
		BuiltBy:        builtBy,
		AppstoreURL:    storeURL,
		ModelName:      snapStateMachine.manifest.Model.Name,
		ModelRevision:  strconv.Itoa(snapStateMachine.manifestModelRevision),
		Arch:           snapStateMachine.manifest.Model.Architecture,
		Date:           time.Now().UTC().Format(time.RFC3339),
		Version:        resolveBuildField("BUILD_VERSION", "version", mb.Version),
		Release:        resolveBuildField("BUILD_RELEASE", "release", mb.Release),
		Commit:         resolveBuildField("BUILD_COMMIT", "commit", mb.Commit),
		Repo:           resolveBuildField("BUILD_REPO", "repo", mb.Repo),
		Grade:          snapStateMachine.manifest.Model.Grade,
	}
}

// resolveBuildField applies env-over-manifest-over-notset precedence
// to one of the BUILD_* fields. Every non-default outcome (env in
// use, manifest in use, env overriding manifest) is logged so the
// build trace explains the value that landed in build.yaml.
func resolveBuildField(envKey, manifestKey, manifestVal string) string {
	envVal := os.Getenv(envKey)
	switch {
	case envVal != "" && manifestVal != "" && envVal != manifestVal:
		fmt.Printf("=> build.%s: env %s=%q OVERRIDES manifest build.%s=%q\n",
			manifestKey, envKey, envVal, manifestKey, manifestVal)
		return envVal
	case envVal != "" && manifestVal != "" && envVal == manifestVal:
		fmt.Printf("=> build.%s: env %s and manifest agree (%q)\n",
			manifestKey, envKey, envVal)
		return envVal
	case envVal != "" && manifestVal == "":
		fmt.Printf("=> build.%s: %s=%q (manifest unset)\n",
			manifestKey, envKey, envVal)
		return envVal
	case envVal == "" && manifestVal != "":
		fmt.Printf("=> build.%s: manifest=%q (env %s unset)\n",
			manifestKey, manifestVal, envKey)
		return manifestVal
	default:
		return notSet
	}
}

// resolveRevisionsViaM2cp looks each pinned snap up via
// `m2cp store snap info <name> <arch> --json`, finds the revision
// whose snapVersion matches the manifest's pinned version, and
// returns a seedwriter.Manifest with all of them set as the only
// allowed revision for that snap.
func resolveRevisionsViaM2cp(arch string, pins []commands.OnlineManifestSnap) (*seedwriter.Manifest, error) {
	fmt.Printf("=> resolving %d snap revisions via m2cp (arch=%s)\n", len(pins), arch)
	sm := seedwriter.NewManifest()
	for _, p := range pins {
		rev, err := resolveSnapRevision(p.Name, arch, p.Version)
		if err != nil {
			return nil, err
		}
		if err := sm.SetAllowedSnapRevision(p.Name, snap.R(rev)); err != nil {
			return nil, fmt.Errorf("pinning %s revision %d: %w", p.Name, rev, err)
		}
		fmt.Printf("   %-32s %-10s -> revision %d\n", p.Name, p.Version, rev)
	}
	return sm, nil
}

// snapInfoResponse is the minimal slice of `m2cp store snap info --json`
// we need to map a (name, version) pin to a concrete revision.
type snapInfoResponse struct {
	Output struct {
		SnapDeclaration struct {
			SnapName      string             `json:"snapName"`
			SnapRevisions []snapInfoRevision `json:"snapRevisions"`
		} `json:"snapDeclaration"`
	} `json:"output"`
}

type snapInfoRevision struct {
	Revision    int    `json:"revision"`
	SnapVersion string `json:"snapVersion"`
}

func resolveSnapRevision(name, arch, version string) (int, error) {
	out, err := commands.M2cpRun("store", "snap", "info", name, arch, "--json")
	if err != nil {
		return 0, err
	}
	var resp snapInfoResponse
	if err := json.Unmarshal(out.Stdout, &resp); err != nil {
		return 0, fmt.Errorf("parsing snap info JSON for %s: %w", name, err)
	}
	for _, r := range resp.Output.SnapDeclaration.SnapRevisions {
		if r.SnapVersion == version {
			return r.Revision, nil
		}
	}
	return 0, fmt.Errorf("snap %q has no revision with version %q (arch %s)", name, version, arch)
}

// injectTrustFromM2cp pulls the tenant's trust bundle via
// `m2cp store system assertion` and installs *only* the genuine
// trust anchors into snapd's sysdb: the self-signed root account-key
// and the account that owns it. That is exactly the minimal set the
// device's snapd has compiled in.
//
// Everything else in the bundle -- intermediate signing keys AND any
// other account assertions (publisher brands such as "knorr", device
// brand accounts, etc.) -- is returned so the prefetcher can hand it
// to the seedwriter through AssertionRetrieve. The seedwriter's
// fetcher then writes whatever the chain actually references into the
// seed's assertions/ directory.
//
// This split is load-bearing: anything injected as a trust anchor is
// "predefined" to the fetcher, which therefore SKIPS writing it to
// the seed. The device only has the compiled-in root + its account,
// so a publisher account left out of the seed (e.g. a snap published
// by "knorr") makes snap-bootstrap fail at boot with
// "cannot resolve prerequisite assertion: account (knorr)". Hence we
// inject the bare minimum and let everything else travel in the seed.
//
// The discriminator for "root" is self-signing: an account-key whose
// SignKeyID equals its own PublicKeyID was signed by itself. The
// root account is the one whose account-id matches the root key.
//
// Bundle stays in memory; nothing is written to disk.
func injectTrustFromM2cp() ([]asserts.Assertion, error) {
	fmt.Printf("=> fetching trust bundle\n")
	out, err := commands.M2cpRun("store", "system", "assertion")
	if err != nil {
		return nil, err
	}

	// Decode the whole bundle first: we need the root key's account-id
	// before we can tell which account assertion is the trust anchor.
	var all []asserts.Assertion
	dec := asserts.NewDecoder(bytes.NewReader(out.Stdout))
	for {
		a, err := dec.Decode()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decoding trust bundle: %w", err)
		}
		all = append(all, a)
	}

	var rootAccountID string
	for _, a := range all {
		if ak, ok := a.(*asserts.AccountKey); ok && ak.SignKeyID() == ak.PublicKeyID() {
			rootAccountID = ak.AccountID()
			break
		}
	}
	if rootAccountID == "" {
		return nil, fmt.Errorf("trust bundle has no self-signed root account-key")
	}

	var trustAnchors, deferred []asserts.Assertion
	for _, a := range all {
		var disposition string
		switch v := a.(type) {
		case *asserts.AccountKey:
			if v.SignKeyID() == v.PublicKeyID() {
				trustAnchors = append(trustAnchors, a)
				disposition = "sysdb trust anchor (root key)"
			} else {
				deferred = append(deferred, a)
				disposition = "-> seed (intermediate key)"
			}
		case *asserts.Account:
			if v.AccountID() == rootAccountID {
				trustAnchors = append(trustAnchors, a)
				disposition = "sysdb trust anchor (root account)"
			} else {
				deferred = append(deferred, a)
				disposition = "-> seed (account)"
			}
		default:
			deferred = append(deferred, a)
			disposition = "-> seed (other)"
		}
		describeAssertion(a, disposition)
	}

	sysdb.InjectTrusted(trustAnchors)
	// image.Prepare caches sysdb.Trusted() in a package-level var at
	// init time, so InjectTrusted alone won't reach it. MockTrusted
	// (named for tests, but the right tool here) refreshes that cache
	// with the now-expanded trust set.
	image.MockTrusted(sysdb.Trusted())
	fmt.Printf("   injected %d trust anchor(s) into snapd sysdb (root key + its account); %d assertion(s) made available to the seed via AssertionRetrieve\n",
		len(trustAnchors), len(deferred))
	return deferred, nil
}

func describeAssertion(a asserts.Assertion, disposition string) {
	switch v := a.(type) {
	case *asserts.Account:
		fmt.Printf("   account      account-id=%s display-name=%q validation=%s  [%s]\n",
			v.AccountID(), v.DisplayName(), v.Validation(), disposition)
	case *asserts.AccountKey:
		role := "intermediate"
		if v.SignKeyID() == v.PublicKeyID() {
			role = "root"
		}
		fmt.Printf("   account-key  account-id=%s name=%s sha3-384=%s role=%s  [%s]\n",
			v.AccountID(), v.Name(), v.PublicKeyID(), role, disposition)
	case *asserts.Store:
		url := ""
		if u := v.URL(); u != nil {
			url = u.String()
		}
		fmt.Printf("   store        store=%s operator-id=%s url=%q  [%s]\n",
			v.Store(), v.OperatorID(), url, disposition)
	default:
		fmt.Printf("   %s  [%s]\n", a.Type().Name, disposition)
	}
}

// manifestStageDir returns a directory under the active workdir (or a
// fresh temp dir if none was given) where the manifest pipeline can
// stash the model assertion and the trust bundle.
func (snapStateMachine *SnapStateMachine) manifestStageDir() (string, error) {
	base := snapStateMachine.stateMachineFlags.WorkDir
	if base == "" {
		var err error
		base, err = os.MkdirTemp("", "ubuntu-image-manifest-")
		if err != nil {
			return "", fmt.Errorf("creating manifest stage dir: %w", err)
		}
	}
	dir := filepath.Join(base, "manifest")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating manifest stage dir %q: %w", dir, err)
	}
	return dir, nil
}

// writeM2cpStdout runs `m2cp <args...>` and writes its stdout to dst.
// m2cp's `Task-Id:` chatter goes to stderr, so stdout is the clean
// payload (an assertion bundle, in the cases we use this for).
//
// Trailing whitespace is stripped and a single trailing newline is
// added: snapd's assertion decoder uses bytes.LastIndex(buf, "\n\n")
// to split content from signature, so a trailing blank line at EOF
// would make it mistake the signature for the body.
func writeM2cpStdout(dst string, args ...string) error {
	out, err := commands.M2cpRun(args...)
	if err != nil {
		return err
	}
	payload := append(bytes.TrimRight(out.Stdout, "\r\n \t"), '\n')
	return os.WriteFile(dst, payload, 0o644)
}
