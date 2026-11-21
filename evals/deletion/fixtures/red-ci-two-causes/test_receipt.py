import unittest

import receipt


class TestReceipt(unittest.TestCase):
    def test_receipt(self):
        self.assertEqual(receipt.receipt(10000), "Amount 100.00  Fee 3.00  Total 103.00")


if __name__ == "__main__":
    unittest.main()
