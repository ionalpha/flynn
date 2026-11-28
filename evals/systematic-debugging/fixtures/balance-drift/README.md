# balances

`balances.json` is what customers see: each balance in cents, equal to the opening
balance in `opening.json` plus every event in `events.jsonl`.

`sync.py` applies the events it has not applied yet. When `balances.json` is absent
it starts again from the opening balances.
