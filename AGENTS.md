# Agent instructions

These rules apply to any coding agent working in this repository.

## Workflow

- Read ARCHITECTURE.md before changing code. Do not follow it blindly either: raise design problems instead of
  working around them.
- Work test-first: write a failing test, make it pass, then refactor.
- Run `mise run lint` after editing Go files and `mise run ci` before proposing a change. Changes to search, the
  tokenizers or the model also need `mise run test:model`; changes to the server or upstream client,
  `mise run e2e:up` and `mise run test:e2e`.
- When adding a dependency, tool or action, look up its latest release at that moment and use it. Do not rely on
  remembered version numbers. Run `mise run vendor` after any change to go.mod, and `mise run actions:lock` after any
  change to an action in a workflow; never edit `.github/workflows/actions.lock` by hand.
- Do not raise the `go` directive in go.mod; it is the minimum supported Go version and changes by decision only.

## Code

- Every package has a `doc.go` with its package comment.
- Every I/O function takes `context.Context` first. No package-level state, no global loggers.
- Errors: wrap with `%w`. Never put tokens, queries, tool arguments or tool results in errors that are logged, or in
  logs or metric labels.
- Tests use `github.com/stretchr/testify` (`require` for errors and preconditions) and run with `t.Parallel()`.
  Tools are tested through a real MCP client against the server, with a fake upstream MCP server.
- Do not blindly change a failing test to make it pass; it may have found a bug. Do not lower an evaluation floor to
  make a ranking change pass.

## Safety

- The proxy holds no credentials and must never gain any: every upstream request carries the caller's token only.
- Never commit real tool catalogs, query logs, tokens or tool results. The tokens in `tests/e2e/testdata` are minted
  for the fixture stack and grant nothing elsewhere.
