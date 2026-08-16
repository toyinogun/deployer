# 0024. Rationale: OAuth for clients that will not hold a token

The build spec is in [index.md](index.md). This file is the decision record and nothing in a build reads it.

## Context

Spec 0023 gave a newly verified person one page holding a finished configuration block per client. Every one of those blocks works the same way: a static `Authorization: Bearer` header, which is the only credential `internal/mcp/middleware.go` knows how to read. That covers the command line clients and it covers nothing else.

The connector surfaces do not offer a header field. A person adding a server in the Claude desktop app, on claude.ai, in Claude mobile or in Cowork pastes a URL and nothing more. The client then asks the server how to sign in, following the MCP authorization specification: a `401` carrying `WWW-Authenticate: Bearer resource_metadata=...`, a protected resource metadata document naming an authorization server, that server's own metadata naming an authorize endpoint, a token endpoint and a registration endpoint, then OAuth 2.1 with PKCE. Deployer serves none of that, so it cannot be added there at all. There is no field to fill in wrong, which is why the failure gave no useful signal.

Three forces already in this codebase shape any answer.

The session cookie carries the `__Host-` prefix, so it is locked to one hostname, the console. A person can only approve something on the console host. The thing being protected is on the deploy host. Spec 0021 put those two apart deliberately and spec 0022 kept them apart, so any design that needs a signed in person and a protected resource is a design across two public origins.

Route registration is opt in on both public hostnames and a catch all answers 404 for the rest. Every endpoint this feature adds is a deliberate registration on exactly one of those patterns, and the direction that fails is the private one.

And the platform already owns everything an authorization server needs: accounts, sessions, CSRF, an audit log, a per address limiter, an account lockout, a suspension switch and a token table with a hash column and a live name index. The question was never how to build identity. It was whether to reuse the identity that exists.

Not deciding leaves the platform usable only by people who can find a configuration file, which is the exact audience spec 0023 was written to stop excluding.

## Options considered

### Option 1: Deployer becomes its own authorization server

The platform serves the discovery documents, a registration endpoint, an approval page on the console and a token endpoint. A grant issues an ordinary `api_tokens` row.

**Pros**:
- One identity. The account that signs in to approve is the account the token belongs to, with no linking table and no second source of truth.
- Every existing rule applies for free: suspension, the lockout, the limiter, the audit log, the token list and its revoke button.
- Nothing new leaves the cluster. The control plane makes no outbound request as part of the flow.
- Works for any specification compliant client, not just Claude, because it is the specification that is being implemented rather than one vendor's behaviour.

**Cons**:
- It is the most code by a distance: two tables, five endpoints, a consent page and a class of security rules the codebase has never had to hold, chiefly open redirect and code replay.
- Writing an authorization server is a well known way to get security subtly wrong, and this one has no library behind it.

### Option 2: Cloudflare Access in front of the deploy hostname

The tunnel already exists. Access can gate the hostname and issue its own tokens.

**Pros**:
- Almost no Go code, and the identity plumbing is someone else's problem.
- It arrives through infrastructure that is already deployed and already trusted.

**Cons**:
- It authenticates a Cloudflare identity, not a deployer account, so the platform still has to map one to the other. That mapping is the account linking this option was supposed to avoid.
- Every rule in `internal/auth` gains a second entrance. The suspension switch, the app cap and the ownership checks would each need to hold on a path that does not go through `Authenticate`.
- It is not the MCP authorization flow, so a client discovering the server still finds no metadata and still cannot connect itself.

### Option 3: An external identity provider

Hand sign in to a hosted provider and accept its tokens.

**Pros**:
- The authorize page, the consent screen and the token endpoint are all built and audited by someone else.
- Refresh, rotation and revocation come with it.

**Cons**:
- Account linking against an `accounts` table that already exists and already works, for a platform with one user.
- A paid third party lands in the joining path, which spec 0015 and spec 0023 both worked to keep short.
- The invite only registration model would have to be reconciled with a provider that will happily authenticate anyone.

### Option 4: Wait for Claude's `static_headers` beta

Claude supports a `static_headers` authentication type in beta, where an organization administrator enters a fixed bearer token once when adding the connector.

**Pros**:
- Zero code. The tokens `/connect` already hands out would work unchanged.
- It is the honest answer to the immediate problem if the feature is available on this account.

**Cons**:
- It is beta, it is organization scoped rather than per person, and availability could not be confirmed from here.
- It solves it for Claude and for nothing else, so ChatGPT connectors and anything following the specification stay locked out.
- It leaves the platform's public deploy surface with no discovery documents at all, which is the thing every future client will look for first.

## Rationale

Option 1 wins on the force that matters most here, which is that the platform already is an identity system. Options 2 and 3 both spend their savings on the same thing: a second way for a request to become an account. The root `AGENTS.md` records what that costs, in the entry about the sign in lockout that lived in one handler while a second surface silently applied none of it for months. A second entrance into `internal/auth` is exactly that shape of bug, and this feature would create one on the surface that runs code on the cluster.

The credential decision follows the same reasoning. An access token that is an ordinary `api_tokens` row means `Authenticate` is untouched, `/tokens` is the one place credentials are listed and revoked, and there is no refresh grant to build. OAuth 2.1 permits an access token with no refresh token, and Claude refreshes reactively on a `401`, so a token that never expires simply is never refreshed. The exposure is identical to a token pasted by hand today, which is a bound the platform already accepted rather than a new one.

Dynamic client registration over Client ID Metadata Documents is the one place this spec chooses the older mechanism deliberately. CIMD is what the MCP draft prefers and what Anthropic recommends, and it would remove a table, an endpoint and a sweep. It would also make the control plane pod fetch an HTTPS URL that a stranger chose, on every fresh connection. Spec 0017 spent a whole feature bounding what an app pod may reach; adding an unbounded outbound fetch to the control plane pod, the one holding the cluster credentials, to save a table is the wrong trade. Claude falls back to registration automatically when CIMD is not advertised, so the cost of this choice is a few extra client rows, which the sweep handles.

Option 4 is not chosen but it is not dismissed. It is recorded in Follow-up, because if `static_headers` turns out to be available it is worth ten minutes of checking before any of this is built.

## References

**Project sources**:
- Spec 0021, the console hostname, the opt in registration rule and the single derivation of the visitor's address.
- Spec 0022, the deploy hostname, its catch all, the deploy path limiter and the bad token lockout inside `Authenticate`.
- Spec 0023, `/connect`, the dated token name with its ordinal fallback, and the show once discipline.
- Spec 0017, the bounded egress posture that rules out an outbound fetch to an attacker chosen URL.
- `internal/store/migrations/00001_initial_schema.sql`, the `api_tokens` shape and the live name partial index this spec copies.
- Root `AGENTS.md`, the rule that every sign in refusal lives in `Service.Login` and the account of what a second unguarded surface cost.

**Practices and standards**:
- OAuth 2.1 authorization code flow with PKCE, public clients only, no implicit grant.
- RFC 9728 protected resource metadata for authorization server discovery.
- RFC 8414 authorization server metadata.
- RFC 7591 dynamic client registration.
- RFC 8707 resource indicators, so a token names the resource it is for.
- RFC 9207 issuer identification in the authorization response.
- RFC 8252 section 7.3, loopback redirect URIs matched with the port ignored.
- RFC 6749 error codes, so a client can tell a dead grant from a bad request.

**Links** (fetched and confirmed on 2026-08-16):
- MCP authorization specification: https://modelcontextprotocol.io/specification/draft/basic/authorization
- Authentication for connectors, Claude documentation: https://claude.com/docs/connectors/building/authentication
- RFC 9728, protected resource metadata: https://datatracker.ietf.org/doc/html/rfc9728
- RFC 7591, dynamic client registration: https://www.rfc-editor.org/rfc/rfc7591
- RFC 8707, resource indicators: https://www.rfc-editor.org/rfc/rfc8707.html
- RFC 9207, issuer identification: https://datatracker.ietf.org/doc/html/rfc9207
- RFC 8252, OAuth for native apps: https://datatracker.ietf.org/doc/html/rfc8252
