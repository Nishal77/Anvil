<!--
Title must follow: <type>(<scope>): <summary> (<REQ-ID>)
e.g.  feat(queue): reclaim jobs with expired leases (FR-012)
-->

## What and why

<!-- What changes, and WHY it is needed. The diff shows what; explain why. -->

Closes #

**Requirements:** <!-- FR-012, SEC-001, or "none" -->
**Phase / week:** <!-- BUILD-PLAN P1 W2, or "n/a" for external contributions -->

## Approach

<!-- Non-obvious decisions and what you rejected. If a reviewer would ask
     "why not X?", answer it here rather than in a review thread. -->

## Testing

<!-- What you added and what it proves. For concurrency or recovery changes,
     say how the property is actually exercised under contention. -->

- [ ] Unit tests added
- [ ] Integration tests added or updated (storage / queue / events changes)
- [ ] Bug fix has a failing-first test
- [ ] Security test added (`test/security/`) if a sandbox restriction changed
- [ ] Chaos test added or updated if recovery behavior changed

## Checklist

- [ ] `make ci` green locally
- [ ] `make test-integration` green (if applicable)
- [ ] No item from the forbidden list (CLAUDE.md §5.2)
- [ ] Errors wrapped with context across package boundaries
- [ ] Every new goroutine has a documented owner and shutdown path
- [ ] Exported identifiers documented
- [ ] `docs/PRD.md` updated **in this PR** if the spec changed
- [ ] `CHANGELOG.md` updated for user-visible changes
- [ ] ADR added if a technology, boundary, or security posture changed
- [ ] No unrelated changes

## Security impact

<!-- Required if this touches: sandbox, policy engine, secrets, auth, egress,
     or SQL. Otherwise write "none". Do not skip this field. -->

**Threat model delta:**

## Breaking changes

<!-- API, schema, or config changes, and the migration path. Otherwise "none". -->

---

<details>
<summary>Reviewer notes</summary>

Focus areas: <!-- where you want the most scrutiny -->

Known gaps: <!-- what is deliberately not addressed here, and why it is safe
                 to defer. Being honest here speeds up review. -->

</details>