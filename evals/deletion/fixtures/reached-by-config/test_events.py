import unittest

import events


class TestEvents(unittest.TestCase):
    def test_signup(self):
        self.assertEqual(events.dispatch("signup", {"name": "Ada"}), "welcome Ada")

    def test_unknown(self):
        with self.assertRaises(ValueError):
            events.dispatch("nope", {})


if __name__ == "__main__":
    unittest.main()
