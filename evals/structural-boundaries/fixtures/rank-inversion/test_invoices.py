import unittest

import invoices


class TestInvoices(unittest.TestCase):
    def test_render(self):
        inv = {"id": "inv1", "customer_id": "c1", "lines": [{"desc": "Widget", "cents": 123400}]}
        self.assertEqual(
            invoices.render(inv),
            ["Invoice inv1 for Ada", "Widget: 1,234.00 EUR", "VAT 19%: 234.46 EUR", "Total: 1,468.46 EUR"],
        )


if __name__ == "__main__":
    unittest.main()
