# Changelog

## 2.3.0

- `slugify(text, max_len=None)` added. Same rules as `slugify_v1`, with the length cap as an argument.
- `slugify_v1` is deprecated and warns on use. It will be removed in 3.0.0.

## 2.2.0

- `badges.badge_id` uses `slugify`.
