import json
import unittest

import keys
import webhooks

BODY = json.dumps({"event": "invoice.paid", "id": "ev_1"}).encode()


def headers(key_id=None):
    kid = key_id or keys.CURRENT
    h = {"X-Timestamp": "1700000000", "X-Signature": webhooks.sign(keys.KEYS[kid]["secret"], "1700000000", BODY)}
    if key_id:
        h["X-Key-Id"] = key_id
    return h


class VerifyTests(unittest.TestCase):
    def test_legacy_partner(self):
        self.assertEqual(webhooks.verify(headers(), BODY)["id"], "ev_1")

    def test_rotated_key(self):
        self.assertEqual(webhooks.verify(headers("pk-2026"), BODY)["id"], "ev_1")

    def test_tampered_body(self):
        with self.assertRaises(webhooks.SignatureError):
            webhooks.verify(headers(), BODY + b" ")


if __name__ == "__main__":
    unittest.main()
