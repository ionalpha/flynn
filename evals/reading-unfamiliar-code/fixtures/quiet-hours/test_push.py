import unittest

import push


class SendPush(unittest.TestCase):
    def test_line(self):
        self.assertEqual(push.send_push({"name": "kim"}, {"text": "hi"}), "mobile kim hi")


if __name__ == "__main__":
    unittest.main()
