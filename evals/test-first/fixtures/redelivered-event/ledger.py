class LedgerUnavailable(Exception):
    pass


class Ledger:
    def __init__(self):
        self._balances = {}

    def credit(self, account, cents):
        self._balances[account] = self._balances.get(account, 0) + cents

    def balance(self, account):
        return self._balances.get(account, 0)
