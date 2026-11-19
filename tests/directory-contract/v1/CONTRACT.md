# The directory contract, version 1

An **account service** holds people and their SSH login keys, and hands each
server enrolled with it the list of users that server admits. This document
is what an enrolled server (a **client** below; shed-server is one) and the
service may rely on, and what anyone implementing either side must do. It is
complete by itself: it refers to nothing outside `v1/`.

**Contract number: 1.** Every response body below that carries `contract`
carries `1`. A response carrying another number is a breaking change: a
client refuses it and keeps the copy it has. A new optional member does not
change the number; a breaking change is `v2/`, beside this one.

A client talks to the service in three steps, each a section below:

1. **Enrolment**, once: an operator gives the client a one-time code; the
   client makes a key pair and sends the code and its public key; the
   service answers with the client's client id.
2. **A token**, whenever it needs one: the client signs a short assertion
   with its key and exchanges it for a ten-minute access token (OAuth 2.0
   client credentials with a JWT client assertion).
3. **The directory**: the client sends the token and gets the whole list of
   users.

The numbered rules (section 4) apply throughout; the sections refer to them
as "rule N".

## Conventions

- HTTPS, JSON, UTF-8 (rule 4). Unknown members are ignored by both sides,
  except where a rule below refuses them.
- **Member names** are compared after JSON unescaping, wherever this
  document speaks of a member's name or of a member named twice:
  `"cont\u0072act"` names the member `contract`, so an object holding
  it beside `"contract"` names `contract` twice.
- **Every JSON answer.** A client holds every answer body it reads as JSON
  (the enrolment 201, the token 200, the directory 200, and every error
  body) to four checks, throughout the body, member names and members a
  reader ignores included, and a service's answers meet them: it is
  valid UTF-8; no string in it escapes an unpaired UTF-16 surrogate (a
  `\ud800` to `\udbff` escape not followed at once by a `\udc00` to
  `\udfff` escape, or a `\udc00` to `\udfff` escape that does not follow
  one: `\ud800` alone, say), while an escaped pair (`\ud83d\ude00`)
  is text like any other; it is one JSON object with nothing after it but
  whitespace; and it nests no deeper than 16 levels, counted as rule 6
  says. *Client:* invalid UTF-8 and an unpaired surrogate are refused,
  never repaired (by a replacement character or otherwise). An answer that
  fails a check is one the client cannot read: a token or directory answer
  so failing is a failed refresh; an error body so failing is an error the
  client does not recognise (below); and an enrolment 201 so failing is,
  for the client, a lost enrolment (section 1, "A lost answer"): it takes
  no client id from a body it cannot read. Rule 6 adds the directory
  answer's own checks.
- Times are RFC 3339 (section 5.6) instants in UTC, written with `Z` and
  whole seconds. (What a client accepts in a key's `expires` is rule 7's
  date-time.)
- **Errors on `/v1`** are `{"error":{"code":"<CODE>","message":"<text>"}}`,
  with the status the tables give. `code` is stable; `message` is for
  people and may change: a client never compares it.
- **Errors from the token endpoint** are OAuth's (RFC 6749, section 5.2):
  `{"error":"<code>","error_description":"<text>"}`. `error_description`
  is for people: a client logs it, quoted, and never compares it.
- *Client:* locates an error's code by exactly these member names, and
  by no other spelling or case: `error` at the body's top level, holding
  either a non-empty string (the OAuth shape, the code itself) or an
  object whose member `code` is a non-empty string (the `/v1` shape). A
  member of another spelling or case beside them (`Error`, `errors`,
  `CODE`) is ignored like any unknown member:
  `{"error":{"code":"SERVER_DISABLED","CODE":"x"}}` is `SERVER_DISABLED`.
  The client recognises no code in a body without those members, in a
  body that fails the checks every JSON answer meets (above), or in a body
  that names a member twice in either object it reads (the body's top
  level, or the object in `error`), whichever member that is: it never
  picks one of two candidates. An error it does not recognise is still a
  failed refresh (or a failed enrolment) by its status.
- Every answer on `/v1` and `/oauth2` carries `Cache-Control: no-store`.
- Cookies are never read on `/v1` or `/oauth2`.
- While the service is starting (its storage not yet checked), `/v1`
  answers 503 `UNAVAILABLE` and the token endpoint and the two documents
  beside it answer 503 `{"error":"server_error"}`. Retry later.
- `issuer` is the service's public URL (`https://accounts.example.test` in
  the fixtures): an origin, with no path and no trailing slash. The token
  endpoint is `<issuer>/oauth2/token`; the audience of a client's access
  token is `<issuer>/v1`.
- No answer on `/v1/*`, `/oauth2/*` or `/.well-known/*` is a redirect
  (rule 3).

## 1. Enrolment

An operator issues a one-time **enrolment code** for a server name, by
whatever means the service offers. A code is opaque to the client: a string
of at most 64 bytes. It is shown once, valid for a limited time (30 minutes
in the fixtures' world unless the operator chose otherwise, at most 24
hours), and spent by one enrolment. A service need keep only a hash of it.

On the server host (for shed, `shed-server directory enroll`), the client
reads the code from standard input or a prompt (never a command-line
argument, which shell history keeps), makes a new key pair, and sends:

```
POST /v1/servers/enroll
Content-Type: application/json

{"code": "<the code>", "public_key": {"kty":"OKP","crv":"Ed25519","x":"..."}, "name": "build-1.example.test"}
```

| Field | Rule |
|---|---|
| `code` | the enrolment code, a string of at most 64 bytes |
| `public_key` | the client's public key, a JWK (below), at most 2048 bytes as sent |
| `name` | a display name for the operator's audit trail (the host name, say): 1 to 255 printable ASCII characters. The server's name in the service is the one the operator issued the code for, not this |

The body is at most 8 KiB, and is one JSON object (never an array); a
member named twice, known or not, is refused; other members are ignored.
The service checks, in order: its rate limit (before the body is read), the
media type, the size, the JSON, each field's limits and the key, and only
then the code; a request refused before the code is looked up leaves the
code unspent.

**The public key** is exactly one of:

- EC P-256: `{"kty":"EC","crv":"P-256","x":"...","y":"..."}`, the point on
  the curve; its assertions are signed `ES256`;
- OKP Ed25519 (recommended): `{"kty":"OKP","crv":"Ed25519","x":"..."}`, a
  canonical encoding not of small order; its assertions are signed `EdDSA`
  (rule 11).

Coordinates are unpadded base64url of exactly 32 bytes. Optional members:
`kid` (a string), `alg` (the key's own: `ES256` or `EdDSA`), `use` (`sig`),
`key_ops` (`["verify"]`, or `verify` beside `sign`, none repeated). Refused:
any private member (`d`, `p`, `q`, `dp`, `dq`, `qi`, `k`; a client that
sends one has leaked its key), any other member, key type or curve, a member
named twice, nesting deeper than eight, and anything that is not one JSON
object. The service keeps the key as the JSON text exactly as sent.

**Answers** (all with `Cache-Control: no-store`):

| Status | Body | When |
|---|---|---|
| 201 | `{"contract":1,"client_id":"srv_<32 hex>","issuer":"<issuer>","token_endpoint":"<issuer>/oauth2/token","organization":"<slug>"}` | enrolled: the code is spent and the server exists |
| 400 `INVALID_REQUEST` | | the body is not a JSON object with the three fields and their limits, or did not arrive whole within 10 seconds |
| 400 `INVALID_PUBLIC_KEY` | | `public_key` is not a key as above |
| 401 `ENROLMENT_CODE_INVALID` | | no such code |
| 409 `ENROLMENT_CODE_SPENT` | | the code was used already |
| 410 `ENROLMENT_CODE_CANCELLED` | | the operator cancelled the code |
| 410 `ENROLMENT_CODE_EXPIRED` | | the code expired |
| 409 `SERVER_NAME_IN_USE` | | a live server already has the name the code was issued for; the code stays unspent |
| 409 `PUBLIC_KEY_IN_USE` | | an enrolled server already has this key; the code stays unspent |
| 413 `PAYLOAD_TOO_LARGE` | | the body is over 8 KiB |
| 415 `UNSUPPORTED_MEDIA_TYPE` | | the body is not `application/json` |
| 429 `RATE_LIMITED` | | the caller has made too many enrolment requests; `Retry-After` says when to try again (rule 2) |
| 503 `UNAVAILABLE` | | the service could not reach its storage, lost it while committing, or did not finish within 15 seconds: **the enrolment may or may not have happened**. Treat it as a lost answer (below) |

The client checks the 201: it meets the checks every JSON answer meets
(Conventions; a 201 that fails them is a lost answer, below); `contract`
is 1; `client_id` matches `^srv_[0-9a-f]{32}$`; `issuer` is the issuer it
was configured with, byte for byte; `token_endpoint` is
`<issuer>/oauth2/token`; `organization` is accepted (rule 5). It stores `client_id`, `issuer`, `token_endpoint` and
`organization` beside its key, and uses that token endpoint from then on
(rule 13). A failed enrolment discards the key pair it made.

**A lost answer** (a 201 that never arrived or that the client cannot
read, a 503, a timeout). If the
service committed the enrolment, the code is spent and the server exists,
under a client id the client never learned. Retrying with the same code then
answers `ENROLMENT_CODE_SPENT`, which after a lost answer means exactly
this. The operator finds that server among the service's servers (its name,
enrolment time and key thumbprint) and disables it, issues a new code, and
the client enrols again with a new key pair, each by whatever means the
service offers.

**Rotating a client's key, or bringing a disabled server back,** is a new
enrolment: a new code, a new key pair, a new client id. A disabled server
stays disabled.

## 2. A token

The client logs in with OAuth 2.0 client credentials (RFC 6749, section 4.4)
and a JWT client assertion (RFC 7521; RFC 7523, section 2.2):

```
POST <token endpoint>
Content-Type: application/x-www-form-urlencoded

grant_type=client_credentials
&client_assertion_type=urn%3Aietf%3Aparams%3Aoauth%3Aclient-assertion-type%3Ajwt-bearer
&client_assertion=<the assertion>
```

- Optional: `client_id` (equal to the assertion's if sent) and `scope`
  (absent means every scope the server has: `directory:read`; sent, scope
  tokens separated by single spaces).
- Never: `resource`, `audience` (400 `invalid_target`), `client_secret` or
  an `Authorization` header (401 `invalid_client`). `grant_type`,
  `client_assertion`, `client_assertion_type`, `client_id` and `scope` may
  each be sent once only (twice, whatever the values: 400
  `invalid_request`); other parameters are ignored. A parameter sent empty
  counts as absent.
- The body is at most 64 KiB; a larger one is a bare 413 (not the OAuth
  shape). A client's request is far smaller.

**The assertion** is a compact JWS (at most 4096 bytes) signed with the
client's private key:

- header: `alg` is `EdDSA` for an Ed25519 key or `ES256` for a P-256 key
  (fixed by the key: an assertion is checked with that one only); `typ` as
  rule 10 says; `kid`, if sent, a string, and equal to the JWK's `kid` when
  the JWK had one; no `jwk`, `jku`, `x5u`, `x5c`, `crit` or `b64` member,
  not even `null`;
- claims: `iss` and `sub` both the client id; `aud` as rule 10 says; `iat`
  and `exp` integers, `0 < exp − iat ≤ 60`; `nbf` optional; `jti` a string
  of 1 to 256 bytes, never used before;
- times are judged at one instant `t` of the service's clock with 30
  seconds of leeway (`L`): `t < exp + L`, `iat ≤ t + L`, `nbf ≤ t + L`,
  `iat ≥ t − 60 − L`. So a client whose clock runs more than 30 seconds
  ahead of the service's fails every login, and an assertion is refused
  once its `exp` is 30 seconds or more in the service's past (at exactly
  `exp + 30 s` it is refused). An assertion presented after its `exp` and
  less than 30 seconds after it is still in its window. Keep the clock
  synchronized (NTP);
- no JSON member named twice in the header or the claims.

An assertion is spent once, keyed on its client and `jti` (never its
bytes): the same assertion presented again is refused as a replay. A
service remembers each spent assertion at least until its `exp` plus the
leeway, the whole time in which it would otherwise be accepted (it may
forget it at `exp + 30 s`, when the window refuses it anyway).

**Answers:**

| Status | Body | When |
|---|---|---|
| 200 | `{"access_token":"<JWT>","token_type":"bearer","expires_in":600,"scope":"directory:read"}`, with `Cache-Control: no-store` and `Pragma: no-cache` | issued. Compare `token_type` without regard to case. No refresh token |
| 400 `invalid_request` | | a malformed request, one of the five parameters above sent twice, one of the two assertion parameters without the other, a body that is not a form |
| 400 `unsupported_grant_type` | | no `grant_type` value is `client_credentials` (checked first: `grant_type=password&grant_type=password` is this, not `invalid_request`) |
| 400 `invalid_scope` | | `scope` malformed or naming a scope the server lacks (the assertion is not spent) |
| 400 `invalid_target` | | `resource` or `audience` sent (the assertion is not spent) |
| 401 `invalid_client` | `{"error":"invalid_client","error_description":"<text>"}`, no `WWW-Authenticate` | any authentication failure: an unknown or disabled server, a bad signature, a wrong `aud`, an expired or replayed assertion, any refused header or claim. **Accept `invalid_client` as 400 or 401.** A failed refresh: the client keeps its copy until its trust time ends ("Keeping the list"), and logs loudly (a disabled server looks exactly like this) |
| 429 `temporarily_unavailable` | `{"error":"temporarily_unavailable","error_description":"<text>"}` | the caller has made too many token requests; `Retry-After` says when (rule 2) |
| 500 `server_error` | `{"error":"server_error"}` | the service failed while spending the assertion, or the request took more than 20 seconds: **the assertion may have been spent**. Sign a new one |
| 503 `server_error` | `{"error":"server_error","error_description":"<text>"}` | the service could not look the server up; nothing was spent |

Every 4xx and 503 carries `Cache-Control: no-store`.

A client takes the 200 only when it meets the checks every JSON answer
meets (Conventions) and carries `access_token` (a non-empty
string), `token_type` `bearer` (any case) and `expires_in` (a positive
integer), and `scope`, when present, includes `directory:read`. Anything
else is a failed refresh.

**The access token** is a JWT signed `RS256` by the service, with the header
type `at+jwt` (RFC 9068). Its claims: `iss` the issuer; `sub` and
`client_id` the client id; `aud` an array holding `<issuer>/v1`;
`gty: "client_credentials"`; `scp: ["directory:read"]`; `jti`; `iat` and
`nbf` the issue time; `exp` ten minutes later. A client treats the token as
opaque: it never verifies or decodes it (rule 13). A service may serve its
verification keys at `GET <issuer>/.well-known/jwks.json`, and RFC 8414
metadata at `GET <issuer>/.well-known/oauth-authorization-server` (the
issuer, the token endpoint, the JWKS, the grant `client_credentials`, the
method `private_key_jwt` with `ES256` and `EdDSA`, and the scope
`directory:read`; nothing a person would use).

**Retries.** A client signs a **fresh assertion, with a new `jti`, for
every attempt**, after any failure, and never sends one twice. A 429, a 5xx,
a timeout or a refused connection is a failed refresh: retry after 15
minutes, and never before `Retry-After` (rule 2), except that a refresh an
operator asks for explicitly may be sent sooner. On a 401, log the
response's `Date` header beside the client's own clock (a skewed clock is
the usual cause). Client timeouts: 30 seconds for a refresh (a background
one, one an operator asks for, or one an unknown key starts). A login by an
unknown key waits at most 8 seconds for the refresh it started, and is
refused if its key is still unknown when that wait ends. That refresh runs
on to its own bound and its result still applies, so a later attempt
succeeds once a pull has brought the key. A service that cannot answer a
token request and a directory request within about 8 seconds from cold
should expect first logins to be refused until one pull completes.

## 3. The directory

```
GET <issuer>/v1/directory
Authorization: Bearer <access token>
```

```json
{
  "contract": 1,
  "organization": "example",
  "version": "<64 lowercase hex>",
  "generated_at": "2026-11-16T06:00:00Z",
  "rules": { "version": 0 },
  "users": [ <user>, ... ]
}
```

| Field | Rule |
|---|---|
| `contract` | `1` |
| `organization` | the organization's slug (rule 5) |
| `version` | 64 lowercase hex digits, opaque; it changes whenever the list's content changes (a key added, removed or expired, a user removed, an admin flag changed). A client may log it once rule 6 has accepted it. There is no `ETag`, no `If-None-Match` and no 304: every pull returns the whole list |
| `generated_at` | when the service read the list |
| `rules` | reserved: `{"version": 0}` in this version; a client reads and ignores it |
| `users` | every user, ordered by `name` (bytewise) |

Every pull is one consistent snapshot: a change committed while it is read
appears whole or not at all.

**Answers** (all with `Cache-Control: no-store`):

| Status | When |
|---|---|
| 200 | the list above |
| 401 `invalid_token`, with `WWW-Authenticate: Bearer error="invalid_token"` | no bearer token, more than one `Authorization` header, a token that does not verify (expired or not yet valid, another issuer's, another audience's, a person's token, `sub` other than `client_id`, a server that no longer exists). Log in again, once, then treat it as a failed refresh |
| 403 `INSUFFICIENT_SCOPE` | the token lacks `directory:read` |
| 403 `SERVER_DISABLED` | the server was disabled while it still held a token. A failed refresh: keep the copy until the trust time ends ("Keeping the list"), log loudly |
| 429 `RATE_LIMITED` | the server has made too many directory requests; `Retry-After` says when (rule 2) |
| 503 `UNAVAILABLE` | the service cannot reach its storage. A failed refresh |

The service checks, in order: the token (one `Authorization: Bearer`
header; its own signature, header type and key id; the issuer; the
audience; `exp` and `nbf` with the leeway; `gty`; `sub` equal to
`client_id`), its scope, its rate limit, then the server's own record, read
on every request (gone: 401; disabled: 403), and read again as the first
read of the list's snapshot, so a list never holds a change committed after
the server was disabled. Every 401 carries the same body, whatever failed.

**A good pull replaces the kept list whole.** A user in the copy a client
kept and absent from the new answer is gone: their keys stop opening new
connections. That is how a removal or a lost key reaches a server. A
refresh that fails never shortens or empties the copy a client keeps.

### Keeping the list

A **good pull** is a 200 whose body rules 1 and 6 accept and whose
`organization` the client accepts (rule 5), and that holds no access token
the client presented in that pull (below). Anything else (any other status,
a redirect, a timeout, a body the client refuses) is a **failed refresh**.

**An answer never holds a client's credentials.** A service never puts a
client's access token, or any credential a client presented, in an answer.
A client may refuse, as a failed refresh, an answer that contains an access
token it presented in that pull, wherever in the answer it appears (in a
member a client ignores, in a user's name, anywhere else): that is the one
exception to the rule that one bad user never rejects the whole list
(below, under "The list is authoritative").

- **Refresh.** A client pulls on a schedule (every 24 hours by default, a
  setting of the client), when an admin asks for it, and at once when a key
  it does not know tries to log in: at most once a minute for a first login
  (for shed, a login as `_bootstrap`), the login waiting at most 8 seconds
  for it, and at most once every 5 minutes for a login to an existing shed
  (for shed, a login name that is the name of a shed on that server), the
  login not waiting. Each of those two rates is a bound across the whole
  client (one enrolled server), whatever the key, the person, the login
  name or the peer that offered the key; the two rates are independent of
  each other. A key it does not know, offered under any other login name,
  starts no pull. While refreshes fail, each of those two rates
  becomes at most once every 15 minutes. A pull an unknown key starts runs
  on to its own bound, and its result still applies. A failed refresh is
  retried every 15 minutes, never before a `Retry-After` (rule 2); a refresh
  an operator asks for explicitly may be sent sooner.
- **Trust time.** The kept copy authorizes logins for a fixed time after
  the **last good pull** (72 hours by default, a setting of the client),
  counted from when that pull's answer arrived. The client stores that
  instant with the copy. Only a good pull starts the trust time again: a
  failed refresh, a 401, a 403 `SERVER_DISABLED`, a restart, or reading the
  copy back from disk never does.
- **When the trust time ends** without a good pull, the copy lets nobody
  in: only the client's own local admins (keys in its configuration,
  independent of the service) are accepted, and the client says why in its
  log and its status. The next good pull restores the list. So a client cut
  off from the service, or disabled in it, stops admitting a removed person
  at the latest one trust time after its last good pull.
- **Start-up.** With a kept copy still inside its trust time, a client uses
  it at once and refreshes in the background. With none, or one past its
  trust time, only the local admins work until a good pull.
- **A key's `expires`** is enforced by the client at that instant, on its
  own clock, whether or not a refresh has happened since and whatever the
  trust time: an expired key never opens a connection. Expiry is checked
  when a key is used, never when the list is read: classifying an answer
  (rule 7) never depends on the clock. A key whose `expires` is already
  past stays in its user's keys and is counted for duplicates, and a user
  whose every key has expired is still a listed user; none of those keys
  opens a connection.

### Users

Each user is (rule 8):

```json
{
  "name": "ada",
  "admin": true,
  "keys": [
    { "key": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...", "label": "laptop" },
    { "key": "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAA...", "label": "yubikey", "expires": "2027-01-01T00:00:00Z" }
  ]
}
```

| Member | Type | Rule |
|---|---|---|
| `name` | string | The user name on every server: `^[a-z][a-z0-9-]{0,62}$`. Never reused for another person, even after the person is removed. Servers know a person only by this name, so a service that changes a person's name has made, to every server, a new user. Never `root`, `admin`, `git`, `shed`, `system` or `nobody`; an implementation may refuse further names. Unique in the list |
| `admin` | boolean | Whether the user is an admin on the server that pulled the list |
| `keys` | array | Always present. Each key's `key` is the canonical line below; `label` as rule 9 says; `expires`, when present, is the instant the key stops being valid, an RFC 3339 date-time (section 5.6) in UTC with `Z` and nothing else (omitted when it never expires; rule 7 gives what a client accepts). Ordered by `label` (bytewise). No key is in the list twice, under one user or two (a key's identity is its key blob, rule 7) |

**Who is listed** is the service's own business. Who is listed, and
whether as admin, may differ from one requesting server to another. A
service never lists a user to a server when the user's access there
depends on a restriction that server has not declared it enforces (in this
version a server declares none: `rules` is reserved): it omits the user.
It never lists a user with no key, nor a key that has expired; a user
holds at most 20 keys.
Nothing else about a person is specified: a service with more to say says
it in members a client ignores (rule 8).

**Mapping to shed's user configuration** (`auth.users` entries):

| Contract | shed |
|---|---|
| `name` | the `auth.users` key |
| `admin` | `Admin` |
| `keys[].key` | `UserKeyConfig.Key` |
| `keys[].label` | `Label` |
| `keys[].expires` | `Expires`: a date-time as rule 7 gives it and nothing else. A bare date, which shed's own configuration accepts, is not one: a key whose `expires` is a date is not used (rule 7) |
| — | `Script`: never sent; absent means false |

**The list is authoritative, empty or not.** `users: []` means nobody but
the client's own local admins may log in; it never means "no user list". A
list with no admin is valid. A user a client cannot accept (rule 7, and the
client's own rules: for shed, a name equal to one of its service clients or
one of its local admins, or a key a local admin holds) is skipped and
logged; the rest of the list stands. Whether a listed key is one a local
admin holds is decided by the same identity rule 7 uses for duplicates:
the key blob, never the text of the line. A client never discards the whole list
for one bad user, and never passes the list through a path where an empty
list means "unconfigured" or a list without an admin fails to load. The one
exception: an answer that contains an access token the client presented in
that pull, in a user or anywhere else, may be refused whole, as a failed
refresh ("An answer never holds a client's credentials", above).

### Reserved

`POST /v1/deliveries` is reserved for a later version and not specified
here. Nothing in version 1 writes anything into a server or a shed.

## 4. Rules

Each rule states what a service does and, apart, what a client does. A
client's own rules are sometimes more permissive than what a service sends;
a service never relies on that.

1. **Sizes.** *Service:* never serves a directory body over 8 MiB, nor a
   token or enrolment answer over 64 KiB. *Client:* refuses a larger one,
   counted on the bytes it reads (with no content decoding, so the bytes
   read are the body's). It may read one byte past the limit, to learn that
   the body is larger than the limit (a body of exactly the limit is
   accepted), and reads no further. An error body is read to at most 4 KiB.
2. **`Retry-After`.** *Service:* sends it only on 429 and 503, and only as
   delta-seconds (a non-negative integer), once. *Client:* reads it only
   on a 429 or a 503 and ignores it on any other status. It accepts
   exactly this: the header given once, whose value, after optional
   whitespace (spaces and tabs) is trimmed from both ends, is one or more
   ASCII digits (`0` to `9`) and nothing else. That is a number of
   seconds; the client waits at least that long, up to one hour, and
   treats a larger value (however many digits it has) as one hour. A
   refresh its operator asks for explicitly may be sent sooner.
   Anything else is treated as absent: an HTTP date, a sign (`+` or `-`), a
   decimal point, any other character, an empty value, or the header given
   more than once. Absent, the 15-minute retry applies.
3. **Redirects.** *Service:* never redirects `/v1/*`, `/oauth2/*` or
   `/.well-known/*`. *Client:* never follows one: any 3xx is a failed
   refresh or a failed enrolment.
4. **Transport.** HTTPS with ordinary certificate checks. *Client:* may
   allow plain HTTP only to a loopback destination, and only when its
   operator asks for it explicitly.
5. **The organization** matches `^[a-z][a-z0-9-]{0,62}$`. *Client:* with an
   organization configured, refuses an answer naming another; with none
   configured, compares with the organization its enrolment answer named.
6. **The directory answer's top level.** `contract` present and written
   as the integer 1 exactly: the one character `1` (`1.0`, `1e0` and every
   other spelling of the number one are refused); `organization` per rule
   5; `version` 64 lowercase hex digits; `users` an array; no member named
   twice at the top level. And, as for every JSON answer (Conventions),
   throughout the body, members a reader ignores included: valid UTF-8; no
   escaped unpaired UTF-16 surrogate; one JSON object with nothing after
   it but whitespace; nesting no deeper than 16. Each object and each
   array adds a level: the top-level object is level 1, an object or array
   that is a member's value or an array's element is one level deeper than
   what holds it, and other values add none; members a reader ignores
   count like any other. *Client:* any failure, of these checks or of the
   ones every JSON answer meets, refuses the whole answer (a failed
   refresh). Invalid UTF-8 and an unpaired surrogate are refused, never
   repaired (by a replacement character or otherwise). A number too large
   for a reader to hold (`1e400`, an integer of forty digits) is not a
   failure of this rule: it is judged only where a reader reads the member
   that holds it, by that member's own check (`contract` so written is not
   the integer 1; in an entry it is a value of the wrong type, rule 7), and
   inside a member a reader ignores it is ignored. It never refuses the
   answer by itself.
7. **Each user is decoded alone.** A user is an object with `name`
   (string), `admin` (boolean) and `keys` (array); each key an object with
   `key` (string), `label` (string) and optional `expires` (string).
   Members a reader does not know are ignored. A bad entry skips that
   user, never the list.

   **Members named twice** (their names compared after unescaping,
   Conventions) are looked for in three places only: the answer's top
   level (rule 6), each user object and each key object. A
   member named twice inside a member a reader ignores (`rules`, or a
   member a service adds to a user or a key), at any depth, is not looked
   at: it neither refuses the answer nor skips the entry.

   **A number too large for a reader to hold**, as the value of a member
   the reader reads in an entry (`admin`, say), is a value of the wrong
   type: the entry is `malformed`. As the value of a member the reader
   ignores, or anywhere inside one, it is ignored. It never refuses the
   answer (rule 6).

   **A key's identity is its key blob**: the algorithm and the key bytes
   the base64 field decodes to, never the text of the line. Two lines that
   differ only by a comment or by whitespace are the same key. (For shed,
   two keys are the same when their SHA-256 fingerprints are equal.) The
   same identity decides whether a listed key is one a client's local
   admin holds.

   **A key is used** when the reader's parser accepts `key`, `label` is as
   rule 9 says, and `expires` is absent or a date-time as below; a bare
   date, or a date-time without an offset, is not one. A key that is not
   used is not an error: the user stands on its other keys. Whether
   `expires` is past is not part of this (see "Keeping the list").

   **The date-time of `expires`.** *Service:* writes an RFC 3339 date-time
   (section 5.6) in UTC with `Z`, in whole seconds. *Client:* accepts RFC
   3339's date-time with these limits, and nothing else: `YYYY-MM-DD`, an
   upper-case `T`, `hh:mm:ss`, then optionally a fraction (a `.` and one
   or more digits; nothing but a dot introduces one), then an upper-case
   `Z` or a numeric offset `+hh:mm` or `-hh:mm`. Every field has exactly
   its digits (four for the year, two for each other) and is within its
   range: the month 01 to 12, the day within its month (leap years
   counted), the hour 00 to 23, the minute and the second 00 to 59 (a
   second of 60, a leap second, is never accepted), the offset's hour 00
   to 23 and its minute 00 to 59. An offset of `-00:00` is accepted, as the
   same instant as `Z`. A lower-case `t` or `z` (which RFC 3339 itself
   allows), a one-digit field, an offset of `+24:00`, anything after the
   `Z` or the offset, or anything else is not a date-time here: that key is
   not used. A client keeps at least the whole seconds of an accepted
   `expires`, and may drop the digits of a fraction beyond what it can
   represent (shed keeps nanoseconds: `.123456789123` is kept as
   `.123456789`).

   **Counting.** Names and keys are counted in every entry that holds
   them, whether or not that entry is skipped for another reason: an
   entry's `name` when it is a string named once in the entry, and each
   key whose `key` is a string named once, in a `keys` array named once,
   that the reader's parser accepts, used or not.

   **Each entry is checked in this fixed order**, and the first check it
   fails skips it with that reason:

   1. *Structure.* A member named twice inside the entry (in the user or
      in one of its keys): `duplicate-member`. Otherwise, an entry that is
      not an object, a wrong type, a `null`, or a missing required member
      (in the user or in one of its keys): `malformed`.
   2. *Name.* A name the reader does not accept (for shed, one outside
      `^[a-z][a-z0-9-]{0,62}$`): `invalid-name`.
   3. A name counted in another entry too: `duplicate-name`.
   4. A key counted in another entry too, or twice in this one:
      `duplicate-key`.
   5. No key the reader uses: `no-usable-key`.

   A client's own further rules (section 3, "The list is authoritative")
   apply to the entries these checks leave. The checks are
   order-independent: the same entries in another order give the same
   result.
8. **A user is a name, an admin flag and keys.** No other member is
   specified. A service with more to say about a person says it in members
   a reader ignores.
9. **Labels.** *Service:* sends 1 to 64 bytes of UTF-8 with no control
   character, unique within the user. *Client:* needs only a non-empty
   label, unique within the user (keys of a user that share a label are
   none of them used), and quotes labels when it prints them. Uniqueness
   counts every element of the user's `keys`, a key the reader does not
   use for another reason included (a line its parser refuses, an
   `expires` that is not a date-time): two elements that share a label are
   neither of them used, and the user stands on its other keys.
10. **The assertion.** *Service:* accepts `typ` absent, `JWT`, or
    `client-authentication+jwt` (in any case, with an optional
    `application/` prefix), and refuses every other `typ` (`at+jwt`
    among them); accepts `aud` as the issuer string or a one-element array
    holding it, byte for byte, and never an array of more elements (even
    one holding the issuer), the token endpoint or the issuer with a
    trailing slash. *Client:* sends `typ: "client-authentication+jwt"` and
    `aud` as the issuer string.
11. **Ed25519.** *Service:* may refuse, at enrolment, an `x` that does not
    decode to a curve point, or enrol it and then refuse every assertion
    for it (`invalid_client`); both conform. A key made by a real Ed25519
    implementation is always a curve point.
12. **Placeholders**, so that two implementations pass one fixture set:
    `{{message}}` (any non-empty string, in every `/v1` error's `message`),
    `{{description}}` (any non-empty string, in every OAuth
    `error_description`), `{{version}}` (64 lowercase hex digits; equal at
    every step of a scenario where the list did not change, different where
    it did). Placeholders are never widened to let an implementation pass.
13. **The client** uses the token endpoint its enrolment answer returned,
    sends the access token as an opaque bearer, and fetches no metadata or
    key set. *Service:* may serve RFC 8414 metadata and a JWKS; the
    recorded `metadata` and `jwks` exchanges hold for a service that does.
14. **Standards** are cited in this document only for what they say:
    RFC 3339 (section 5.6, the date-time format); RFC 6749 (section 4.4,
    the client credentials grant; section 5.2, the error response); RFC
    7521 (the assertion framework, `client_assertion` and
    `client_assertion_type`); RFC 7523 (section 2.2, the
    `client_assertion_type` value for a JWT); RFC 9068 (the access token's
    `at+jwt` type and claims); RFC 8414 (the metadata document and its
    well-known path). Rule 10's `aud` and `typ` rules are this contract's
    own and stand without a citation.

## Keys

Every `key` is exactly one SSH public key, in one canonical form:

```
<type> <base64 of the canonical key blob>
```

no options, no comment, no newline: Go's `ssh.MarshalAuthorizedKey` of the
key without its trailing newline. The service stores only keys whose blob
is already canonical, so this is the line every Go reader re-marshals the
key to, and its fingerprint (`SHA256:` and the unpadded base64 of the
SHA-256 of the blob, Go's `ssh.FingerprintSHA256`, what `ssh-keygen -lf`
prints) is the one a client computes. One key never appears in two
encodings.

| Type | Accepted |
|---|---|
| `ssh-ed25519`, `sk-ssh-ed25519@openssh.com` | 32-byte keys |
| `ecdsa-sha2-nistp256`, `ecdsa-sha2-nistp384`, `ecdsa-sha2-nistp521`, `sk-ecdsa-sha2-nistp256@openssh.com` | the curve the type names; the point uncompressed (`0x04 ‖ X ‖ Y`), of the curve's exact length, on the curve |
| `ssh-rsa` | modulus of 2048 to 16384 bits; exponent odd, at least 3, at most 24 bits; both positive, minimal mpints |
| `ssh-dss`, `ssh-mldsa-*` | never |
| certificates (`*-cert-v01@...`) | never |

A security key (`sk-`) carries its application string (UTF-8). A blob holds
nothing after the key.

**What the service accepts from a person** (a line pasted from a `.pub`
file): leading and trailing whitespace is trimmed (as Go's
`strings.TrimSpace` does, so a trailing newline passes); the line is then
one line of at most 16 KiB with no control character but a tab; the first
field (to the first space or tab) is the type; the second, after spaces and
tabs, is standard padded base64 with zero padding bits; anything after it is
a comment and is dropped, whatever it holds. Options (`no-pty ...`,
`from="..."`) are refused. Each refusal has a stable code; the corpus lists
them.

**Whose key it is.** The service is not required to check that a key
belongs to the person who added it. If it does not, a person can add a
colleague's public key, which the colleague then cannot add; a server sees
that key under the person who added it. An operator removes such an entry
by removing that person.

### The key corpus

`fixtures/keys/` holds every case the service's validator decides, as
files a reader can run through its own parser:

- `accept/<name>.json`: `{"line", "canonical", "fingerprint", "algorithm"}`:
  the service accepts `line` and stores `canonical`, of type `algorithm`,
  with `fingerprint`.
- `refuse/<name>.json`: `{"line", "code", "shed_also_refuses"}`: the
  service refuses `line` with `code`; `shed_also_refuses` says whether
  shed's `ParseUserKey` refuses it too (where it is false, the service is
  stricter on purpose: DSA and ML-DSA keys, RSA under 2048 bits,
  non-positive or zero-padded RSA numbers, non-zero base64 padding bits,
  control characters in a comment, lines over 16 KiB, a non-UTF-8
  security-key application, and an options field of commas only).
- `sources/*.pub`: the throwaway public keys, made with `ssh-keygen`, the
  entries start from (no private key exists).

`canonical`, `fingerprint`, `algorithm` and `shed_also_refuses` are
recorded from shed's own parser, never written by hand. A client checks,
for every `accept` entry, that its parser accepts `line`, re-marshals it to
exactly `canonical` and fingerprints it as `fingerprint`; and, for every
`refuse` entry, that it refuses `line` exactly when `shed_also_refuses` is
true.

## Limits

| What | Limit |
|---|---|
| Enrolment body | 8 KiB, arriving whole within 10 seconds |
| Enrolment code | 64 bytes as sent; valid for a limited time, at most 24 hours; spent once |
| Public key (JWK) | 2048 bytes as sent |
| Enrolment `name` | 1 to 255 printable ASCII characters |
| Token request body | 64 KiB |
| Assertion | 4096 bytes; `exp − iat` at most 60 seconds; 30 seconds of clock leeway |
| Access token lifetime | 600 seconds |
| Keys per user | 20 |
| Directory body | 8 MiB (rule 1) |
| Token and enrolment answers | 64 KiB (rule 1) |
| Error body, as a client reads it | 4 KiB (rule 1) |
| `Retry-After`, as a client honours it | one hour (rule 2) |

A service limits the rate of enrolment, token and directory requests as it
sees fit, and answers 429 with `Retry-After` when a limit is reached; how it
counts callers is its own.

## The fixtures

`fixtures/` holds what both sides test against. Every file in `v1/` but the
manifest is listed with its SHA-256 in `MANIFEST.sha256` (`<hex>  <path>`
lines, sorted by path), so a copy can be checked whole.

- `world.json`: the state every exchange starts from: the organization
  `example`, the issuer `https://accounts.example.test`, the service's
  clock at `2026-11-16T06:00:00Z`, the users as the directory serves them
  (`{name, admin, keys}`), two servers (one live, one disabled) and
  enrolment codes in each state. Codes appear in the clear because they are
  test data.
- `test-keys/`: throwaway keys, public by design: `server-ed25519` (the
  live server `srv-a`), `server-p256` (the disabled `srv-b`),
  `enrol-ed25519` (a key no server holds, for enrolments), each a PKCS#8 PEM
  private key and its public JWK (`*.jwk.json`, exactly the text a client
  sends); and `token-signing-key.pem`, the RSA key the service signs access
  tokens with in the fixtures, under the key id `test-signing-1`.
- `exchanges/*.json`: what an implementation of the service answers
  (below).
- `client/*.json`: answers a client must classify (below).
- `keys/`: the key corpus (above).

### Scenario time

A scenario runs on the service's clock: the world's `clock`, until a
`clock.set` step moves it. Every instant the service judges or writes (an
assertion's window, a code's expiry, `generated_at`, a key's expiry, an
access token's `iat`) is on that clock, and a harness signs each assertion
at it.

### Exchanges

Each file is one scenario run against a fresh copy of the world:

```json
{
  "name": "<file name without .json>",
  "description": "<what it shows>",
  "before": [ <set-up steps> ],
  "assertion": { "key": "server-ed25519", "client_id": "srv_..." },
  "request": { "method": "GET", "path": "/v1/directory", "headers": { "Authorization": "Bearer {{access_token}}" }, "body": null },
  "response": { "status": 200, "headers": { "Content-Type": "application/json", "Cache-Control": "no-store", "Pragma": null, "WWW-Authenticate": null, "Retry-After": null, "ETag": null }, "body": { } },
  "then": [ { "assertion": { "reuse": true }, "request": { }, "response": { } } ]
}
```

This is a closed language: a scenario's members are `name`,
`description`, `before`, `assertion`, `token`, `request`, `response` and
`then`; a step in `then` has only `before`, `assertion`, `token`,
`request` and `response`; a request has `method`, `path`, `headers` and
`body`; a response has `status`, `headers` and `body` (or `text`). A
harness refuses a fixture that uses any member, set-up step or
placeholder this section does not name, rather than giving it a default
(a misspelt `nbf_ofset` read as zero would sign a valid token for a case
that expects a refusal).

- `assertion` (optional) says what `{{assertion}}` is in this step: a fresh
  one, `{"key": "<test-keys file stem>", "client_id": "<id or
  {{client_id}}>"}` (once a client id is bound, it must be that one), or
  `{"reuse": true}`, the scenario's previous assertion byte for byte (a
  replay). A fresh one may also carry, each changing only what it names:
  `typ` (the header's `typ`, a string, or `null` for no `typ` member;
  default `"JWT"`), `aud` (any JSON value; default the issuer string),
  `lifetime` (`exp − iat` in seconds; default 60), `iat_offset` (seconds,
  an integer: `iat` is the scenario's clock plus this, and `exp` is `iat`
  plus `lifetime`; default 0), `nbf_offset` (seconds, an integer: the
  claims carry `nbf` = `iat` + this; default no `nbf` claim) and
  `duplicate_claim` (a claim name: the claims object names that member a
  second time, with the same value). No other member is part of this
  language.
- `token` (optional, instead of `assertion`): `{"client_id": "<id>",
  "scope": ["<scope>", ...]}`, optionally with, each changing only what it
  names: `nbf_offset` (seconds; default 0), `sub` (default the client id),
  `aud` (any JSON value; default `["<issuer>/v1"]`), `gty` (default
  `"client_credentials"`), `iss` (default the issuer), `typ` (the
  header's `typ`; default `"at+jwt"`), `kid` (the header's `kid`, a
  string; default `"test-signing-1"`) and `corrupt_signature` (only
  `true`: the lowest bit of the signature's first byte is flipped after
  signing). No other member is part of this language. `{{access_token}}`
  in this step's request is then a token the harness signs itself with
  `test-keys/token-signing-key.pem`, exactly as the service signs a
  client's (the `{{access_token}}` rule below), but for that client id and
  scope, with `nbf` = `iat` + `nbf_offset` and each other member as given:
  for a case the service's token endpoint never issues, such as a token
  without `directory:read`, or one valid in every claim but one.
- `before` (optional, on the first step or any step in `then`) runs set-up
  steps against the world before that step's request. Each is named by
  what it makes true, an object with one member, and these six are the
  only ones:
  - `{"users.remove": "<name>"}`: the user is no longer listed;
  - `{"users.set_admin": ["<name>", true | false]}`: the user's admin flag
    is that value;
  - `{"servers.disable": "<server name>"}`: the server is disabled;
  - `{"clock.set": "<RFC 3339>"}`: the service's clock is that instant;
  - `{"lookups.fail": true}`: every lookup the service makes fails, as in a
    storage outage (the service has finished starting);
  - `{"limits.exhaust": "<limit>"}`: the caller of the step's request has
    used the named limit's whole allowance for the current window, and
    that window has exactly 60 seconds left, so the 429 carries
    `Retry-After: 60`: `enroll`, `token`, or `directory` (for the
    scenario's server).
- `request.body` is a JSON value (sent as JSON), a string (sent as it is:
  a form body), or `null` (no body). `request.headers` are sent as given.
- `response.headers` lists exactly these six, each with its exact value
  (every value of a repeated header, joined with `, `), or `null` for
  "absent": `Content-Type`, `Cache-Control`, `Pragma`, `WWW-Authenticate`,
  `Retry-After` and `ETag`. Other headers, `Date` among them, are not
  compared.
- `response.body` is compared as canonical JSON (sorted keys, no
  whitespace): every value exactly, except a placeholder in a place the
  table below allows (and there the placeholder must be, so its rule always
  runs). `null` means an empty body. A body that is not JSON is given as
  `response.text` instead, compared exactly, with `body` `null`.
- `then` (optional) are further steps of the same scenario, in order;
  values bound by a placeholder carry over.

**Placeholders.** A placeholder is a whole JSON string `"{{name}}"` (or,
in a request, a part of a header value or a string body). Nothing else is
normalized away; any other placeholder is an error.

| Placeholder | Where | Rule |
|---|---|---|
| `{{client_id}}` | the 201 body of `POST /v1/servers/enroll`, member `client_id` | matches `^srv_[0-9a-f]{32}$`; bound for the rest of the scenario, where every use (in later requests, an assertion's `client_id`) is that value |
| `{{assertion}}` | a token request's `client_assertion` | made when the step runs, after its `before` steps, from the step's `assertion` member (or, with `reuse`, the previous one again): the private key `test-keys/<key>.pem`; header `{"alg":"EdDSA" or "ES256" (by the key),"typ":"JWT"}` unless the step says otherwise; claims `iss` = `sub` = `client_id`, `aud` = the issuer string, `iat` = the scenario's clock plus `iat_offset`, `exp` = `iat` + `lifetime`, `nbf` only with `nbf_offset`, `jti` fresh and unique, each unless the step says otherwise |
| `{{access_token}}` | the 200 body of `POST /oauth2/token`, member `access_token` | a compact JWS with header `alg` `RS256`, `typ` `at+jwt` and `kid` `test-signing-1`, whose signature verifies under `test-keys/token-signing-key.pem`'s public key; claims `iss` = the issuer, `sub` = `client_id` = the scenario's client id, `aud` an array holding `<issuer>/v1`, `gty` = `client_credentials`, `scp` = `["directory:read"]`, `nbf` = `iat`, `exp` − `iat` = 600, `iat` within one second of the scenario's clock, `jti` non-empty and not seen before in the run. Bound for later requests (`Authorization: Bearer {{access_token}}`) |
| `{{message}}` | every `/v1` error body, member `error.message` | any non-empty string (rule 12) |
| `{{description}}` | every OAuth error body that has an `error_description` | any non-empty string (rule 12) |
| `{{version}}` | every directory 200 body, member `version` | 64 lowercase hex digits; equal at every step of a scenario where the list did not change, different where it did (rule 12) |

Everything else the service computes from its clock and the world
(`generated_at`, `expires`, the JWKS of the committed signing key, every
error body but its `message` or `error_description`) is exact. A client's
tests can serve these exchanges as a fake service: answer each request
with its response, filling `{{client_id}}` with a value of the pattern,
`{{access_token}}` with any opaque string, and each other placeholder with
a value of its rule.

**The exchanges.** The token endpoint (`token-ok`, `token-bad-signature`,
`token-unknown-client`, `token-disabled-server`, `token-replay`,
`token-replay-within-leeway` (a replay inside the leeway refused, and a
fresh assertion with the same window then accepted),
`token-exp-leeway`, `token-iat-future-leeway`, `token-nbf-leeway` (fresh
assertions at and just outside each time boundary), `token-unavailable`,
`token-rate-limited`, `token-aud-array`, `token-aud-array-two-elements`,
`token-aud-trailing-slash`, `token-aud-token-endpoint`,
`token-typ-client-authentication`, `token-typ-jwt-lowercase`,
`token-typ-at-jwt`, `token-exp-window-too-long`, `token-duplicate-claim`,
`jwks`, `metadata`); enrolment (`enroll-ok`, `enroll-code-invalid`,
`enroll-code-spent`, `enroll-code-expired`, `enroll-code-cancelled`,
`enroll-invalid-public-key`, `enroll-public-key-in-use`,
`enroll-server-name-in-use`, `enroll-rate-limited`); the directory
(`directory-ok`, `directory-version-changes` (two equal versions, a
change, a different version, then the clock moved forward with nothing
changed and the same version), `directory-last-person-removed`
(`users: []`), `directory-last-admin-removed` and
`directory-last-admin-demoted` (a list with no admin),
`directory-no-bearer`, `directory-invalid-token` (an expired token),
`directory-foreign-token` (a person's token from another issuer),
`directory-cookie-only`, `directory-server-disabled` (disabled after its
token was issued), `directory-rate-limited`,
`directory-insufficient-scope`, and tokens shaped as the service's own
that fail one check each: `directory-token-not-yet-valid`,
`directory-token-subject-mismatch`, `directory-token-unknown-server`,
`directory-token-wrong-audience`, `directory-token-wrong-grant`,
`directory-token-wrong-issuer`, `directory-token-wrong-type`,
`directory-token-bad-signature` (every claim valid, the signature
corrupted) and `directory-token-wrong-key-id` (signed correctly, naming a
key id the service does not have)).

The two literal tokens are fixed strings (RS256 signatures are
deterministic). `directory-invalid-token`'s is signed with
`test-keys/token-signing-key.pem` (header `{"typ":"at+jwt","alg":"RS256","kid":"test-signing-1"}`;
claims `aud` `["https://accounts.example.test/v1"]`, `client_id` and `sub`
`srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a`, `exp` 1767226200, `gty`
`client_credentials`, `iat` and `nbf` 1767225600, `iss`
`https://accounts.example.test`, `jti`
`test-srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a-1767225600`, `scp`
`["directory:read"]`: expired on 2026-01-01). `directory-foreign-token`'s is
signed with a throwaway RSA key whose private half was discarded (key id
`foreign-test-1`, issuer `https://signin.example.test`, `aud`
`["accounts"]`, `client_id` `accounts`, `jti` `foreign-access-token`): it
stands for a sign-in provider's token issued to a person.

### Client fixtures

Each file in `client/` is one answer to `GET /v1/directory` and how a
client must classify it:

```json
{
  "name": "<file name without .json>",
  "description": "<what it shows>",
  "response": { "status": 200, "headers": { "Content-Type": "application/json" }, "body": { } },
  "expect": { "result": "good", "users": [ ], "skipped": [ { "entry": 1, "reason": "<reason>" } ] }
}
```

- The client classifying it accepts the organization `example` and
  contract 1. Classification does not depend on the clock (rule 7).
- `response.headers` are sent as given: a header's value is a string, or
  an array of strings each sent as its own header line (a header given
  more than once). The body is one of: `response.body`, a JSON value (sent
  as JSON); `response.text`, sent byte for byte as its UTF-8, for a body
  that is not one JSON value or that names a member twice, or whose exact
  text is the point (a number written `1.0` or `1e400`, an escape such as
  `\ud800`), which a harness reading `body` as a JSON value might rewrite;
  or
  `response.body_base64`, the body's exact bytes in padded standard base64,
  for a body a JSON string cannot carry (bytes that are not UTF-8). A
  `response` with none of the three has no body.
- `expect.result` is `good` (a good pull) or `failed` (a failed refresh).
- For `good`: `expect.users` are the users the client admits, in name
  order, each `{name, admin, keys}` with only the keys it uses, as the body
  sent them; `expect.skipped` lists every skipped entry, ordered by its
  index in `users` (counting from 0), as `{"entry": <index>, "reason":
  <reason>}`, the reason the first check of rule 7 the entry fails:
  `duplicate-member`, `malformed`, `invalid-name`, `duplicate-name`,
  `duplicate-key` or `no-usable-key`. Entries are identified by index, not
  name, because a skipped entry may have no name or share one.
- For `failed`: `expect.retry_after_seconds` is the least time, by rule 2,
  the client waits before its next try (the next try may come later, on
  the client's own schedule: it is a minimum, not the next attempt's
  time), or `null` where it must treat `Retry-After` as absent.
- No other member is part of this language, and a client's tests refuse a
  fixture that uses one.

An oversize body (rule 1) is generated by a client's tests, not stored.
