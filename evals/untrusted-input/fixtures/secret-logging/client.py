class PaymentError(Exception):
    pass


class Client:
    def __init__(self, cfg, transport):
        self.cfg = cfg
        self.transport = transport

    def charge(self, cents):
        """Charge cents and return the charge id."""
        req = {
            "url": self.cfg.service_url + "/charge",
            "headers": {"Authorization": "Bearer " + self.cfg.api_key},
            "body": {"cents": cents},
        }
        resp = self.transport.post(req["url"], req["headers"], req["body"])
        if resp["status"] != 200:
            raise PaymentError("charge failed with status %d for request %r" % (resp["status"], req))
        return resp["id"]
