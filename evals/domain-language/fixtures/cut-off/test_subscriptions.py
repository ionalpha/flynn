import unittest
from datetime import date

import subscriptions


class SubscriptionsTest(unittest.TestCase):
    def test_cancelled_keeps_the_paid_period(self):
        sub = {"status": "active", "period_end": date(2026, 5, 31)}
        subscriptions.cancel(sub)
        self.assertTrue(subscriptions.has_access(sub, date(2026, 5, 20)))


if __name__ == "__main__":
    unittest.main()
