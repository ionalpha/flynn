import unittest

import cli


class RenderTest(unittest.TestCase):
    def test_lists_every_item(self):
        out = cli.render([{"name": "backup", "owner": "ana", "state": "done"}])
        for value in ("backup", "ana", "done"):
            self.assertIn(value, out)


if __name__ == "__main__":
    unittest.main()
