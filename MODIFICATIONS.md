# MODIFICATIONS.md

This file describes the modifications layered on top of upstream
`canonical/ubuntu-image` and `canonical/snapd` to support building
images against our self-hosted, snap-store-compatible appstore.

The flow is: user provides a manifest (model + snap versions), the
ubuntu-image side drives m2cp to fetch trust + revisions + download
URLs, and snapd does the actual seed pipeline through two small
fork-side hooks. The resulting image is bit-identical to one built
against a Canonical-style snap store.

---

## Part 1 — snapd fork (`/r/snapd`)

The fork is consumed by ubuntu-image via `go.work` pointing at the
local directory (branch `liot-image`). The patch surface is
additions only, symmetric with the pre-existing `AssertionRetrieve`
hook: the online-store flow (`SnapDownloadURL`, ~30 lines + one
~70-line helper) plus the `AllowExtraSnaps`/`SnapIDForName`
seed-composition relaxations across `image/` and `seed/seedwriter/`.

### `image/options.go`

Add one field to the existing `image.Options` struct:

```go
// SnapDownloadURL, when set, replaces the snap store's
// metadata/URL-resolution step with a caller-provided lookup.
// The hook returns a fully-formed HTTPS URL pointing at the
// .snap blob for the given snap at the given revision; snapd
// HTTP-GETs that URL, populates snap.Info from the downloaded
// file, and runs the rest of the seed pipeline (snap-revision
// sha3 match, assertion chain, seed writing) unchanged. The
// resulting image is identical to one built via the normal
// store path.
//
// Intended for closed-ecosystem appstores whose metadata API
// is not snap-store-compatible (e.g. GraphQL-fronted stores)
// but whose download endpoint is a plain signed HTTPS URL.
SnapDownloadURL func(name string, revision snap.Revision, snapID string) (url string, err error)
```

New import: `github.com/snapcore/snapd/snap` (for `snap.Revision`).

### `image/image_linux.go`

1. Add `snapDownloadURL` field on the `imageSeeder` struct, populated
   from `opts.SnapDownloadURL` in `newImageSeeder`:

   ```go
   snapDownloadURL func(name string, revision snap.Revision, snapID string) (string, error)
   ```

2. Branch at the top of `downloadSnaps` to dispatch to the new helper
   when the hook is set:

   ```go
   if s.snapDownloadURL != nil {
       return s.downloadSnapsViaURLHook(snapsToDownload)
   }
   ```

3. New function `downloadSnapsViaURLHook`. Per snap:
   - Look up the pinned revision in
     `s.w.Manifest().AllowedSnapRevision(name)` (the seedwriter's
     pre-provided manifest).
   - Call `s.snapDownloadURL(name, rev, snapID)` to get a download URL.
   - HTTP-GET the URL into a temp file.
   - Parse the file with `snapfile.Open` +
     `snap.ReadInfoFromSnapFile` to obtain `*snap.Info`. Stamp Revision
     and SnapID on the result.
   - `s.w.SetInfo(sn, info, nil)` to register the snap with the
     seedwriter; this assigns the final `sn.Path`.
   - `s.validateSnapArchs(...)` against the model's architecture.
   - Rename (or copy-and-remove on cross-device) the temp file to
     `sn.Path`.
   - Return a `*tooling.DownloadedSnap{Path, Info}` keyed by snap name.

4. New helper `httpDownloadToFile(url string, dst io.Writer) error`:
   thin `http.Get` + status-code check + `io.Copy`. Transport-level
   integrity is intentionally not checked here — the seedwriter
   verifies the snap-revision sha3-384 against the downloaded bytes
   downstream.

New import: `net/http`.

### `image/options.go` + `seed/seedwriter` (`AllowExtraSnaps`, `SnapIDForName`)

A second, independent addition supports seeding snaps that are **not
declared in the model** at `signed`/`secured` grade. Manifest mode
injects `extra-snaps` (e.g. `core24`, pulled in as the base of a
modeled app) via `image.Options.Snaps` (the `--snap` path); snapd
classifies them as extra snaps. A seed of any non-dangerous grade
otherwise forbids extra snaps in four distinct places, so the fix is
one opt-in flag honored at all four, plus a snap-id resolver:

**`AllowExtraSnaps`** — `bool` on both `image.Options` and
`seedwriter.Options`; `image_linux.go` threads one into the other.
When set, the seedwriter treats a model of any grade as dangerous for
these seed-composition checks only (`seed/seedwriter/seed20.go`,
`writer.go`):

1. `policy20.checkAllowedDangerous` — the up-front gate behind *"cannot
   override channels, add devmode snaps, local snaps, or extra
   snaps/components with a model of grade higher than dangerous"*.
   Returns nil when the flag is set.
2. `Writer.checkBase` — a model app's base may be supplied by an extra
   snap downloaded in a *later* batch, so it isn't in `availableByMode`
   when the app is checked. The flag defers (skips) the base check, as
   dangerous mode already does for option snaps.
3. `tree20.writeMeta` — extra snaps are written to `options.yaml`,
   which was rejected (*"unexpected non-model snap overrides with grade
   …"*) above dangerous grade. The flag permits it.

**`SnapIDForName`** — `func(name string) (string, error)` on
`image.Options`, used by the `SnapDownloadURL` download path
(`downloadSnapsViaURLHook`). An extra snap's store-assigned snap-id is
neither in the model nor in the `.snap` blob, so without it the
seedwriter fails (*"snap-id should have been set"*). The hook resolves
it (ubuntu-image supplies it from the prefetched snap-declarations,
see Part 2) and stamps `snap.Info.SnapID` before `SetInfo`.

Additions only. **Build-time only** — these affect which snaps the
image builder composes into the seed; the model assertion is never
modified (an extra snap stays out of the signed model). The device is
unaffected: the on-device snapd reads the seed and installs every
seeded snap at run-mode boot independently of these flags (the
run-mode load path has no grade gate). The trust model is "the
manifest author pins an exact snap set and verifies the image out of
band." Verified end-to-end: an arm64 `signed`-grade build with
`core24` in `extra-snaps` produces an image whose `seed.manifest`
lists `core24` while the seeded model assertion does not.

### What stays unchanged in snapd

The hook strictly replaces the metadata-fetch-and-URL-resolution step
(`tsto.DownloadMany` → `SnapAction` round-trip). All other behavior
runs:

- Trust chain verification (model → brand account-key → root account-key)
- `snap-revision` sha3-384 verification of downloaded blobs
- `snap-revision` / `snap-declaration` / `account` / `account-key`
  resolution (handled via the existing `AssertionRetrieve` hook)
- Seed writing (`seedwriter.Writer`)
- Gadget yaml processing, bootloader configuration, disk image generation

### Companion: pre-existing `AssertionRetrieve` hook

The online-store flow depends on a hook that already exists in the
fork:

```go
// image.Options
AssertionRetrieve func(ref *asserts.Ref) (asserts.Assertion, error)
```

ubuntu-image's manifest pipeline prefetches every required assertion
via m2cp and feeds them through this hook so snapd resolves them
locally instead of going to the store. Not new in this iteration, but
load-bearing for the online-store mode.

### Out of scope (snapd, still in the tree, candidates for revert)

The fork currently carries additional changes from an earlier
offline-mode experiment that are **not** needed by the online-store
flow and should be reverted in a separate audit pass:

- `asserts/database.go`, `asserts/fetcher.go`: `SkipSignatureCheck`
  field on `DatabaseConfig`. The online flow uses the real chain
  (trust anchors injected via `sysdb.InjectTrusted` from
  `m2cp store system assertion`); no need to skip verification.
- `seed/seedwriter/helpers.go`, `seed20.go`, `writer.go`: offline-mode
  tweaks layered over the seedwriter.
- `snapdenv/trust.go`: previously held a `TrustInsecure` toggle.

---

## Part 2 — ubuntu-image fork

All the m2cp shell-outs, manifest parsing, and orchestration. The
`snap` subcommand grows a `--manifest` flag that, when set, runs the
manifest pipeline before the existing state machine takes over.

### Manifest format

A small YAML file pinning a model and a set of snap versions:

```yaml
model:
  name: edge-imx93-uc-vtg
  revision: 1
  architecture: arm64

snaps:
  - name: snapd
    version: "4.3.0.42"
  - name: core24
    version: "1.2.0"
  - name: mlpa-os-imx-kernel
    version: "6.1.5"
  # ...
```

Versions, not revisions, because the user's appstore guarantees
version↔revision bijection; the resolution happens via
`m2cp store snap info --json` during the build.

There is no `model.type` field — only one model type exists in this
fork ("m2cp", meaning the Ubuntu Core family).

### New files

- **`internal/commands/online_manifest.go`** — the YAML schema
  (`OnlineManifest`, `OnlineManifestModel`, `OnlineManifestSnap`),
  `LoadOnlineManifest()`, and the validator.

- **`internal/statemachine/snap_manifest.go`** — the entire pipeline:
  - `prepareFromManifest` (the orchestrator, called from
    `SnapStateMachine.Setup` when `--manifest` is set)
  - `injectTrustFromM2cp` — runs `m2cp store system assertion`,
    decodes account + account-keys + store assertion, filters to trust
    anchors (account, account-key), calls `sysdb.InjectTrusted`, then
    `image.MockTrusted(sysdb.Trusted())` to refresh the package-level
    cache in the `image` package
  - `resolveRevisionsViaM2cp` / `resolveSnapRevision` — per snap,
    `m2cp store snap info <name> <arch> --json`, finds the matching
    version, populates a `seedwriter.Manifest` via
    `SetAllowedSnapRevision`
  - `buildSnapDownloadURL` — returns the callback handed to the snapd
    `SnapDownloadURL` hook; shells out to
    `m2cp store app download <name> <ARCH> <revision> --url` and
    returns the temporary signed URL
  - `prefetchAssertionsViaM2cp` — per snap,
    `m2cp store snap assertion <name> <arch> -r <revision>`, decodes
    the bundle, indexes by `Ref.Unique()`, returns a function suitable
    for `image.Options.AssertionRetrieve`. Also harvests a
    snap-name→snap-id map from the decoded `snap-declaration`
    assertions and returns a resolver for `image.Options.SnapIDForName`
    (needed to stamp the snap-id of extra snaps)
  - `verifyAssertionIndex` / `unsatisfiedRefs` — runs once the index is
    complete, before the state machine downloads anything. Walks every
    indexed assertion's signing key and `Prerequisites()` refs (a
    snap-declaration names its publisher account, a snap-revision its
    developer account and declaration) and collects the refs that
    neither the index nor `sysdb.Trusted()` satisfies, each with the
    snaps that referenced it
  - `fetchMissingFromAppstore` / `appstoreAssertionFetch` — asks the
    appstore's GraphQL API for each missing `account` and
    `account-key` and adds what comes back to the index, from where it
    travels into the seed through `AssertionRetrieve` like every other
    assertion. What the store declines to serve stays missing, and the
    error names it with the snaps that need it. An unserved account
    reads the same whether the store holds none or withholds it from
    this session, so the message asks the operator to verify the
    `Assertion.Read.All` scope (`m2cp user roles list`) rather than
    naming a cause it cannot see. See "Publisher accounts" below
  - `manifestStageDir` — workdir layout for the model.assert file
  - Helpers: `writeM2cpStdout`, `describeAssertion`,
    `redactSignedURL`, `refPrimaryKeyHeaders`,
    `configureStoreURLFromM2cp` (legacy, leftover; the URL hook makes
    `SNAPPY_FORCE_API_URL` unused but harmless — it still supplies the
    appstore URL recorded in `build.yaml`, and now reads it through
    `commands.M2cpSessionStatus` instead of its own parse)

- **`internal/commands/m2cp.go`** — the one way the builder invokes
  the appstore CLI: `M2cpRun(args...)` (capture stdout, tee m2cp's
  stderr, return `M2cpOutput`) and `M2cpStream(args...)` (both streams
  to our stderr, for output meant for the reader). Both echo the
  command line as `   $ m2cp ...`, shell-quoted so it can be pasted
  back, and close with exit status, duration and payload size. Errors
  carry the command line and m2cp's stderr, so no call site has to
  reassemble them. `M2cpRun` returns stdout on failure too: m2cp
  reports some server-side errors in a JSON envelope on stdout while
  exiting non-zero, and `pushModelGetRevision` parses it for the real
  message. `M2cpCapture` is the same runner with stderr kept to
  itself, for `m2cp user jwt`, whose token goes to stderr (the command
  uses Go's builtin `println`) and would otherwise land in every build
  log. Holds the `M2cpCLI` constant.

- **`internal/commands/m2cp_graphql.go`** — `M2cpGraphQL(label, query,
  vars, out)`: posts a GraphQL query to the endpoint the active m2cp
  session points at, with that session's bearer token, and unmarshals
  `data` into `out`. Query errors and non-200 answers come back as Go
  errors carrying the store's own message. The token comes from
  `M2cpUserStatus.JWT`, falling back to `m2cp user jwt` through
  `M2cpCapture`; it reaches an Authorization header and nothing else.
  The narration matches an m2cp call (`   $ POST <url> (<label>)`,
  then status, duration and size). A call that fails prints the query
  and its variables verbatim (`EchoFailedQuery`), so the log carries a
  request to hand a backend developer or paste into
  `m2cp store system gql`; the account fetch prints them for an empty
  result too, since that arrives as a successful 200. A declined read
  comes back as `*GraphQLError`, whose `Unauthorized()` reads the HTTP
  status, the extension codes (`AUTH_NOT_AUTHORIZED` and friends) and
  the message text. This is the builder's one direct API call, and it
  exists to fetch publisher account assertions; everything else goes
  through the CLI.

- **`internal/commands/m2cp_status.go`** — the one reader of
  `m2cp user status --json`: `M2cpSessionStatus()` (shell out) and
  `parseM2cpUserStatus()` (parse), returning `M2cpUserStatus`
  (`LoggedIn`, `Tenant`, `StoreURL`). Both consumers — the
  `liot-image` preflight and the pipeline's store-URL discovery — go
  through it, so neither can drift behind an m2cp output-format
  change. Tolerates both known shapes: older m2cp wraps the payload
  in an `output` object, current m2cp emits it flat. Detection looks
  for the wrapper key (case-insensitively, since `encoding/json`
  would have matched `Output` against the old struct tag too) rather
  than guessing from a missing field, so an incomplete payload
  produces a real error instead of being silently reparsed as the
  other format. Further tolerance: the status string is folded before
  comparison (`logged in` / `logged-in` / `loggedIn` all match, while
  `logged out` still doesn't), tenant falls back
  `alias` → `tenant-alias` → `tenantName` → `tenant-name`, and a
  payload with no status field but a live store URL counts as a
  session. `m2cp_status_test.go` pins every one of those cases.

- **`test-online-store/manifest.yaml`** — concrete pin for
  `edge-imx93-uc-vtg` rev 1, 12 snaps at the revisions of the vanilla
  shipping device.

- **`test-online-store/test.sh`** — end-to-end driver: builds
  ubuntu-image, runs it against the manifest, dumps the resulting
  `seed.manifest` for inspection.

### Modified files

- **`internal/commands/snap.go`** — added the `--manifest` flag to
  `SnapOpts`; dropped the `required:"false"` tag on the positional
  args struct (go-flags treats *any* `required:"..."` value as
  truthy, which was making `model_assertion` falsely required at
  parse time when `--manifest` should be the input instead).

- **`internal/statemachine/snap.go`** — added fields to
  `SnapStateMachine`:
  - `manifestSeedManifest *seedwriter.Manifest`
  - `manifestSnapURL func(name, revision, snapID) (string, error)`
  - `manifestAssertionRetrieve func(*asserts.Ref) (asserts.Assertion, error)`
  - `manifestSnapIDForName func(name string) (string, error)`

  And added the `prepareFromManifest()` call at the top of `Setup`
  when `Opts.Manifest` is set.

- **`internal/statemachine/snap_states.go`** — wires the hooks
  into `image.Options`:
  - `SnapDownloadURL: snapStateMachine.manifestSnapURL`
  - `AssertionRetrieve: snapStateMachine.manifestAssertionRetrieve`
  - `AllowExtraSnaps: snapStateMachine.Opts.Manifest != ""` — on in
    manifest mode only, so the recipe's `extra-snaps` (snaps not in
    the signed model) are accepted into the seed; normal builds keep
    upstream's dangerous-only restriction
  - `SnapIDForName: snapStateMachine.manifestSnapIDForName` — resolves
    an extra snap's snap-id (not known from the model) for the URL-hook
    download path
  - Precedence check in `imageOptsSeedManifest` so
    `manifestSeedManifest` wins over the existing `Opts.Revisions`
    path

- **`cmd/ubuntu-image/main.go`** + **`cmd/ubuntu-image/liot.go`** —
  the tool prints `[L-IoT Image Builder <version>]` on stderr as the
  first line of *every* invocation (build, `--dry-run`, `--model`,
  quick help, hidden subcommands), not just under `--version`, so
  build logs and customer bug reports always carry the version that
  produced them. `liotVersion()` is the single resolver — build-time
  `-X main.Version` stamp, else `SNAP_VERSION` from the snap
  environment, else `unknown-version` — and both the banner,
  `--version`, and `commands.BuilderVersion` (the seed manifest's
  `builder-version`) read through it. The banner is suppressed for a
  bare `--version`, whose own stdout line is the same information, and
  it goes to stderr so `--model`'s JSON stdout stays clean. The
  redundant `[L-IoT Image Builder]` header the preflight used to print,
  and the name headline of the quick-help text, were dropped in favour
  of it.

- **`.gitignore`** — ignore `test-online-store/work/`,
  `test-online-store/out/`, and the locally-built `ubuntu-image`
  binary.

### Pipeline (what `prepareFromManifest` runs)

1. Parse the YAML manifest.
2. `m2cp store model assertion m2cp <name> <revision>` →
   write `<workdir>/manifest/model.assert`, set
   `Args.ModelAssertion` to that path so the existing state machine
   picks it up unchanged.
3. `m2cp store system assertion` → decode trust bundle, narrate every
   assertion with its key fingerprint, `sysdb.InjectTrusted`,
   `image.MockTrusted(sysdb.Trusted())`.
4. `m2cp store snap info <name> <arch> --json` per snap → version →
   revision → `seedwriter.Manifest.SetAllowedSnapRevision`.
5. Build the `SnapDownloadURL` callback that wraps
   `m2cp store app download ... --url`.
6. `m2cp store snap assertion <name> <arch> -r <revision>` per snap →
   decode into an in-memory index → verify every prerequisite the
   index references resolves → build the `AssertionRetrieve` callback.
7. Force `AllowSnapdKernelMismatch = true` — manifest mode is the
   user explicitly declaring a tested kernel/snapd combo, so snapd's
   defensive version check is redundant.

Every step narrates itself in plain language so the build log is a
readable audit trail of which trust anchors were installed, which
revision each version resolved to, and which URL served each blob.

### Design decisions worth recording

- **Versions in the manifest, not revisions.** Human-readable input;
  resolution to revision happens via m2cp.
- **Always fetch model fresh from m2cp.** No caching; the manifest
  names a model + revision and the assertion is pulled per build.
- **Trust bundle in memory only.** No on-disk store.assert artifact;
  the audit trail is the narrated log of injected key fingerprints.
- **URL hook, not blob hook.** snapd keeps owning HTTPS GET, caching,
  sha3 verification — only the URL-resolution step is overridden.
  Smaller patch surface, same security properties.
- **m2cp on `PATH`.** No `M2CP_BIN` env-var indirection.
- **Narrate every step.** Transparency is the default for the
  manifest pipeline, not opt-in.
- **Auto `AllowSnapdKernelMismatch` in manifest mode.** Pinning is
  the user explicitly declaring intent; snapd's defensive version
  check is redundant.
- **Every m2cp call is echoed.** The appstore CLI is the builder's
  only source of trust, revisions, URLs and assertions, so a build log
  lists each invocation with its duration and payload size. That is
  what tells "m2cp said no" apart from "m2cp answered, and we rejected
  the answer" — the distinction a support case turns on. m2cp's own
  stderr, Task-Id included, passes through attached to the call that
  produced it.
- **Prerequisites are checked at preparation time.** An assertion the
  appstore leaves unserved surfaces deep inside snapd's fetcher, three
  minutes later, once every blob has been downloaded, as a bare
  `account (knorr) not found`. `verifyAssertionIndex` finds the same
  gap while the reference is still attached to the snaps that caused
  it, so the error names them, names the tenant, and says where the
  assertion has to come from.
- **One direct API call, and a way back.** The builder speaks to the
  appstore through m2cp, with one exception: the publisher account
  assertion, which no m2cp command serves. That call names itself in
  the log like every other, lives behind `M2cpGraphQL`, and moves back
  behind the CLI the day `m2cp store system assertion --account <id>`
  exists.
- **One parser per m2cp output.** m2cp's JSON shape is outside our
  control and has already changed once. Every payload we read gets a
  single tolerant parser (`internal/commands/m2cp_status.go` for
  `user status`) that both consumers call, so a format change is one
  fix in one place instead of a hunt for the copy that wasn't
  hardened.
- **Version in every log.** The name+version banner prints
  unconditionally at startup; a build log without a version is not
  worth much when a customer reports a bad image.

### Publisher accounts (cross-tenant snaps)

Every assertion the seed carries drags its prerequisites with it: a
`snap-declaration` names a `publisher-id`, a `snap-revision` a
`developer-id`, and both refer to an `account` assertion that the
build must resolve and write into `assertions/`. The device resolves
the same account again at first boot.

Those account assertions reach the build from one place, and it is
scoped to the logged-in tenant. Measured against m2cp v5.14.2:

| call | returns |
| --- | --- |
| `m2cp store system assertion` | `account` + `account-key`s + `store`, for the caller's tenant |
| `m2cp store snap assertion <name> <arch> -r <rev>` | `snap-revision` + `snap-declaration`, and nothing else |
| `m2cp store model assertion m2cp <name> <rev>` | the `model` assertion alone |

So a store whose snaps all belong to one tenant builds cleanly: the
publisher is that tenant's account, which arrives in the trust bundle
(and, when the tenant owns the root key, is a trust anchor besides).
A store that serves *shared* apps — `m2cp store app share` makes an app
global, and the blob then carries the publishing tenant's alias, e.g.
`knorr_kb-firewall-control...snap` — hands the build a snap-declaration
whose publisher account the tenant cannot fetch. The store's own seed
generator has the same gap: `m2cp store system snapd-seed generate`
writes `model.assert`, `store.assert` and one `<app>_<rev>.assert` per
app (`cmd/store/system/snapdseed/generate.go`), from those same three
sources.

The builder closes the gap itself: `fetchMissingFromAppstore` asks the
store's GraphQL API for each missing account and adds it to the
prefetched index, from where the seedwriter writes it into the seed. A
live check against the dev store confirms the route works: the generic
query returned and decoded `account (mlpa)`, signed by the store root
key, and an unfiltered run returned two accounts — `mlpa` and the
store's other tenant `T3E65402FDFC174C` — so the query does reach
across tenants. That session holds `*.*.All` scopes, so what a
tenant-scoped session sees is still open; the customer's store answers
it on the next build, because the fetch attempt and its outcome are
now in the log.

`TestAppstoreAccountFetchLive` is the probe:
`LIOT_LIVE_ACCOUNT=knorr` asks for one account,
`LIOT_LIVE_ACCOUNT='*'` lists every account the session can read.

Verified end to end against the customer's store
(`dev.knorr-bremse-digital.com/m2cp`), on the recipe and snap set whose
build failed there: `store system assertion` returns the `mlpa` account
alone, the generic query returns nine accounts including `knorr` and
`TAC9AE5A5136BB3A`, and `knorr` is signed by `RhtFcWyut-…`, the root
key that store's trust bundle carries — so the device can verify what
the seed now holds. The build logs

    => account (knorr) is missing; asking the appstore for it directly
       $ POST https://…/m2cp/graphql (assertions: account (knorr))
         200 OK 400ms, 1030 byte(s)
       account  account-id=knorr … [-> seed (fetched from the appstore)]

and finishes, and `m2cp image analyze --assertions` on the image
reports the knorr account in the seed and "no missing account
assertions detected".

The GraphQL API can serve the assertion. `assertions(where: …)` filters
on `assertionType.assertionTypeName`, `assertionBody`, `tenantId` and
`isGloballyShared`, and the m2cp CLI already wraps it as
`gql.AssertionsByTypeAndStringFilter(ctx, type, filter)`
(`go/graphql/assertion.go`), whose doc comment lists `account` and
`account-key` among the types it serves. Today one caller uses it, for
models (`helper/model.go`). Two pieces are missing:

- a CLI command that asks for an account, e.g.
  `m2cp store system assertion --account <account-id>` wrapping
  `AssertionsByTypeAndStringFilter(ctx, "account", "account-id: <id>")`;
- server-side visibility of the foreign account: reads are scoped to
  the caller's tenant unless the record is globally shared or the
  caller holds an `.All` scope (`go/graphql-examples.md`), so
  `shareApps` needs to flag the publisher's `account` and
  `account-key` the way it flags the app. Serving the publisher
  account inside `snapAssertions` would close the same gap for every
  consumer at once, including `snapd-seed generate`.

The store's snap-store-compatible API would be the better route --
`GET <store>/v2/assertions/account/<id>` is the shape snapd speaks, no
token and no GraphQL schema in the picture. It is closed to us:
against `dev.knorr-bremse-digital.com/m2cp` that path answers 401 to an
anonymous request, to the developer session token as `Bearer`, and to
the same token in a `Macaroon` header, for both `account` and
`account-key`. The gate is on assertions rather than on the whole
surface: `/v2/snaps/refresh` answers 405 (method dispatch) to an
anonymous request. `TestSnapStoreAssertionEndpointLive`
(`internal/commands`) re-runs that measurement. Whatever credential
that route expects, the build host does not hold it, so the builder
asks GraphQL.

Two patches carry this forward, kept in `~/r/M2CP.2/Tools/m2cp`:
`cmd/store/system/gql.go` adds `m2cp store system gql`, which runs a
raw query with the session's own token and prints the server's answer
verbatim — the way to measure what the store serves a given tenant
without a command per question; and `cmd/user/jwt.go` now prints the
token on stdout (it used Go's builtin `println`, which writes to
stderr, so the token could be neither piped nor kept out of a log).

Worth raising separately: `m2cp user status --json` carries the raw
token under `session.jwt` whatever the `--jwt` flag says, because the
payload is `viper.AllSettings()`. Any build log that captures that
output verbatim captures a bearer token. The builder only ever reads
that field, never prints it.

When the store's answer is "no", `verifyAssertionIndex` reports it at
preparation time, naming the account, the snaps and the tenant.

### Verifying

```sh
bash test-online-store/test.sh
```

A passing run produces a bootable `.img` and a `seed.manifest` whose
12 pinned revisions match the manifest input exactly (the same set
of revisions as a vanilla install of the reference device).
