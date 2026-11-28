import unittest

from inbox import Seen
from ledger import Ledger
from webhook import handle


class Webhook(unittest.TestCase):
    def test_payment_is_credited(self):
        ledger = Ledger()
        self.assertTrue(handle({"id": "e1", "account": "a", "cents": 500}, ledger, Seen()))
        self.assertEqual(ledger.balance("a"), 500)

    def test_redelivery_is_credited_once(self):
        ledger, seen = Ledger(), Seen()
        event = {"id": "e1", "account": "a", "cents": 500}
        self.assertTrue(handle(event, ledger, seen))
        self.assertTrue(handle(event, ledger, seen))
        self.assertEqual(ledger.balance("a"), 500)


if __name__ == "__main__":
    unittest.main()
