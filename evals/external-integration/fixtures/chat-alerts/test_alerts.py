import json
import unittest

import alerts
import jobs
import transport


def boom():
    raise ValueError("disk full")


class AlertTest(unittest.TestCase):
    def test_failed_job_is_announced(self):
        sent = []

        def post(url, body, headers=None):
            sent.append(body)
            return transport.Response(200, {}, json.dumps({"ok": True, "ts": "1"}))

        transport.post = post
        got = jobs.run_nightly([("backup", boom), ("report", lambda: None)])
        self.assertEqual(got["report"], "ok")
        self.assertIn("backup", sent[0]["text"])


if __name__ == "__main__":
    unittest.main()
