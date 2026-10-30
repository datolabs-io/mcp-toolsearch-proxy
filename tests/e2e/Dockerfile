# Images for the end-to-end tests, built from the working tree: the proxy,
# and the fixture MCP servers. Releases are built by GoReleaser instead (see
# the Dockerfile at the root).
FROM golang:1.27.3-trixie@sha256:7b072c590a326b9d2a43b165ae247f6e8eafff32e1ad154ab97139e89f053be9 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY vendor vendor
COPY cmd cmd
COPY internal internal
COPY tests/e2e/fixture tests/e2e/fixture
RUN CGO_ENABLED=0 go build -trimpath -mod=vendor -o /out/ ./cmd/mcp-toolsearch-proxy ./tests/e2e/fixture

FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99 AS proxy
COPY --from=build /out/mcp-toolsearch-proxy /usr/bin/
USER nonroot:nonroot
ENTRYPOINT ["/usr/bin/mcp-toolsearch-proxy"]

FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99 AS fixture
COPY --from=build /out/fixture /usr/bin/
USER nonroot:nonroot
ENTRYPOINT ["/usr/bin/fixture"]
