import customers
import money
import pricing


def render(invoice):
    who = customers.get(invoice["customer_id"])
    net = sum(line["cents"] for line in invoice["lines"])
    vat = pricing.vat_cents(net)
    out = ["Invoice %s for %s" % (invoice["id"], who["name"])]
    for line in invoice["lines"]:
        out.append("%s: %s" % (line["desc"], money.fmt(line["cents"], locale=who["locale"])))
    out.append("VAT %d%%: %s" % (pricing.VAT_PERCENT, money.fmt(vat, locale=who["locale"])))
    out.append("Total: %s" % money.fmt(net + vat, locale=who["locale"]))
    return out
