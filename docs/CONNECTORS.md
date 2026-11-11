# Connector and MCP integration

Wizards & Wands can discover and invoke tools exposed by **configured external MCP Streamable HTTP services**. It does **not** inherit ChatGPT plugins, OAuth grants, API tokens or network access from the maintainer's ChatGPT account.

A curated catalog of 23 common integrations is in [`catalog/connectors.json`](../catalog/connectors.json). These are developer-oriented **integration ideas**, not installed connections. Any connector must supply an independently authorized MCP endpoint or have an adapter implemented.

## Example local MCP service

```json
{
  "mcp_servers": [
    {
      "name": "mytools",
      "url": "http://127.0.0.1:9999/mcp",
      "token_env": "MY_TOOLS_MCP_TOKEN",
      "protocol_version": "2026-07-28",
      "safe_tools": ["lookup_document", "get_status"]
    }
  ]
}
```

Put this entry into your actual `config.json` alongside model profiles. Populate `MY_TOOLS_MCP_TOKEN` from your local secret manager. **Never place API keys directly in the JSON file.** The harness reads the configured environment variable at call time. Tool names exposed to the model are `mcp__mytools__lookup_document` and `mcp__mytools__get_status` (only if the server actually advertises them).

Under `autopilot`, only entries in `safe_tools` can run without human approval. `safe_tools` must be audited by the operator; it is **not** automatically inferred from untrusted remote annotations. Tools with side effects should never be classified as safe. Chat/assisted/research/code/media modes require approval for MCP tool calls.

The built-in `web_fetch` tool performs bounded HTTPS GET requests, rejects private/reserved destination IPs and redirects, and does not execute JavaScript. For interactive browsing, use your own properly isolated browser MCP service; browser automation is not built into this repository.

## DATA Frameworks bridge

Set `data_frameworks.base_url` to the authenticated loopback API exposed by an AEGIS DATA Frameworks instance, for example `http://127.0.0.1:8790`; set `data_frameworks.api_key_env` to an environment variable containing its API token. This enables read-only tools `data_capabilities` and `data_readiness`. The private development version and public version can each run independently behind their own configured endpoints; no private path is embedded in Wizards & Wands.

Data mutation and workflow scheduling through DATA's `/v1/runs` remain a **planned separately permissioned adapter**, not an automatic effect of installing this harness.

## Supported transport boundary

The current implementation focuses on Streamable HTTP JSON-RPC `tools/list` and `tools/call`, with JSON and SSE responses, explicit protocol-version headers, tool-name headers and client `_meta`. Full 2026-07-28 MCP compliance (including subscriptions/MRTR/header annotations), OAuth, stdio MCP servers, client tool elicitation, media blobs, and dynamic trust policy updates are future work.
