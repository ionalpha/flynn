import unittest

from accounts import Accounts
from gateway import Gateway


class PlanChange(unittest.TestCase):
    def test_upgrade_applies_to_the_next_request(self):
        acc = Accounts()
        gw = Gateway(acc, lambda: 0)
        for _ in range(6):
            gw.allow("u")
        acc.set_plan("u", "pro")
        self.assertTrue(gw.allow("u"))
        self.assertTrue(gw.allow("u"))

    def test_other_users_keep_their_plan(self):
        acc = Accounts()
        gw = Gateway(acc, lambda: 0)
        acc.set_plan("a", "pro")
        self.assertEqual([gw.allow("b") for _ in range(6)], [True] * 5 + [False])
