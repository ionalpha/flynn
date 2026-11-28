# Signature

The canonical form is the payload's fields as name=value pairs joined with "&".

Order: the fields listed in fields.RANK come first, in RANK order. Every other field
follows them, by name in ascending order. The fields in fields.UNSIGNED are left out.

The signature is the lowercase hex HMAC-SHA256 of the canonical form under the shared
secret.
