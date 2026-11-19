"""Test runner."""

import sys
import unittest

suite = unittest.TestLoader().discover(".", pattern="test_*.py")
result = unittest.TextTestRunner(verbosity=0).run(suite)
sys.exit(0 if result.wasSuccessful() else 1)
