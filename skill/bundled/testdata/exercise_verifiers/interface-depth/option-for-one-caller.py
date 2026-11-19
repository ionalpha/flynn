import inspect
import sys

sys.path.insert(0, ".")
fails = []
SALE = {"currency": "USD", "items": [("Green tea", 1250), ("Mug", 899)]}
try:
    import cart
    import invoice
    import money
    import receipt

    params = list(inspect.signature(money.format_price).parameters.values())
    if [p.name for p in params] != ["cents", "currency"] or any(p.default is not inspect.Parameter.empty for p in params):
        fails.append("format_price's signature is now %s; every caller reads past a parameter one caller needed" % (inspect.signature(money.format_price),))
    for args, want in (((1250, "USD"), "$12.50"), ((99, "CAD"), "CA$0.99"), ((100000, "AUD"), "A$1000.00")):
        try:
            got = money.format_price(*args)
        except Exception as e:
            got = repr(e)
        if got != want:
            fails.append("format_price%r is %r, want %r" % (args, got, want))
    if cart.cart_line("Mug", 899, "USD") != "Mug: $8.99":
        fails.append("the cart line changed: %r" % (cart.cart_line("Mug", 899, "USD"),))
    if invoice.invoice_total([1250, 899], "CAD") != "Total due: CA$21.49":
        fails.append("the invoice total changed: %r" % (invoice.invoice_total([1250, 899], "CAD"),))
    if money.format_price(0, "USD") != "$0.00" or money.format_price(5, "CAD") != "CA$0.05":
        fails.append("format_price on small amounts gives %r and %r" % (money.format_price(0, "USD"), money.format_price(5, "CAD")))
    cad = [x for x in receipt.receipt_lines({"currency": "CAD", "items": [("Mug", 899)]}) if "Mug" in x]
    if not cad or "8.99" not in cad[0] or "$" in cad[0] or "CA" in cad[0]:
        fails.append("the receipt line for a sale in CAD is %r, want the amount 8.99 with no symbol or prefix" % (cad,))
    lines = receipt.receipt_lines(SALE)
    if not any("USD" in line for line in lines):
        fails.append("the receipt header no longer names the currency: %r" % (lines,))
    for name, amount in (("Green tea", "12.50"), ("Mug", "8.99")):
        line = [x for x in lines if name in x]
        if not line or amount not in line[0] or "$" in line[0]:
            fails.append("the receipt line for %s is %r, want the amount %s with no symbol" % (name, line, amount))
except Exception as e:
    fails.append("the price formatting could not be exercised: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
