# Changelog

## v0.2.0

- Distinguish task timeout (`Reason: "timeout"`) from a lost result (`Reason: "deadline"`).
- Refuse millisecond receivedAt values locally, matching the Node SDK.
- Return an empty, non-nil results map for success without results.
- `Wait` returns a retained result even after the task's or the wait's deadline.
- `Version` and the default User-Agent are 0.2.0.
- Cover non-JSON HTTP errors with typed-error regression tests.

## v0.1.0

- Initial release of the Go client for Mailman's hosted Tasks API.
- Task creation, verification, destructive polling, shared waits and code/link helpers.
- Typed errors, local validation, bounded result retention and safe retry pacing.
- Standard library only; Go 1.21 or newer.
