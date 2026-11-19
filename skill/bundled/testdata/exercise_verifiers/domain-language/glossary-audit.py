import os
import shutil
import subprocess
import sys

fails = []


def run(d):
    p = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", ".", "-p", "test*.py"],
                       cwd=d, capture_output=True, text=True, timeout=60)
    return p.returncode, p.stdout + p.stderr


if not os.path.exists("test_glossary.py"):
    fails.append("there is no test_glossary.py")
else:
    rc, out = run(".")
    if rc != 0:
        fails.append("the tests fail on the code as it stands, which uses no displaced word: " + out[-300:])
    cases = [
        ("shop/zz_case.py", "class ClientRecord:\n    pass\n", None, True, "a class named ClientRecord"),
        ("shop/zz_case.py", "def purchaser_name(c):\n    return c\n", None, True, "a function named purchaser_name"),
        ("shop/zz_case.py", "def total(buyOrder):\n    return buyOrder\n", None, True, "an argument named buyOrder"),
        ("shop/zz_case.py", "po_number = 7\n", None, True, "a variable named po_number"),
        ("shop/zz_case.py", "def vendor_list():\n    return []\n",
         "\n## Supplier\n\nA business we buy stock from.\n\nDisplaces: vendor.\n\nSource: the purchasing team.\n",
         True, "a function named vendor_list after the glossary gains Supplier, displacing vendor"),
        ("shop/zz_case.py", "def report_position(post_office, exporter):\n    return post_office\n", None, False,
         "names that only contain the letters of a displaced word (report_position, post_office, exporter)"),
        ("third_party/zz_case.py", "class ApiClient:\n    purchaser = None\n", None, False, "vendored code under third_party/"),
    ]
    for i, (path, code, entry, should_fail, what) in enumerate(cases):
        d = "_mut_%d" % i
        shutil.copytree(".", d, ignore=shutil.ignore_patterns("_mut_*", "__pycache__", ".git"))
        with open(os.path.join(d, path), "w", newline="\n") as f:
            f.write(code)
        if entry:
            with open(os.path.join(d, "GLOSSARY.md"), "a", newline="\n") as f:
                f.write(entry)
        rc, out = run(d)
        if should_fail and rc == 0:
            fails.append("the check passes with " + what)
        if not should_fail and rc != 0:
            fails.append("the check fails on " + what + ": " + out[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
