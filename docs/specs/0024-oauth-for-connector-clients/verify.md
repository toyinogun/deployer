# Verify: connecting a client that will not hold a token · spec 0024 · updated 2026-08-16
_Steps derived from spec 0024 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

The one that matters most is the first: the whole feature exists so a real
connector can be added, and nothing in the suite can reach that. The fake
clientset resolves no names, `httptest` is behind no tunnel, and every unit test
here drives the platform's own handlers rather than a client that has its own
ideas about what a metadata document should say.

## UI / manual

- [x] In the real Claude desktop app, add a connector with the address from the fifth `/connect` tab → discovery finds the console, the browser lands on the approval page, Approve returns to the app, and the app lists the tools → AC-1, AC-3, AC-4, AC-9, AC-16, AC-19 · driven 2026-08-16 against digest `sha256:77de8773`: the app registered itself as `Claude`, listed all ten tools, and its grant reads `Claude 2026-08-16` in `/tokens`, last used one minute after it was issued
- [x] From that connector, run one `deploy_app` through to healthy → AC-20 · driven with a granted token rather than from the desktop app: release 8 of `verify demo` reached healthy in 44s and the hostname answers 200
- [x] Sign in to the console, open `/tokens` → the grant is listed under the client's name plus today's date; revoke it → the connector's next call fails → AC-20, AC-20a
- [x] Open `/connect` → five tabs; the fifth shows only `https://<MCPHost>/mcp`, carries no token and no placeholder, and offers no mint control → AC-26
- [x] Load `/connect` with JavaScript disabled → all five panels stack, the fifth included → AC-27
- [x] Sign out, then open an authorize URL with a query string directly → you land on `/login`, and signing in returns you to that same authorize URL with `state` and `redirect_uri` intact → AC-9a
- [x] On the approval page for a client whose registered addresses are all loopback → the heading is the redirect host, the client's name is quoted and labelled as its own claim, and the local machine warning is present → AC-13, AC-13a
- [x] Repeat with a client registered on an `https` address → the same page, no local machine warning → AC-13a
- [x] Press Deny → the client reports `access_denied`, and `oauth_clients.approved_at` for it is still null → AC-15
- [x] Suspend an account from `/admin/accounts`, then have it open an authorize URL → no approval page, and no row was written → AC-14

## Commands

- [x] `curl -s https://<MCPHost>/.well-known/oauth-protected-resource | jq` → `resource` is exactly `https://<MCPHost>`, `authorization_servers` holds exactly the console URL → AC-1
- [x] `curl -s https://<MCPHost>/.well-known/oauth-protected-resource/mcp | jq .resource` → exactly `https://<MCPHost>/mcp`, the path's own address rather than the host's → AC-1
- [x] `curl -si -X POST https://<MCPHost>/mcp -H 'Authorization: Bearer nope' | grep -i www-authenticate` → `Bearer resource_metadata="https://<MCPHost>/.well-known/oauth-protected-resource/mcp", scope="deploy"` → AC-2
- [x] Spend the deploy path bucket until `/mcp` answers 429 → that response carries no `WWW-Authenticate` → AC-2a
- [x] `curl -s https://<ConsoleHost>/.well-known/oauth-authorization-server | jq` → `issuer` is the console URL, the three endpoints sit under it, `scopes_supported` is `["deploy"]` with no `offline_access`, and there is no `client_id_metadata_document_supported` → AC-3, AC-3a
- [x] `curl -s -X POST https://<ConsoleHost>/oauth/register -d '{"client_name":"x","redirect_uris":["http://example.org/cb"]}'` → `400 invalid_redirect_uri`; the same with no `redirect_uris` → `400 invalid_client_metadata` → AC-4a, AC-5
- [x] Register with `http://[::1]/callback` → accepted, so a conforming IPv6 loopback client is not turned away → AC-5
- [x] Authorize with a `redirect_uri` nobody registered → an error page on the console, **no** `Location` header, and nothing the caller sent rendered on it → AC-10, AC-10c
- [x] Authorize a registered `https://host/cb` as `https://HOST/cb`, as `https://host/cb/`, and percent encoded → all three refused on the page → AC-10b
- [x] Authorize a registered `http://localhost/callback` as `http://localhost:54321/callback` → matched → AC-10a
- [x] Authorize with `code_challenge_method=plain`, then with no challenge at all → both redirect to the registered address with `invalid_request`, carrying `state` and `iss` → AC-11
- [x] Authorize with `resource=https://someone-else.example/mcp` → redirects with `invalid_target` → AC-12
- [x] Exchange a code twice, the second time with a wrong `code_verifier` → the second is `400 invalid_grant`, and the token the first issued no longer authenticates `/mcp` → AC-16a, AC-16b
- [x] Exchange a code, then exchange it again with the **right** verifier once the first has finished → the second is still `400 invalid_grant`, and the token the first issued still authenticates `/mcp` → AC-16b
- [x] Do that again with a mismatched `client_id` but the right verifier → refused, and the first token still authenticates, so the verifier alone decided → AC-16b
- [x] Exchange the same code twice concurrently, then use the token that came back → it authenticates, rather than having been revoked by its own duplicate → AC-16b, AC-18a
- [x] Register a loopback client with no port, authorize with one, and follow the whole flow → the redirect carries `localhost:<port>`, and the exchange against that same address succeeds → AC-10a, AC-10d · a real listener on 54321 caught the callback
- [x] Wait 61 seconds after approving, then exchange → `400 invalid_grant` → AC-16a
- [x] `curl -X POST .../oauth/token -H 'Content-Type: application/json' -d '{}'` → `400 invalid_request`, never `415` → AC-17
- [x] Exchange with a wrong `code_verifier` → `400 invalid_grant`, and the body names no check → AC-18
- [x] Exchange the same code twice concurrently (`xargs -P2`) → exactly one token comes back → AC-18a
- [x] Grant the same client twice → `SELECT count(*) FROM api_tokens WHERE oauth_client_id=? AND revoked_at IS NULL` is 1 → AC-19, AC-19b
- [x] Check the token response headers → `Cache-Control: no-store`, no `expires_in`, no `refresh_token` → AC-19
- [x] Add a connector four times over from one address, then sign in to the console from that address → the sign in works, so the connector bucket is not the sign in one → AC-6, AC-22
- [x] `SELECT count(*) FROM audit_log WHERE action='connector_grant'` after one exchange → 1, with the token id as the target and the client id in `reason`; after a registration → unchanged → AC-7, AC-23
- [x] Set an `oauth_clients` row's `created_at` back 8 days with `approved_at` null, restart the pod → the row is gone; do the same with `approved_at` set → the row survives → AC-8, AC-8a
- [x] `curl -s -o /dev/null -w '%{http_code}\n' https://<MCPHost>/oauth/authorize` and the other four console routes on the deploy host → 404 each; both protected resource documents on the console host → 404 each → AC-25a
- [x] Time each of the five endpoints → every one well under a second, none doing anything but local SQLite → AC-28
- [x] `kubectl exec` into the pod and read the schema → `oauth_clients`, `oauth_codes`, `api_tokens.oauth_client_id` and both indexes present → AC-29
- [x] `grep -r DEPLOYER_ deploy/` → no variable was added for this feature → AC-30

## Value sourcing

One per row of the spec's Value sourcing table, each varying the input so a
value taken from the wrong place is visible rather than merely plausible.

- [x] Change `DEPLOYER_MCP_HOST`, restart → both protected resource documents, the `WWW-Authenticate` header and the fifth `/connect` tab all follow, with nothing carrying the old host → AC-1, AC-2, AC-26
- [x] Change `DEPLOYER_CONSOLE_HOST`, restart → `issuer`, all three endpoint URLs, `authorization_servers` and every `iss` on a redirect follow → AC-1, AC-3, AC-15, AC-16
- [x] Register a client with two redirect URIs, authorize against the second → the approval page's heading is the host of the **matched** one, not the first registered and not the requested string → AC-13
- [x] Register a client named `<script>alert(1)</script>` with control characters and a trailing run of spaces → the approval page shows it escaped, and the token name in `/tokens` is bounded, single line and printable → AC-13, AC-20a
- [x] Grant a **second client of the same name** on one day → the second token's name carries the ` (2)` ordinal rather than failing on the live name index → AC-20a · the original wording could never fire it, since AC-19 revokes the previous token in the same transaction and so frees the name; a colliding name across two clients is what exercises the fallback
- [ ] Approve from account A while account B is signed in elsewhere → the code is written against A, the session that approved, and the token lands on A → AC-9, AC-16
- [ ] Move the platform clock forward a day and grant again → the token name carries the new date, from the service clock rather than the machine's → AC-20a
- [x] Exchange with no `resource` at all → succeeds, taking the value stored on the code; exchange with a different one → `invalid_grant` → AC-18

## Acceptance-criteria coverage

- AC-1 · AC-2 · AC-2a → the discovery and challenge steps, plus `internal/httpapi/oauth_test.go` and `internal/mcp/oauth_test.go`
- AC-3 · AC-3a → the metadata document steps, plus `internal/web/oauth_test.go`
- AC-4 · AC-4a · AC-5 → the registration steps, plus `internal/web/oauth_test.go` and `internal/identity/oauth_test.go`
- AC-6 · AC-22 → the four connectors then sign in step
- AC-7 · AC-23 → the audit_log count steps
- AC-8 · AC-8a → the sweep steps, plus `internal/store/oauth_test.go`
- AC-9 · AC-9a → the sign in gate round trip, plus `internal/web/oauth_test.go`
- AC-10 · AC-10a · AC-10b · AC-10c → the redirect matching steps, plus `internal/identity/oauth_test.go`
- AC-11 · AC-12 → the PKCE and resource steps
- AC-13 · AC-13a → the approval page steps
- AC-14 · AC-15 → the suspension and Deny steps
- AC-16 · AC-16a → the replay and expiry steps
- AC-17 · AC-18 · AC-18a → the token endpoint steps, plus `internal/store/oauth_test.go`
- AC-19 · AC-19a · AC-19b → the live token count and response header steps
- AC-20 · AC-20a → the real connector deploy and the token list steps
- AC-21 → covered by the token endpoint authenticating no client, exercised throughout
- AC-24 → every OAuth failure above answers an RFC 6749 code; `internal/domain/reason.go` gained no member
- AC-25 · AC-25a · AC-25b → the cross host 404 steps, plus `internal/httpapi/deployhost_test.go` and `internal/web/oauth_test.go`
- AC-26 · AC-27 → the `/connect` tab steps
- AC-28 · AC-29 · AC-30 → the timing, schema and configuration steps

## Owed

Nothing. The three wordings this file used to owe were settled by `/architect` on 2026-08-16, in the spec rather than here: **AC-25b** now says `404` on a host scoped pattern and `405` on the bare one, which is what `TestAWrongMethodOnAnOAuthRouteIsRefused` already asserts; **AC-26** records that the connector tab sits before the generic one because the last entry in the set is the fallback; and **AC-10d** pins which of the two addresses a redirect URI match yields, after the build yielded the wrong one.

**AC-16b** landed on 2026-08-16 and was driven on the cluster the same day against digest `sha256:77de8773`: the client's own retry is refused and keeps its token, a replay carrying another client's id but the right verifier is refused and also keeps it, and a replay with a wrong verifier still revokes. The raced pair mints once and leaves that token live.

The real connector step was driven the same day and passed, so the platform's half and a real client's half have both been seen. Two steps stay undriven, neither of them fixable by running something again today, and neither leaving a criterion without evidence:

- **Two accounts at once** (AC-9, AC-16). Only one account was available, so approving as A while B is signed in elsewhere was never exercised.
- **The clock a day forward** (AC-20a). Moving the live platform's clock is a change to the deployment, which this gate does not make. The date half of the name is therefore covered by the suite alone; the ordinal half was driven.
