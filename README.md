# cassandra-plugins

Claude Code plugin marketplace for the Cassandra platform.

## Setup

Add the marketplace:

```bash
claude plugin marketplace add Cassandras-Edge/cassandra-marketplace
```

## Install a plugin

```bash
claude plugin install <plugin-name>
```

Available plugins:

| Plugin | Type | Description |
|--------|------|-------------|
| `stopgate` | Hook | Blocks lazy stops — uses an LLM call to verify task completion |
| `acl-lint` | Hook | Validates `acl.yaml` structure on write/edit |
| `gateway-mcp` | MCP | Cassandra Gateway — execute any platform tool via sandboxed Python |
| `market-research` | MCP | Financial market data — stocks, SEC filings, macro, options, earnings |
| `twitter-mcp` | MCP | Twitter/X — news search, Grok AI, post analytics, personal timeline |
| `reddit-mcp` | MCP | Reddit — search, subreddit browsing, post + comment reading |
| `claudeai-mcp` | MCP | claude.ai — conversations, projects, knowledge docs, artifacts |
| `discord-mcp` | MCP | Discord — search messages, read channels/threads/DMs, attachments |
| `media-mcp` | MCP | YouTube/media — transcription, search, comments, Watch Later |

## Examples

```bash
# Install the stop gate hook
claude plugin install stopgate

# Install the gateway MCP server
claude plugin install gateway-mcp

# Install to current project only
claude plugin install media-mcp --scope project
```

## Managing plugins

```bash
# List installed plugins
claude plugin list

# Disable a plugin without uninstalling
claude plugin disable stopgate

# Re-enable
claude plugin enable stopgate

# Uninstall
claude plugin uninstall stopgate

# Update all plugins after marketplace changes
claude plugin marketplace update
claude plugin update <plugin-name>
```

## Auth

MCP plugins authenticate via WorkOS OAuth. On first tool call, you'll be prompted to authorize in the browser. No API keys or manual config needed.
