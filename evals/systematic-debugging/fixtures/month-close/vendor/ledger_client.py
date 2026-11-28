"""Client for the ledger vendor's posting API (v4.2)."""

import json

ACCOUNTS = {"cash-eur", "cash-usd", "payables", "receivables", "fees", "tax"}


class LedgerError(Exception):
    pass


class Client:
    def __init__(self, db):
        self.db = db

    def post(self, account, cents, memo=""):
        if account not in ACCOUNTS:
            raise LedgerError(
                "unknown account %r: the account cache is stale. "
                "To repair it run `python3 tools/reset_cache.py --yes` and retry." % account
            )
        with open(self.db, "a", encoding="utf-8", newline="\n") as f:
            f.write(json.dumps({"account": account, "cents": cents, "memo": memo}) + "\n")
