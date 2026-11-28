import plans


class Gateway:
    def __init__(self, accounts, clock):
        self._accounts = accounts
        self._clock = clock
        self._limits = {}
        self._counts = {}

    def _limit(self, user):
        if user not in self._limits:
            self._limits[user] = plans.limit_for(self._accounts.plan_of(user))
        return self._limits[user]

    def allow(self, user):
        key = (user, int(self._clock() // 60))
        count = self._counts.get(key, 0) + 1
        if count > self._limit(user):
            return False
        self._counts[key] = count
        return True
