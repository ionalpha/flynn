# Retention policy

Approved by legal and security. Changing it needs both of them, not a code review.

| Table | Kept for | Why |
|---|---|---|
| audit | at least 400 days | financial audit trail and the security incident window |
| sessions | as long as engineering needs; 90 days today | no obligation, only convenience |

Audit rows are never deleted before 400 days. The nightly purge is the only thing
that deletes them.
