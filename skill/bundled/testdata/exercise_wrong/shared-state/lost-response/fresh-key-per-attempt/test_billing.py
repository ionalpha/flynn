import unittest
from unittest import mock

import billing
from vendor.gateway import Gateway

INVOICES = [{"id": "inv-1", "account": "a1", "cents": 500}, {"id": "inv-2", "account": "a2", "cents": 700}]


@mock.patch("billing.time.sleep", lambda s: None)
class Settle(unittest.TestCase):
    def test_pays_every_invoice(self):
        gw = Gateway()
        self.assertEqual(billing.settle(gw, INVOICES), {"paid": ["inv-1", "inv-2"], "declined": []})
        self.assertEqual(gw.applied, [("a1", 500), ("a2", 700)])

    def test_declined_invoice_is_reported(self):
        gw = Gateway(["decline"] * 3)
        got = billing.settle(gw, INVOICES[:1])
        self.assertEqual(got, {"paid": [], "declined": ["inv-1"]})
        self.assertEqual(len(gw.calls), 1)

    def test_a_lost_response_is_retried_without_a_second_charge(self):
        gw = Gateway(["lose"])
        self.assertEqual(billing.settle(gw, INVOICES[:1])["paid"], ["inv-1"])
        self.assertEqual(gw.applied, [("a1", 500)])

    def test_an_unclassified_error_is_not_retried(self):
        gw = Gateway(["garbled"])
        with self.assertRaises(ValueError):
            billing.settle(gw, INVOICES[:1])
        self.assertEqual(len(gw.calls), 1)


if __name__ == "__main__":
    unittest.main()
