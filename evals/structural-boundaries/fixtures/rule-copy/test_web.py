import unittest

import web


class WebTest(unittest.TestCase):
    def test_submit(self):
        db = {"customers": {"c": {"blocked": False, "limit_cents": 100}}, "orders": [], "invoices": []}
        self.assertEqual(web.submit(db, "c", 50), "ok")


if __name__ == "__main__":
    unittest.main()
