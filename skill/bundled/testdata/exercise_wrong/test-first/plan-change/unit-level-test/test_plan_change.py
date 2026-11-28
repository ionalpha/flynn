import unittest

import plans
from accounts import Accounts


class PlanChange(unittest.TestCase):
    def test_pro_limit(self):
        self.assertEqual(plans.limit_for("pro"), 20)

    def test_set_plan_is_remembered(self):
        acc = Accounts()
        acc.set_plan("u", "pro")
        self.assertEqual(acc.plan_of("u"), "pro")
