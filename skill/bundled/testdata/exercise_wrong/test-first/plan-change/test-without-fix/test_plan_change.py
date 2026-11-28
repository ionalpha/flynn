import unittest

from accounts import Accounts
from gateway import Gateway


class PlanChange(unittest.TestCase):
    def setUp(self):
        self.acc = Accounts()
        self.gw = Gateway(self.acc, lambda: 0)

    def use(self, n):
        return [self.gw.allow("u") for _ in range(n)]

    def test_upgrade_applies_to_the_next_request_and_keeps_the_count(self):
        self.use(5)
        self.acc.set_plan("u", "pro")
        self.assertEqual(self.use(16), [True] * 15 + [False])

    def test_downgrade_applies_to_the_next_request(self):
        self.acc.set_plan("u", "pro")
        self.use(8)
        self.acc.set_plan("u", "free")
        self.assertEqual(self.use(1), [False])
