# ADR 0004 — Go with go-git for the product, Python spike first

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** operator + agent
- **Context source:** `~/.claude/kickoffs/psproxmoxve-remediation/mcp-server-github-handoff-review.md`; operator interview 2026-09-03

## Context

The tool needs GitHub App auth (RS256 JWT to installation token, same three env vars as the official server), a GraphQL client, and read and write access to a local git repository. The operator wants to shrink what agents shell out for and wants the product to run no subprocess at all, but wants the semantics proven cheaply before the Go build.

## Decision

Two phases, same design. Phase 1 (spike) is Python 3.12+: `cryptography` for the JWT, stdlib `urllib` for GraphQL, `subprocess` git with fixed argv. It exists to prove the replay semantics, measure the payload ceiling, and pin the failure modes; it is thrown away once phase 2 passes the same definition of done. Phase 2 (product) is Go: go-git for every repository operation (range walk, blob reads, fetch with the installation token as `x-access-token` basic auth, ref update), `ghinstallation` for App auth, `net/http` for GraphQL, and the official `modelcontextprotocol/go-sdk` for the stdio server. No `os/exec` anywhere in the product.

## Rejected alternatives

- **Python product with the `mcp` SDK.** Fastest path but keeps a git subprocess or takes on dulwich, whose HTTPS fetch with a token is the fiddly part.
- **Go from day one, no spike.** The semantics (expectedHeadOid retry, local reset, refusal set, payload ceiling) are cheaper to get wrong in a script.
- **Fork github-mcp-server and add a toolset.** Ties the release cadence to upstream and needs local filesystem access the upstream server deliberately lacks.

## Consequences

- The spike's offline test cases become the Go table tests; the DoD is identical for both phases.
- Any git behaviour go-git cannot reproduce (mode detection, packed refs edge cases) is found in phase 2 review, not phase 1.
- `Bash(git *)` stays allowed for agents until the product ships; whether to add read verbs to the MCP tool is a later decision.
