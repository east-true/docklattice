# Operations

These guides are for operators who install, expose, configure, and recover
DockLattice. Start with the supported-environment check; DockLattice intentionally
refuses several deployment shapes that it cannot manage safely.

| Task | Document |
|---|---|
| Confirm that a host is supported | [Supported environments](supported-environments.md) |
| Install the Server and Agents | [Installation](install.md) |
| Trial an install on a registry-restricted internal network | [Internal-network trial installation](internal-network-install.md) |
| Configure commands and operational defaults | [Configuration reference](configuration.md) |
| Integrate with the HTTP interface | [HTTP API](api.md) |
| Recover Server or Agent identity | [Identity recovery](recovery.md) |
| Recover from storage pressure | [Degraded storage](degraded-storage.md) |
| Diagnose an offline or stuck Agent | [Agent diagnostics](agent-diagnostics.md) |

The security boundary is documented separately in
[`SECURITY.md`](../../SECURITY.md). DockLattice v1 has no browser authentication;
do not expose the Server UI without an authenticating proxy or a private
tunnel.
