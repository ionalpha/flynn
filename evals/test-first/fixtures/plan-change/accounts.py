class Accounts:
    def __init__(self):
        self._plans = {}

    def plan_of(self, user):
        return self._plans.get(user, "free")

    def set_plan(self, user, plan):
        self._plans[user] = plan
