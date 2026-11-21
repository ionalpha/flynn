import unittest

from events import parse_event


class TestParse(unittest.TestCase):
    def test_paid(self):
        e = parse_event({"id": "e1", "status": "paid", "amount_cents": 500, "region": "eu"})
        self.assertEqual((e.status, e.amount_cents), ("paid", 500))


if __name__ == "__main__":
    unittest.main()
