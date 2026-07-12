# Task 6 Brief: Immutable Dream Outputs and Evidence Drill-Down

Starting commit: `e0c4485b87679674f32adbc8f65eabd48a5956b3`

Implement only plan Task 6. Preserve the workflow-only execution and hierarchical scheduler behavior from Tasks 1-5.

Acceptance requirements:

- A successful Dream output persists display, retrieval and sealed-pointer layers plus structured facts/themes/trends/risks/todos, exact child-run lineage, evidence links, search index jobs and a timeline node.
- Database-visible output writes are one transaction. Any object-store, transaction/index/timeline/lineage write failure fails the run and exposes no successful display summary.
- Add ticket-guarded overview, run-list, run-detail, annotation, rerun and evidence-access routes.
- Annotations are append-only actions `confirm|reject|mark_incorrect|comment` and never rewrite summaries.
- Evidence access requires `dream:evidence:read`, a ticket/enterprise/pointer-bound AgentNexus locate/read grant, and a successful mandatory audit append before returning sanitized evidence.
- Do not return raw sealed storage locations or unmasked content through ordinary list/detail APIs.
- Use bounded queries and preserve enterprise isolation.

Verification includes focused Dream/app tests, service tests, fresh PostgreSQL plus S3-compatible integration, SDK/contracts/generation, vet/race, and clean generation diff.
