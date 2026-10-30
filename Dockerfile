# Built by GoReleaser from the release binaries; see .goreleaser.yaml.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99

ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/mcp-toolsearch-proxy /usr/bin/
COPY build/licenses /usr/share/doc/mcp-toolsearch-proxy/licenses
# Owned by the user the proxy runs as, so that a new volume mounted here is
# too, and fetch-model can write the model into it.
COPY --chown=65532:65532 --chmod=0755 packaging/model /model

# The built-in model lives in /model: mount a volume there and fill it once with
#   docker run --rm -v <volume>:/model <image> fetch-model
# Set TOOLSEARCH_UPSTREAM at run time.
ENV MCP_ADDR=0.0.0.0:8000 \
    TOOLSEARCH_MODEL_DIR=/model
VOLUME /model
EXPOSE 8000
USER nonroot:nonroot
ENTRYPOINT ["/usr/bin/mcp-toolsearch-proxy"]
