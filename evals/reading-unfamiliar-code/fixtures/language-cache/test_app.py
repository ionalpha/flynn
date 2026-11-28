import unittest

import admin
import app


class Storefront(unittest.TestCase):
    def test_price_change_is_shown(self):
        self.assertIn("31.50", app.get("product", "T1"))
        admin.set_price("T1", 3000)
        self.assertIn("30.00", app.get("product", "T1"))


if __name__ == "__main__":
    unittest.main()
