import unittest

from parse import parse


class ParseTest(unittest.TestCase):
    def test_a_failed_login(self):
        self.assertEqual(parse("2026-03-02T08:00:01 web1 user=amir result=fail"), ("amir", "fail"))

    def test_other_lines_are_skipped(self):
        self.assertIsNone(parse("2026-03-02T08:03:05 web1 healthcheck result=ok"))


if __name__ == "__main__":
    unittest.main()
