// Command mcp-toolsearch-proxy serves four MCP tools, search_tools,
// describe_tool, call_read_tool and call_write_tool, in front of an MCP server with too many
// tools to list to a model, such as a gateway that aggregates many servers.
//
// Every request it makes upstream carries the caller's own Authorization
// header; it holds no credential of its own. Run "mcp-toolsearch-proxy -h"
// for its settings.
package main
