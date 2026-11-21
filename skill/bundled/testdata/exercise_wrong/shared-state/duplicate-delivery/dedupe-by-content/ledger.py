"""The account ledger, kept in a JSON file so it survives a restart."""

import json
import os


class Ledger:
    def __init__(self, path):
        self.path = path
        self._doc = {"balances": {}}
        if os.path.exists(path):
            with open(path, encoding="utf-8") as f:
                self._doc = json.load(f)

    def _save(self):
        tmp = self.path + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(self._doc, f)
        os.replace(tmp, self.path)

    def balance(self, account):
        return self._doc["balances"].get(account, 0)

    def credit(self, account, cents):
        applied = self._doc.setdefault("applied", [])
        key = f"{account}:{cents}"
        if key in applied:
            return False
        balances = self._doc["balances"]
        balances[account] = balances.get(account, 0) + cents
        applied.append(key)
        self._save()
        return True
