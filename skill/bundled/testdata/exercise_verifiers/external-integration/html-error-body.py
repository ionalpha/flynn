import contextlib
import io
import json
import logging
import sys

sys.path.insert(0, ".")
fails = []
records = []


class Keep(logging.Handler):
    def emit(self, record):
        try:
            records.append(record.getMessage())
        except Exception:
            records.append(str(record.msg))


logging.getLogger().addHandler(Keep())
logging.getLogger().setLevel(logging.DEBUG)
PAGE = "<html><head><title>502 Bad Gateway</title></head>\n<body><center>nginx</center></body></html>\n"

try:
    import invoices
    import transport

    def case(status, ctype, body):
        records.clear()

        def get(url, *args, **kwargs):
            return transport.Response(status, {"Content-Type": ctype}, body)

        transport.get = get
        out = io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(out):
            try:
                got, err = invoices.get_invoice("inv_1"), None
            except Exception as e:
                got, err = None, e
        said = " ".join([str(err) if err else ""] + records + [out.getvalue()])
        return got, err, said

    got, err, said = case(502, "text/html; charset=utf-8", PAGE)
    if err is None:
        fails.append("a 502 HTML page from the proxy returned %r instead of raising" % (got,))
    else:
        if isinstance(err, json.JSONDecodeError):
            fails.append("a 502 HTML page still surfaces as a JSON decode error")
        for want, what in (("502", "the status"), ("text/html", "the content type"), ("Bad Gateway", "the first line of the body")):
            if want not in said:
                fails.append("the failure for a 502 HTML page does not say %s (%s): %r" % (what, want, said[:300]))
    got, err, said = case(504, "text/plain", "upstream request timeout" + chr(10))
    if err is None:
        fails.append("a 504 with a plain-text body returned %r instead of raising" % (got,))
    else:
        for want, what in (("504", "the status"), ("text/plain", "the content type"), ("upstream request timeout", "the body")):
            if want not in said:
                fails.append("the failure for a 504 plain-text body does not say %s (%s): %r" % (what, want, said[:300]))
    got, err, said = case(503, "application/json", json.dumps({"message": "Service Unavailable"}))
    if err is None:
        fails.append("a 503 with a JSON body came back as an invoice: %r" % (got,))
    got, err, said = case(400, "application/json", json.dumps({"error": {"message": "invoice id is malformed"}}))
    if err is None or "invoice id is malformed" not in said:
        fails.append("a 400 should raise with the service's message; got %r, error %r" % (got, err))
    got, err, said = case(200, "application/json", json.dumps({"id": "inv_1", "amount_cents": 500}))
    if err is not None or not isinstance(got, dict) or got.get("id") != "inv_1":
        fails.append("a 200 invoice no longer comes back: %r, error %r" % (got, err))
except Exception as e:
    fails.append("the invoice client could not be exercised: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
