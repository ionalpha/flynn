import unittest

from billing.invoice import invoice_total
from billing.money import parse_money
from billing.refund import refund_cents


class MoneyTest(unittest.TestCase):
    def test_plain(self):
        self.assertEqual(parse_money("12.50"), 1250)

    def test_thousands(self):
        self.assertEqual(parse_money("1,234.50"), 123450)

    def test_whole(self):
        self.assertEqual(parse_money("7"), 700)

    def test_invoice(self):
        self.assertEqual(invoice_total(["1.00", "2.50"]), 350)

    def test_refund(self):
        self.assertEqual(refund_cents("5.00", 100), 400)
