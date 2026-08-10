"""Test runner. CI passes a seed so the order varies between builds."""

import random
import sys
import unittest

seed = int(sys.argv[1]) if len(sys.argv) > 1 else 0
loader = unittest.TestLoader()
suite = loader.discover(".", pattern="test_*.py")
cases = list(unittest.TestSuite(suite))
random.Random(seed).shuffle(cases)
result = unittest.TextTestRunner(verbosity=0).run(unittest.TestSuite(cases))
sys.exit(0 if result.wasSuccessful() else 1)
