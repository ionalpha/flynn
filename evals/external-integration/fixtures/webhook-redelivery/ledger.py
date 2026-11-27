"""Account balances, kept on disk."""

import json
import os

PATH = "ledger.json"

# Tests set this to play the ledger database being unreachable.
DOWN = False


class LedgerUnavailable(Exception):
    pass


def _read():
    if not os.path.exists(PATH):
        return {}
    with open(PATH, encoding="utf-8") as f:
        return json.load(f)


def balance(account):
    return _read().get(account, 0)


def credit(account, amount_cents):
    if DOWN:
        raise LedgerUnavailable("ledger is unreachable")
    data = _read()
    data[account] = data.get(account, 0) + amount_cents
    with open(PATH, "w", encoding="utf-8") as f:
        json.dump(data, f)
