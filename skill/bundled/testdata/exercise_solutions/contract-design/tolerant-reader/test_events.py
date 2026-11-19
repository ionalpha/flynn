import unittest

from events import UNKNOWN, EventError, parse_event


class TestParse(unittest.TestCase):
    def test_paid(self):
        e = parse_event({"id": "e1", "status": "paid", "amount_cents": 500})
        self.assertEqual((e.status, e.amount_cents), ("paid", 500))

    def test_ignores_fields_it_does_not_read(self):
        e = parse_event({"id": "e1", "status": "paid", "amount_cents": 500, "region": "eu"})
        self.assertEqual(e.status, "paid")

    def test_unseen_status_is_unknown(self):
        e = parse_event({"id": "e1", "status": "disputed", "amount_cents": 500})
        self.assertEqual((e.status, e.raw_status), (UNKNOWN, "disputed"))

    def test_rejects_a_bad_amount(self):
        with self.assertRaises(EventError):
            parse_event({"id": "e1", "status": "paid", "amount_cents": "500"})


if __name__ == "__main__":
    unittest.main()
