# TODO.md

Work this fork owes. Each entry carries enough context to pick it up
cold; `MODIFICATIONS.md` describes what the fork already does.

---

## Fetch publisher account assertions through the m2cp CLI

**Today.** `appstoreAssertionFetch`
(`internal/statemachine/snap_manifest.go`) posts a GraphQL query
straight to the appstore to resolve an `account` or `account-key` the
tenant-scoped m2cp calls leave out. It exists because no m2cp command
served an account assertion when a customer build stopped at
`account (knorr) not found`, and because a patched builder reaches
customers through a GitHub download while a new m2cp reaches them
through a release.

**The replacement.** m2cp gained the command the builder was missing
(`~/r/M2CP.2`, commit `acafd72a`):

```sh
m2cp store account assertion <tenantAlias | tenantName | accountId>
```

It resolves a tenant by alias, by name, or by account-id, and prints
the assertion as snapd reads it.

**What to change.** In `appstoreAssertionFetch`, replace the
`commands.M2cpGraphQL` call with `commands.M2cpRun("store", "account",
"assertion", ref.PrimaryKey[0])` and decode its stdout with
`normalizeAssertionBody` + `asserts.Decode`, keeping the reference
check that follows (the CLI resolves the exact account-id, so a single
assertion comes back). Account-keys have no command yet: either keep
the GraphQL path for `asserts.AccountKeyType`, or ask for
`store account assertion --key <sha3-384>` upstream first.

**What this deletes.** With the last GraphQL caller gone,
`internal/commands/m2cp_graphql.go`, its tests, and the `JWT` field on
`M2cpUserStatus` go with it — and with them the builder's only
dependency on the store's GraphQL schema and on handling a bearer
token. `M2cpCapture` stays useful for any future call whose output is
a secret. Keep `internal/commands/snapstore_assertions_live_test.go`:
it records that `<store>/v2/assertions/*` answers 401 to everything a
build host can present, which is why neither route is the snap-store
API.

**When.** Once the m2cp release carrying `store account assertion`
reaches the customers who build images — the CLI shipped 5.14.2 without
it. Until then both paths would have to coexist, and one path that
works everywhere beats two.

**How to verify.** Against a store whose snaps have a foreign
publisher (`dev.knorr-bremse-digital.com/m2cp` serves nine accounts,
`knorr` among them):

```sh
m2cp store account assertion knorr                     # the command answers
liot-image <recipe-with-a-knorr-published-snap>.yaml   # the build completes
m2cp image analyze --assertions bin/<image>.img        # "no missing account assertions detected"
```

The build log should show the fetch as an `m2cp` call rather than a
`POST`, and the seed must still carry the account: without it the
device stops at boot with
`cannot resolve prerequisite assertion: account (knorr)`.
