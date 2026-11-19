import ipaddress
import sys

sys.path.insert(0, ".")
import fetcher
import transport

PUB = "93.184.216.34"
META = "169.254.169.254"
fails = []


def private(ip):
    a = ipaddress.ip_address(ip)
    a = getattr(a, "ipv4_mapped", None) or a
    return not a.is_global


def setup():
    transport.DNS.clear()
    transport.SERVERS.clear()
    del transport.CALLS[:]
    d = transport.DNS
    d["ok.example.com"] = [PUB]
    d["hop.example.com"] = [PUB]
    d["redir.example.com"] = [PUB]
    d["internal.example.com"] = ["10.0.0.5"]
    d["mixed.example.com"] = ["10.0.0.7", PUB]
    d["mapped.example.com"] = ["::ffff:" + META]
    d["loop6.example.com"] = ["::1"]
    d["meta.example.com"] = [META]
    d[META] = [META]
    answers = [[PUB], [META]]
    d["rebind.example.com"] = lambda: answers.pop(0) if len(answers) > 1 else answers[0]
    s = transport.SERVERS
    s[(PUB, "ok.example.com", "/")] = (200, {}, b"ok")
    s[(PUB, "hop.example.com", "/")] = (302, {"Location": "http://ok.example.com/"}, b"")
    s[(PUB, "redir.example.com", "/")] = (302, {"Location": "http://meta.example.com/latest"}, b"")
    s[(META, "meta.example.com", "/latest")] = (200, {}, b"METADATA")
    s[("10.0.0.5", "internal.example.com", "/")] = (200, {}, b"INTERNAL")


def run(url):
    try:
        return fetcher.fetch(url), None
    except Exception as e:
        return None, e


for url in ("http://ok.example.com/", "http://hop.example.com/"):
    setup()
    body, err = run(url)
    if body != b"ok":
        fails.append("a legitimate fetch of %s returned %r (%r)" % (url, body, err))
    if any(c[0] != PUB for c in transport.CALLS):
        fails.append("a legitimate fetch dialled %r, not the address it resolved to" % (transport.CALLS,))

hostile = [
    "http://169.254.169.254/latest",
    "http://internal.example.com/",
    "http://mixed.example.com/",
    "http://mapped.example.com/",
    "http://loop6.example.com/",
    "http://rebind.example.com/",
    "http://redir.example.com/",
]
for url in hostile:
    setup()
    body, err = run(url)
    dialled = [c[0] for c in transport.CALLS if private(c[0])]
    if dialled:
        fails.append("%s: connected to internal address %s" % (url, dialled[0]))
    if body in (b"INTERNAL", b"METADATA"):
        fails.append("%s: returned internal content" % url)
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
