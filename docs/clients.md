# Connecting a client

Clients connect to the route that leads to the proxy, such as a gateway's `/mcp/search`, not to the upstream. The
examples use `https://mcp.example.com/mcp/search`.

## What allowing reads grants

Five tools reach the model. `search_tools` and `describe_tool` only read the catalog. `call_read_tool` calls tools the
upstream marks read-only, `call_read_tools` calls several of them at once, and `call_write_tool` calls any tool. The
point of the split is to let the first four run without asking, and keep asking before every `call_write_tool`. Writes
are never batched, so each one keeps its own approval. `-max-batch-calls 0` leaves `call_read_tools` out.

Be clear about what that standing grant covers. Allowing `call_read_tool` or `call_read_tools` once allows every tool
the upstream marks read-only, on every server behind it, including servers added to the upstream later. The proxy
decides "read-only" from the tools' own annotations: a tool counts as read-only when it is marked `readOnlyHint` and not
also `destructiveHint`, and a name the upstream lists more than once only when every copy is. So the grant is only as
trustworthy as the server that sets the hint: a server that marks a write as read-only gets that write through
`call_read_tool` without a prompt. The check reads the caller's catalog, so a tool that loses its hint upstream is still
called as a read until the catalog is listed again, at most `-catalog-max-age` later. The gateway's authorization rules
still apply to every call.

When a server's annotations can't be trusted, list its tools in `-write-tools` (`TOOLSEARCH_WRITE_TOOLS`), by name or
by pattern such as `github_*`. The proxy then treats them as writes whatever the upstream says, so `call_read_tool`
refuses them and they need `call_write_tool`. The setting can only take tools away from the read tools, never add
one.

## claude.ai

1. In **Customize > Connectors**, choose **Add custom connector**, give it a name and the proxy route's URL, such as
   `https://mcp.example.com/mcp/search`.
2. Sign in when asked. The gateway runs the OAuth flow; the proxy only passes the token on.
3. In the connector's tool permissions, set `search_tools`, `describe_tool`, `call_read_tool` and `call_read_tools` to
   always allow, and
   leave `call_write_tool` asking for approval. Choosing "Allow always" on the first approval request of a tool does
   the same for that tool.

## Claude Code

```bash
claude mcp add --transport http toolsearch https://mcp.example.com/mcp/search
```

Run `/mcp` in Claude Code to sign in. For an upstream that takes a fixed token instead of OAuth, pass it with
`--header "Authorization: Bearer <token>"`.

Claude Code names the tools `mcp__<name>__<tool>`, after the name given to `claude mcp add`. To let reads run without
asking, allow the four read tools in `.claude/settings.json` (or `~/.claude/settings.json` for every project) and
leave `call_write_tool` out, so it keeps asking:

```json
{
  "permissions": {
    "allow": [
      "mcp__toolsearch__search_tools",
      "mcp__toolsearch__describe_tool",
      "mcp__toolsearch__call_read_tool",
      "mcp__toolsearch__call_read_tools"
    ]
  }
}
```

## Server instructions are cut by the client

The proxy forwards the upstream's instructions in the handshake, after its own, up to `-max-instructions` characters.
Clients cut that text again, and much earlier: Claude Code keeps the first 4,096 characters of a server's instructions
by default (`CLAUDE_CODE_MAX_MCP_DESCRIPTION_LENGTH` changes it), so with the proxy's own guidance in front, only about
the first 3,500 characters of the upstream's text reach the model. Whatever comes after is lost without a sign, and
the proxy's own note that it cut the text is never seen. Behind a gateway that joins the instructions of many servers,
which of them survive depends on the order it joins them in.

Put what matters into the start of the upstream's instructions, or into the tools' own descriptions, which the model
reads when it needs them. `-max-instructions 0` stops forwarding entirely.

## When you don't need this

The proxy pays off when a client would otherwise load every tool's definition up front. Clients that already defer MCP
tools and search them on demand, as Claude Code does by default, gain less: they see the tool names but load
definitions only when needed. The proxy can still help there with a catalog too large even for the list of names,
with search in languages other than the tools', and with one read/write split across every server, but measure before
adding a hop.
