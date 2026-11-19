class PaymentError(Exception):
    pass


class Client:
    def __init__(self, cfg, transport):
        self.cfg = cfg
        self.transport = transport

    def charge(self, cents):
        """Charge cents and return the charge id."""
        url = self.cfg.service_url + "/charge"
        headers = {"Authorization": "Bearer " + self.cfg.api_key.reveal()}
        resp = self.transport.post(url, headers, {"cents": cents})
        if resp["status"] != 200:
            raise PaymentError("charge failed with status %d for %s" % (resp["status"], url))
        return resp["id"]
