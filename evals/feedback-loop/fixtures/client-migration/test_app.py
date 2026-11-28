import importlib
import pkgutil
import unittest

import app


class AppTest(unittest.TestCase):
    def test_every_module_reads_a_user(self):
        for info in pkgutil.iter_modules(app.__path__):
            with self.subTest(module=info.name):
                module = importlib.import_module("app." + info.name)
                self.assertIsNotNone(module.run(1, False))


if __name__ == "__main__":
    unittest.main()
