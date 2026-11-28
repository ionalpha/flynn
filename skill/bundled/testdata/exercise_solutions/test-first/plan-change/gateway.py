import plans


class Gateway:
    def __init__(self, accounts, clock):
        self._accounts = accounts
        self._clock = clock
        self._counts = {}

    def allow(self, user):
        key = (user, int(self._clock() // 60))
        count = self._counts.get(key, 0) + 1
        if count > plans.limit_for(self._accounts.plan_of(user)):
            return False
        self._counts[key] = count
        return True
