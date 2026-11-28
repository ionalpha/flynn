Webhook signer for the partner API. The signature is described in partner-spec.md.

CI runs `python3 -m unittest discover -q`. test_signing fails on about one build in
two and passes when the job is run again. It started failing the week the memo and
note fields were added to the payload.
