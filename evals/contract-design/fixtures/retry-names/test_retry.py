import unittest
from unittest import mock

import retry


class TestRetry(unittest.TestCase):
    def test_succeeds_after_failures(self):
        calls = []

        def fn():
            calls.append(1)
            if len(calls) < 3:
                raise OSError("down")
            return "ok"

        with mock.patch("time.sleep") as sleep:
            self.assertEqual(retry.call_with_retry(fn, tries=3, delay=0.5), "ok")
        self.assertEqual([c.args[0] for c in sleep.call_args_list], [0.5, 1.0])


if __name__ == "__main__":
    unittest.main()
