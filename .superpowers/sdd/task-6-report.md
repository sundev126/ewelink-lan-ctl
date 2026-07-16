# Task 6 Report: Process Wiring and Graceful Shutdown

## Status

Complete.

## RED evidence

1. Construction smoke test:
   - Command: `go test ./cmd/ewelink-lan-ctl -v`
   - Observed failure: `undefined: buildHandler`.
2. Permanent refresh-rejection adapter:
   - Command: `go test ./cmd/ewelink-lan-ctl -run 'TestRefreshAdapter' -v`
   - Observed failure: an HTTP 401 upstream rejection was returned unchanged instead of matching `token.ErrOAuthRequired`.
3. Graceful shutdown boundary:
   - Command: `go test ./cmd/ewelink-lan-ctl -run 'TestServeWith' -v`
   - Observed failure: `undefined: serveWith`.
4. Configured HTTP timeouts:
   - Command: `go test ./cmd/ewelink-lan-ctl -run 'TestConfiguredHTTPComponents' -v`
   - Observed failure: `undefined: newHTTPClient` and `undefined: newHTTPServer`.

## GREEN evidence

- The construction test passes with a missing state file and reports `200` plus `oauth_ready:false`.
- Malformed credential JSON logs one fixed `ERROR` message with category `state_malformed`, does not disclose credential material, and leaves the service available but unauthenticated.
- Refresh error adaptation maps eWeLink token errors and HTTP 401/403 refresh rejection to `token.ErrOAuthRequired`; transient failures are preserved.
- Context cancellation invokes graceful server shutdown, `http.ErrServerClosed` is normal, and an already-canceled root context does not start serving.
- The configured request timeout is applied to the outbound `http.Client` and server `ReadHeaderTimeout`.
- The token scheduler runs with the signal-controlled root context.

Focused GREEN command:

```text
go test ./cmd/ewelink-lan-ctl -v
PASS
ok github.com/zm/ewelink-lan-ctl/cmd/ewelink-lan-ctl
```

## Final verification

Command:

```text
gofmt -w cmd && go test ./... && go test -race ./... && go vet ./... && go build ./cmd/ewelink-lan-ctl
```

Result: exit 0. All packages passed the normal and race-enabled test suites; vet and command build completed without diagnostics.

## Self-review

- Process logs use fixed messages and fixed category values; they do not attach raw configuration, credentials, OAuth codes, provider payloads, or upstream errors.
- Tests use `httptest` or an injected serve function; they do not bind a fixed port or contact eWeLink.
- Shutdown uses a fresh 10-second context so cancellation of the root context does not cancel cleanup.
- No unrelated files were modified.

## Concerns

None.

## Quality follow-up: safe error classification

### RED evidence

1. Run and credential-load categories:
   - Command: `go test ./cmd/ewelink-lan-ctl -run 'TestRunErrorLogCategories|TestLogCredentialLoadFailure' -v`
   - Observed compile failures for the missing run sentinels, category classifier, malformed-credential sentinel, and credential-load logging boundary.
2. Existing malformed-state integration contract:
   - Command: `go test ./cmd/ewelink-lan-ctl -v`
   - Observed failure because the old catch-all `invalid_state` assertion did not match the new specific `state_malformed` category.
3. Filesystem read errors during JSON decoding:
   - Command: `go test ./internal/store -run 'TestFileLoadDoesNotClassifyReadFailure' -v`
   - Observed failure because reading a directory was incorrectly wrapped as `ErrMalformedCredentials`.
4. Truncated JSON classification:
   - Command: `go test ./internal/store ./cmd/ewelink-lan-ctl -v`
   - Observed failure because `io.ErrUnexpectedEOF` was not yet included in malformed JSON classification.

### GREEN evidence

- Configuration, construction, listen/serve, and shutdown errors carry distinct sentinels and map to fixed safe log categories; raw error text is never logged.
- Startup credential loading emits distinct fixed categories for malformed state, state-file I/O, OAuth-required/permanent rejection, and transient refresh failures.
- A healthy manager after a transient startup refresh logs `startup token refresh deferred`, without claiming credentials are invalid or unavailable.
- Missing state remains a normal unauthenticated startup and emits no error log.
- `store.ErrMalformedCredentials` covers malformed JSON and structurally incomplete credentials, while actual filesystem read errors preserve their `*os.PathError` classification.

Final follow-up verification command:

```text
gofmt -w cmd internal && go test ./... && go test -race ./... && go vet ./... && go build ./cmd/ewelink-lan-ctl && git diff --check
```

Result: exit 0. All normal and race-enabled tests passed; vet, build, and diff checks completed without diagnostics.
