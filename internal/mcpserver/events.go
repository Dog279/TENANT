package mcpserver

import "tenant/internal/logs/event"

// Events is the MCP server's provider (TEN-387): Tenant serving its memory
// over MCP (`tenant mcp-memory`), which writes the mcp log. Its own provider,
// since this package may not import the connectors' (mcpremote).
var Events = event.NewProvider("mcpserver", "mcp")

var EmbedFailed = Events.Event(2001, event.Warning, "Memory search", "Query embedding failed; memory_search matches keywords only: {err}", "err").
	Doc("A memory_search call from an MCP client could not embed its query, so it matched facts and episodes by keyword alone.",
		"The embedder role's model (often Ollama) is down, unreachable or not loaded.",
		"Check the embedder's backend (tenant doctor, the model log). Searches work again, in full, once it answers.")
