# Dreamtune

Connect the proxy to Dreamtune's hosted MCP server over streamable HTTP. It
provides model requests, tool discovery and execution, GPU compute, and usage
inspection through a Dreamtune workspace key. See the
[Dreamtune MCP setup guide](https://docs.dreamtunelabs.com/connect/mcp).

## Start the proxy

1. Create a workspace API key at
   [Dreamtune API keys](https://dreamtunelabs.com/dashboard/api-keys/).
   Grant only the model, tool and compute access your client needs. Execution
   uses the workspace balance; GPU jobs incur ongoing usage charges while running.
2. Supply two separate secrets through the environment:

   ```sh
   export DREAMTUNE_API_KEY='<YOUR_DREAMTUNE_WORKSPACE_KEY>'
   export MCP_PROXY_TOKEN='<YOUR_LOCAL_PROXY_TOKEN>'
   ```

   `DREAMTUNE_API_KEY` authenticates the proxy to Dreamtune. `MCP_PROXY_TOKEN`
   authenticates your MCP client to the local proxy. Use distinct values and
   keep both out of source control.
3. Validate the configuration, then start the proxy from the repository root:

   ```sh
   mcp-proxy -config examples/dreamtune.json -check-config
   mcp-proxy -config examples/dreamtune.json
   ```

   Environment expansion is enabled by default; do not pass `-expand-env=false`.
   `-check-config` validates local settings without testing the remote key.

## Connect a client

Use streamable HTTP at `http://127.0.0.1:9090/dreamtune/mcp` and send
`Authorization: Bearer <YOUR_LOCAL_PROXY_TOKEN>`. The proxy supplies the separate
Dreamtune key to the upstream; clients do not need to receive that key.

The example binds only to loopback and disables tool payload logging. For remote
clients, configure your own HTTPS reverse proxy and a matching `baseURL` rather
than exposing the example's plaintext listener publicly.

Start by listing the available MCP tools. Discover tool identifiers with
`list_tools` and inspect inputs with `inspect_tool` before choosing an execution
operation. Listing MCP tools does not execute an upstream service. Model/tool
execution and GPU creation can consume workspace credit; review tool calls in
your client before approving them.
