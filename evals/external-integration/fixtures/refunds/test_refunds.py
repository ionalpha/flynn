import json
import unittest

import refunds
import transport


class RefundTest(unittest.TestCase):
    def test_refund_returns_provider_id(self):
        sent = []

        def post(url, body, headers=None):
            sent.append(body)
            return transport.Response(200, {}, json.dumps({"id": "re_1"}))

        transport.post = post
        got = refunds.issue_refund({"id": "rr_1", "order_id": "o_77", "amount_cents": 1000})
        self.assertEqual(got, "re_1")
        self.assertEqual(sent[0]["amount_cents"], 1000)


if __name__ == "__main__":
    unittest.main()
