import unittest

import service


class Handle(unittest.TestCase):
    def test_resolves_each_key(self):
        self.assertEqual(service.handle(["a", "b"]), ["A!", "B!"])


if __name__ == "__main__":
    unittest.main()
