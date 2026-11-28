import customers
import localefmt
import pricing


def render(invoice):
    who = customers.get(invoice["customer_id"])
    f = localefmt.for_locale(who["locale"])
    net = sum(line["cents"] for line in invoice["lines"])
    vat = pricing.vat_cents(net)
    out = ["Invoice %s for %s" % (invoice["id"], who["name"])]
    for line in invoice["lines"]:
        out.append("%s: %s" % (line["desc"], f.fmt(line["cents"])))
    out.append("VAT %d%%: %s" % (pricing.VAT_PERCENT, f.fmt(vat)))
    out.append("Total: %s" % f.fmt(net + vat))
    return out
