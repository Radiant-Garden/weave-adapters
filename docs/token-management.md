# Token management

How weave authenticates to this adapter: what a token is, how it is stored, and
how to rotate one without dropping requests.

For the bare flag list, see [cli.md](cli.md#token-management).

> **Status — what ships today.** Minting, storage, listing and revocation are
> live: you can create tokens now and they are stored as described below. The
> adapter does **not yet check them** — the auth middleware that verifies the
> `Authorization` header, enforces expiry, and emits auth-outcome events lands in
> M2 Phase 2. Until then every endpoint is unauthenticated. Sections describing
> request-time behavior are marked **(Phase 2)**.

## The short version

```console
$ weave-adapter-dhcp-windows token gen --label weave-prod
Token "weave-prod" added to tokens.toml

  wadapt_agWOSIO6l39eChimWpCh7zMtU7pVSq_NwM_Ot2QvnX8

This is the only time the token is shown — it is stored as a hash.
Give it to weave as the full Authorization header value, including the scheme:
  Bearer wadapt_agWOSIO6l39eChimWpCh7zMtU7pVSq_NwM_Ot2QvnX8

Restart the adapter for this change to take effect.
```

Copy the `Bearer …` line into weave, restart the adapter, done.

Three details in that output are load-bearing, and each has its own section
below: the token is shown **once**, weave needs the **scheme included**, and the
adapter only reads tokens **at startup**.

## What a token is

32 bytes from `crypto/rand`, base64url-encoded, prefixed `wadapt_`:

```
wadapt_agWOSIO6l39eChimWpCh7zMtU7pVSq_NwM_Ot2QvnX8
```

The prefix is not decoration. It makes tokens greppable by secret scanners and
identifiable in a leak report — if one turns up in a paste or a log, you can
tell what it opens.

256 bits of entropy is far past brute-force range, which is what makes the
storage scheme below safe.

## How tokens are stored

**The adapter never holds a usable token.** `tokens.toml` contains only hashes:

```toml
[[tokens]]
label = 'weave-prod'
hash = 'sha256:65ef9b3358b2212120b5e2fef56fda4da6818b2d27fd4b378dc1e9bc47d30d9e'
createdAt = 2026-07-18T09:02:36Z

[[tokens]]
label = 'weave-staging'
hash = 'sha256:4c36a5ea67a26148b87586e936ddd69fd0e9360109e702c331496f18708e6927'
createdAt = 2026-07-18T09:02:36Z
expiresAt = '2026-10-16T09:02:36Z'
```

On each request the adapter will hash the presented token and compare it
constant-time against these **(Phase 2)**. Nothing in the file can be turned
back into a credential, so **leaking `tokens.toml` does not leak access** — an
attacker would have to reverse SHA-256.

The file is written `0600` and saved atomically (temp file, then rename), so an
interrupted write can never leave a truncated store that locks weave out. On
Windows the Unix mode bits are largely ignored; the directory ACL is the real
protection there.

### Why a plain hash and not bcrypt

bcrypt, scrypt and argon2 exist to slow down brute force against **low-entropy
human passwords**. A token here carries 256 bits of entropy — there is nothing
to brute force, so a slow KDF would buy no security and add latency to every
single request.

### Why SHA-256 and not SHA-512

SHA-512 is faster than SHA-256 in *software* — it uses 64-bit words, so it does
fewer rounds per byte on a 64-bit machine. Hardware acceleration reverses that:
ARMv8 crypto extensions and x86 SHA-NI both accelerate SHA-256, and Go's
`crypto/sha256` uses them. Measured on an Apple M4, hashing a token takes 80 ns
with SHA-256 against 210 ns with SHA-512.

Either way it is one hash of ~50 bytes per request, so the difference is noise
next to the network. SHA-256 keeps the config half as wide and matches what
`sha256sum` and PowerShell's `Get-FileHash` do by default. The stored form is
tagged `sha256:` so the algorithm can change later without guessing at existing
entries.

### Why hashing, when weave encrypts

weave's `credentialMgr` encrypts credentials at rest with AES-GCM, behind a key
provider with master-key rotation. That is correct **for weave** and wrong here,
because the direction of trust is inverted:

|  | weave | this adapter |
|---|---|---|
| Needs | to **present** a credential outbound | to **verify** one inbound |
| Therefore | must recover the plaintext | never needs it back |
| So it uses | reversible encryption + key management | a one-way hash |

Hashing removes an entire problem class: no key provider, no key rotation, no
plaintext-at-rest migration, and no way for a config leak to yield working
credentials.

### The label is not just a comment

Every label becomes the **caller subject** on every event the token's requests
emit **(Phase 2)**. That is what makes an adapter's log answer "who did this",
and it is why a failed-auth event can name which token was rejected without ever
logging the secret:

```
eventId=API-010 caller.subject=weave-prod request.method=GET request.path=/api/v1/leases
```

Give labels that mean something to an operator six months from now —
`weave-prod`, `weave-staging`, `migration-2026-q3` — rather than `token1`.

## Giving the token to weave

Store the **full header value, including the scheme**, in weave's credential
set:

```
Bearer wadapt_agWOSIO6l39eChimWpCh7zMtU7pVSq_NwM_Ot2QvnX8
```

Not just the token. weave's bearer credential type stores `apiToken` as the
complete header value and sends it **verbatim** — it does not prepend `Bearer `
for you. A bare token there arrives as `Authorization: wadapt_…`, which the
adapter rejects.

If that happens, the 401 will say so explicitly rather than making you guess —
its `detail` names the expected format **(Phase 2)**. But it is easier to get
right the first time, which is why `token gen` prints the whole header line.

## Rotating a token

The adapter reads tokens **only at startup**. It does not watch the file and
will not reload it — deliberately, since live reload is machinery to secure and
test for a service that restarts in under a second.

Multiple tokens can be active at once, which is what makes a zero-downtime
rotation possible:

1. **Mint the replacement** — `token gen --label weave-prod-2026q4`
2. **Restart the adapter.** Both the old and new tokens are now accepted.
3. **Switch weave** to the new token.
4. **Revoke the old one** — `token revoke --label weave-prod`
5. **Restart the adapter.** Only the new token is accepted.

Two restarts, no window where weave's requests fail. Doing it in any other order
does have such a window.

## Expiry

Expiry is optional and opt-in per token. It is recorded today and enforced at
request time from **Phase 2**:

```console
$ weave-adapter-dhcp-windows token gen --label weave-prod --expires-in-days 90
```

An expired token will be rejected with its own distinct event — never conflated
with an unknown token, so an operator debugging a sudden 401 sees "expired"
rather than "no such token" **(Phase 2)**.

> **Know what you are opting into.** An expiry means a working deployment starts
> failing at a date, and nothing reaches out to warn you beforehand. `token
> list` renders the remaining time and flags expired entries, but only when
> someone runs it. If you set expiries, put the renewal in a calendar — do not
> rely on noticing.

Tokens without `--expires-in-days` never expire and are revoked explicitly. That
is the safer default for a service-to-service credential nobody is watching.

## Operational notes

- **Back up `tokens.toml`** with your config. It holds no secrets, but losing it
  means every token is gone and weave is locked out until you mint and
  distribute a new one.
- **A corrupt store is a hard failure**, never treated as "no tokens". The CLI
  refuses to overwrite a file it cannot parse, and the adapter will refuse to
  start rather than come up with an empty allow-list **(Phase 2)**.
- **`token list` output is safe to share** — no tokens, no hashes.
- **Nothing logs a token.** Not on success, not on failure, not at debug level.

## See also

- [cli.md](cli.md) — full command and flag reference
- [events.md](events.md) — the event catalog, including auth outcomes
