# Contributing

Thanks for your interest. Please read [ARCHITECTURE.md](ARCHITECTURE.md) first. Everyone taking part follows the
[code of conduct](CODE_OF_CONDUCT.md).

## Setup

```bash
mise install    # installs the pinned tools and the git hooks
mise run ci     # lint, scan and test, exactly as CI does
```

Tool versions are pinned in `.mise.toml` and their download checksums in `mise.lock`. After changing a tool version,
refresh the lockfile with `mise lock --platform linux-x64,macos-arm64,windows-x64`.

GitHub Actions are pinned to commits in `.github/workflows/actions.lock`, which GitHub checks before every run. After
adding or changing an action in a workflow, regenerate it with `mise run actions:lock`; that needs the `gh` CLI with
`gh extension install github/gh-actions-lock`. Renovate keeps it up to date otherwise.

## Tests

- `mise run test`: unit and integration tests. Tools are tested through an MCP client against the server, with a
  fake upstream MCP server.
- `mise run test:model`: downloads the built-in model into `.cache/model` once, then checks the tokenizer and
  embeddings against the reference implementation and search against the evaluation set.
- `mise run e2e:up`, `mise run test:e2e`, `mise run e2e:down`: the end-to-end tests, against agentgateway, the proxy
  and fixture MCP servers started with Docker Compose. `test:e2e` takes its endpoints from `E2E_SEARCH_URL` and
  `E2E_ALL_URL`, so it can run against any stack of the same topology; the tests of a second, failClosed route are
  skipped against another stack unless `E2E_CLOSED_URL`, `E2E_CLOSED_ALL_URL` and `E2E_CLOSED_METRICS_URL` are set
  too. `go run ./tests/e2e/mint` replaces the test key and tokens.
- `mise run bench:context`, `mise run bench:agent`: the benchmarks in `bench/`, a module of its own; see
  [docs/benchmarks.md](docs/benchmarks.md). `bench:agent` makes billed API requests, and runs only with
  `ANTHROPIC_API_KEY` set.

## Rules

- **Test first.** Every change starts with a failing test.
- **No real data.** Never commit real tool catalogs, queries, tokens or tool results. gitleaks runs in the pre-commit
  hook and in CI.
- **Search changes are measured.** A change to ranking, tokenizing or stemming reports the evaluation numbers before
  and after, and does not lower a floor.
- **Conventional commits.** `type(scope): description`, with `!` for breaking changes. PR titles follow the same
  format; PRs are squash-merged.
- **Tool changes are API changes.** Renaming a tool or an argument breaks clients that call it: mark it with `!`.

## Pull requests

Keep PRs focused, describe what changed and how it was tested, and make sure `mise run ci` passes. A change to a flag
needs `mise run docs:config`, which regenerates [docs/configuration.md](docs/configuration.md); a test fails until it
is run.

## Release notes

release-please writes the changelog from the squashed commit of each PR, so a PR's title is all a release note says
about it. Make the title name the change that matters most to someone upgrading. Do not rely on a
`BEGIN_COMMIT_OVERRIDE` block in the PR body: release-please dropped the whole PR from the 0.5.0 notes when one was
present. Whatever the title leaves out, such as a setting added alongside a feature, goes in the "Upgrading" section.

Every release that breaks something or adds a setting gets an "Upgrading" section in CHANGELOG.md and in the GitHub
release, naming each new setting with its environment variable and saying what an operator or a client must do.
Merging the release PR publishes the release at once, and a published release cannot be edited, so the release PR
is the last checkpoint: add the section to its body, which becomes the GitHub release notes, and to its CHANGELOG.md
entry, right before merging it. release-please rewrites the release PR whenever main moves, so add the section again
if anything merges in between. Later releases keep it: release-please only adds new entries at the top.
