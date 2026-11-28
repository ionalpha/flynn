import unittest

from events import EventError, parse_event


class TestParse(unittest.TestCase):
    def test_paid(self):
        e = parse_event({"id": "e1", "status": "paid", "amount_cents": 500})
        self.assertEqual((e.status, e.amount_cents), ("paid", 500))

    def test_rejects_unknown_fields(self):
        with self.assertRaises(EventError):
            parse_event({"id": "e1", "status": "paid", "amount_cents": 500, "x": 1})

    def test_rejects_a_bad_amount(self):
        with self.assertRaises(EventError):
            parse_event({"id": "e1", "status": "paid", "amount_cents": "500"})


if __name__ == "__main__":
    unittest.main()
