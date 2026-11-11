# Security and threat boundaries

**Default:** bind `127.0.0.1`, protect `/api/*` and `/v1/*` with an unpredictable local bearer token, and keep config/credential files outside Git. Do not bind a public interface directly. The environment variable `WW_ALLOW_REMOTE_BIND=1` is an escape hatch for operators who have independently deployed a trusted reverse proxy, TLS, authentication and rate limiting.

- Model-provided tool arguments are untrusted. The built-in calculator uses a restricted Go AST (no execution of arbitrary language expressions). Never add a shell execution tool without sandboxing and explicit approval.
- MCP servers are configured explicitly and must use HTTPS, or HTTP on loopback. Any MCP tool not listed by the operator under `safe_tools` requires approval; safe classification is a **local trust decision**, not proof from remote metadata.
- The web fetcher disallows IP literal URLs, reserved/private DNS resolutions, redirects and non-HTTPS content. It pins the validated DNS target for the request. This is one layer of SSRF defense, not a complete network sandbox.
- Local session and workflow snapshots are written with restrictive file permissions and atomic replacement. They can still include sensitive chat content; users must protect their host filesystem and backups.
- Untrusted webpages, uploaded files and remote tool outputs should be treated as data, not instructions. Prompt injection resistance is not guaranteed by the instruction prompt alone.
- File ingestion is limited to 16 MiB. The file upload operation stores bytes and metadata, not an instruction to execute or decode the file. Media processing requires appropriate external tools.
- External model or tool costs and upstream policies are governed by the user's accounts. Autopilot and delegated agents are bounded but can still incur provider usage charges.
- Credentials are read only from environment variable names explicitly configured by the operator, and are not returned through the public API.

To report a security issue, use the maintainer's private GitHub contact or private vulnerability reporting, not a public issue containing active credentials or exploits.
