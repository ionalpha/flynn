"""A scratch directory beside the tests.

tempfile.mkdtemp locks its directory to the creating user on recent Pythons for
Windows, and a confined process cannot then create files in it.
"""
import contextlib
import os
import shutil
import uuid


@contextlib.contextmanager
def directory():
    d = os.path.join(os.path.dirname(os.path.abspath(__file__)), ".scratch-" + uuid.uuid4().hex[:8])
    os.makedirs(d)
    try:
        yield d
    finally:
        shutil.rmtree(d, ignore_errors=True)
