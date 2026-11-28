import unittest

import api
import gateway
import jobqueue
import worker


class ChargeTest(unittest.TestCase):
    def test_order_is_charged(self):
        api.enqueue_charge({"customer": "c1", "total_cents": 500})
        worker.handle(jobqueue.take())
        self.assertEqual(gateway.charges[-1], ("c1", 500, "usd"))


if __name__ == "__main__":
    unittest.main()
