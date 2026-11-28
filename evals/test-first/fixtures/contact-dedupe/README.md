# contacts

`merge_contacts(records)` in `contacts.py` collapses duplicate contacts. A record is a dict with `id` (str), `name` (str), `emails` (list of str), `phones` (list of str) and `updated` (int, larger is newer).

- An email's key is the address stripped of surrounding spaces and lower-cased, with any `+tag` removed from the part before the `@`. `Ann+news@Example.com ` has the key `ann@example.com`.
- A phone's key is its digits only, and only the last 10 digits when there are more. A phone with fewer than 7 digits has no key and never matches anything.
- Two records are the same person when they share a key. The relation is transitive: if A shares a key with B and B shares another with C, all three are one person, whatever order they come in.
- The result has one dict per person, ordered by `id`, with `id` (the smallest id of the group), `name` (the name of the group's newest record; on equal `updated` the record with the smaller id), `emails` (the sorted unique email keys) and `phones` (the sorted unique phone keys).
- A record with no keys is its own person and is never merged.
- `records` and the dicts in it are not modified.
