# Changelog

## [0.10.0](https://github.com/datolabs-io/mcp-toolsearch-proxy/compare/v0.9.0...v0.10.0) (2026-11-13)


### ⚠ BREAKING CHANGES

* **server:** call_write_tool no longer calls tools that may delete or overwrite, or that lack destructiveHint: false; call them with call_destructive_tool. A client that allowed call_write_tool without asking now lets only additive writes through. Sessions started before the upgrade must reconnect to see call_destructive_tool.

### Features

* **server:** route calls by all four tool annotations ([#17](https://github.com/datolabs-io/mcp-toolsearch-proxy/issues/17)) ([57ffa1c](https://github.com/datolabs-io/mcp-toolsearch-proxy/commit/57ffa1cd5d10b7578afbebfdcdfa3e8a69731069))

## [0.9.0](https://github.com/datolabs-io/mcp-toolsearch-proxy/compare/v0.8.1...v0.9.0) (2026-11-03)


### Features

* **server:** serve over stdio ([#12](https://github.com/datolabs-io/mcp-toolsearch-proxy/issues/12)) ([fcef5fc](https://github.com/datolabs-io/mcp-toolsearch-proxy/commit/fcef5fc19e7c04d6ec98f4894f8e9cb854f3e519))

## [0.8.1](https://github.com/datolabs-io/mcp-toolsearch-proxy/compare/v0.8.0...v0.8.1) (2026-10-31)


### Bug Fixes

* **catalog:** stop flaky refresh and long-call tests ([#8](https://github.com/datolabs-io/mcp-toolsearch-proxy/issues/8)) ([7e4a072](https://github.com/datolabs-io/mcp-toolsearch-proxy/commit/7e4a07280b16c773bd47f68375501d37069ef059))

## [0.8.0](https://github.com/datolabs-io/mcp-toolsearch-proxy/releases/tag/v0.8.0) (2026-10-30)


### Features

* initial public release ([6d82c16](https://github.com/datolabs-io/mcp-toolsearch-proxy/commit/6d82c1674c7f6d2e6682d8e96bccbacbde2ce36e))

## Changelog
