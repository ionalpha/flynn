import json
import unittest

from orders import order_payload


class TestPayload(unittest.TestCase):
    def test_payload_is_json(self):
        p = order_payload({"id": "o1", "total_cents": 1999, "currency": "USD"})
        self.assertEqual(json.loads(json.dumps(p))["id"], "o1")


if __name__ == "__main__":
    unittest.main()
