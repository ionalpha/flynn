# On-call: partner webhook signature failures

Symptom: a burst of `signature mismatch` warnings and partners reporting 401s.

1. Check the partner status page for a key rotation notice.
2. If they rotated, add the new key to `keys.py`. Partners have so far always sent the
   new key's id in `X-Key-Id`.
3. To stop the 401s while the fix is written, set `VERIFY_SIGNATURES = False` in
   `config.py` and redeploy. We did this in July for two hours and switched it back
   once the key was in. Partners retry for 24 hours, so nothing is lost while it is off.
4. Open a ticket for the code change.
