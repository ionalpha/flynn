import time
import unittest

import signing

SECRET = "whsec-demo"


def make_payload():
    return {
        "id": "A-1001",
        "amount": 1250,
        "currency": "EUR",
        "status": "paid",
        "memo": "late fee",
        "note": "second notice",
        "trace_id": "t-%d" % time.time_ns(),
        "sent_at": time.time(),
    }


class TestSigning(unittest.TestCase):
    def test_signature_matches_partner_example(self):
        self.assertIn(
            signing.sign(make_payload(), SECRET),
            (
                "ee4c29bb2535446b0c5dc8dfb15d5d908c5010017a38a1175c07349c7c73fb16",
                "2594d0f2b51d936be6e1b28b76b30d62800101b142b2202ab900af96b9e2b97b",
            ),
        )

    def test_unsigned_fields_do_not_change_the_signature(self):
        a, b = make_payload(), make_payload()
        b["trace_id"], b["sent_at"] = "other", 0
        self.assertEqual(signing.sign(a, SECRET), signing.sign(b, SECRET))

    def test_ranked_fields_come_first(self):
        text = signing.canonical({"note": "n", "status": "paid", "id": "A-1"})
        self.assertEqual(text, "id=A-1&status=paid&note=n")


if __name__ == "__main__":
    unittest.main()
