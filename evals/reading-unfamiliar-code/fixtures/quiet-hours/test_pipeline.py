import unittest

import pipeline


class Process(unittest.TestCase):
    def test_delivers_on_every_channel_in_order(self):
        users = {"u": {"name": "kim", "channels": ["email", "mobile"]}}
        out = []
        pipeline.process(users, {"user": "u", "kind": "order.shipped", "order": "Z9", "at": "2024-01-01T12:00Z"}, out)
        self.assertEqual(out, ["email kim Order Z9 shipped", "mobile kim Order Z9 shipped"])


if __name__ == "__main__":
    unittest.main()
