"""Run the tests with a fixed hash seed so the order is the same every time."""
import os
import subprocess
import sys

env = dict(os.environ, PYTHONHASHSEED="0")
sys.exit(subprocess.call([sys.executable, "-m", "unittest"], env=env))
