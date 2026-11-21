import unittest

import receipt

ORDER = {"id": "R1", "method": "card", "total": 123450, "currency": "USD"}


class Receipt(unittest.TestCase):
    def test_has_the_lines(self):
        text = receipt.render(ORDER)
        self.assertIn("Receipt R1", text)
        self.assertIn("Paid by card", text)
        self.assertIn("Total:", text)


if __name__ == "__main__":
    unittest.main()
