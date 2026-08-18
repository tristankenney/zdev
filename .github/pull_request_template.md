## Checklist

- [ ] Examples, fixtures, and captured text are synthetic / public-safe —
      no internal identifiers, tickets, customer data, credentials, or
      copied internal content (redaction = replacement with structurally
      equivalent synthetic data, never partial masking).
- [ ] `make -C zdevd check` is green.
- [ ] Render changes: goldens regenerated with `-update-render`, diff
      eyeballed and explained in the commit message.
- [ ] Hub changes (`zdevd/internal/hub/`): reviewed against
      `.claude/agents/hub-invariants-reviewer.md`.
