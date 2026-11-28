# Changelog

## 2.4.0

- `sessions.active_sessions` returns session ids in creation order.

## 2.3.0

- `sessions.purge_old_sessions` takes the number of days to keep as `days`.

## 2.5.0

- `sessions.archive_old_sessions(now, days=30)` moves sessions older than `days` to `session_archive`. `purge_old_sessions` calls it, warns, and is removed in 3.0.0.
