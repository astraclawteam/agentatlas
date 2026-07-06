# Contributing to AgentAtlas

AgentAtlas is an open-core enterprise Agent RAG and organizational memory system.

Contributions must preserve:

- The open-core and enterprise repository boundary.
- AgentNexus as the authority for identity, permissions, evidence location, evidence reads, and audit append.
- Production-standard runtime dependencies for integration and end-to-end paths.
- The no-raw-copy data boundary for original enterprise content.

Before opening a pull request:

1. Read `AGENTS.md`.
2. Run the verification command for the current Goal.
3. Confirm no secrets, local paths, private endpoints, customer names, customer documents, or enterprise-only implementation details were added.
4. Keep changes scoped and testable.

