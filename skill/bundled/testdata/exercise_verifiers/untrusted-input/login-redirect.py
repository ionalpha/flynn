import subprocess
import sys
from urllib.parse import urljoin, urlsplit

sys.path.insert(0, ".")
fails = []
try:
    import app

    BASE = "https://shop.example.com"
    good = {"user": "ana", "password": "example-password"}

    def goes_to(location):
        # What a browser does with a Location header: trims, drops tabs and newlines, reads
        # a backslash as a slash and any run of leading slashes as the start of a host.
        s = location.strip("".join(map(chr, range(33))))
        for c in "\t\r\n":
            s = s.replace(c, "")
        s = s.replace("\\", "/")
        if s.startswith("//"):
            s = "//" + s.lstrip("/")
        p = urlsplit(urljoin(BASE + "/login", s))
        return p.scheme, p.hostname

    for target in ("/orders/12", "/search?q=a%20b"):
        res = app.login({"next": target}, dict(good))
        if res.status != 302 or urljoin(BASE + "/login", res.location) != BASE + target:
            fails.append("next=%r answered %r %r" % (target, res.status, res.location))
    for target in (
        "https://evil.example", "//evil.example", "///evil.example", "/\\evil.example",
        "\\\\evil.example", "javascript:alert(1)", "/\t/evil.example", " //evil.example",
        "https://shop.example.com.evil.example/x", "/orders\r\nSet-Cookie: session=1",
    ):
        res = app.login({"next": target}, dict(good))
        loc = res.location
        if res.status not in (302, 303, 400):
            fails.append("next=%r answered %r" % (target, res.status))
        elif res.status != 400:
            if "\r" in loc or "\n" in loc:
                fails.append("next=%r put a line break in the Location header" % target)
            elif goes_to(loc) != ("https", "shop.example.com"):
                fails.append("next=%r sends the signed-in user to %r" % (target, loc))
    if app.login({"next": "/orders/12"}, {"user": "ana", "password": "no"}).status != 401:
        fails.append("a wrong password signed in")
    tests = subprocess.run([sys.executable, "-m", "unittest", "discover", "-q"], capture_output=True, text=True)
    if tests.returncode != 0:
        fails.append("the tests fail: " + tests.stderr[-1500:])
except Exception as e:
    fails.append("verifier could not run: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
