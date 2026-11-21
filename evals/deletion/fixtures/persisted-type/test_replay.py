import unittest

from replay import apply


class ReplayTest(unittest.TestCase):
    def test_created(self):
        self.assertEqual(apply({"total": 0}, {"type": "invoice.created", "amount": 5})["total"], 5)

    def test_voided(self):
        self.assertEqual(apply({"total": 9}, {"type": "invoice.voided", "amount": 4})["total"], 5)

    def test_unknown(self):
        with self.assertRaises(ValueError):
            apply({"total": 0}, {"type": "invoice.other"})


if __name__ == "__main__":
    unittest.main()
