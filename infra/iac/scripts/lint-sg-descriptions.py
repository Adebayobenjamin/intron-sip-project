#!/usr/bin/env python3
"""Reject security group descriptions AWS will refuse at apply time.

EC2 accepts only this character set in a security group or rule description:

    a-zA-Z0-9 . _ - : / ( ) # , @ [ ] + = & ; { } ! $ *

Anything else - an em dash, a curly quote, an apostrophe, an ellipsis - fails
with InvalidParameterValue. `terraform validate` and `terraform plan` both pass
happily, because it is the EC2 API that objects, not the schema. So the failure
lands mid-apply with a partially built stack, which is the worst time for it.

This exists because that happened: six descriptions carried em dashes, and only
two surfaced on the first apply because Terraform stopped at the first error in
each dependency branch. Fixing the two visible ones would have produced the same
failure again on the next run.

Note the apostrophe in particular. "Kamailio's control interface" is a natural
thing to write and AWS rejects it - which is why this checks the whole set
rather than just looking for non-ASCII.

Run:  make lint        (from infra/iac)
Exit: 0 clean, 1 offenders found
"""

import re
import sys
import pathlib

ALLOWED = re.compile(r"^[a-zA-Z0-9 ._\-:/()#,@\[\]+=&;{}!$*]*$")

SG_RESOURCE = re.compile(
    r'resource\s+"(aws_security_group|aws_vpc_security_group_(?:ingress|egress)_rule)"\s+"([^"]+)"\s*\{'
)
DESCRIPTION = re.compile(r'\s*description\s*=\s*"(.*)"\s*$')
# ${...} resolves to a CIDR or an ARN at apply time. Always in the allowed set,
# and the literal braces are themselves allowed, so strip before checking.
INTERPOLATION = re.compile(r"\$\{[^}]*\}")


def offenders(root: pathlib.Path):
    for path in sorted(root.rglob("*.tf")):
        lines = path.read_text().splitlines()
        inside = False
        depth = 0
        name = ""

        for lineno, raw in enumerate(lines, start=1):
            opened = SG_RESOURCE.search(raw)
            if opened:
                inside = True
                name = f"{opened.group(1)}.{opened.group(2)}"
                depth = raw.count("{") - raw.count("}")
                continue

            if not inside:
                continue

            depth += raw.count("{") - raw.count("}")

            found = DESCRIPTION.match(raw)
            if found:
                text = found.group(1)
                probe = INTERPOLATION.sub("", text)
                if not ALLOWED.match(probe):
                    bad = sorted({c for c in probe if not ALLOWED.match(c)})
                    yield path, lineno, name, bad, text

            if depth <= 0:
                inside = False


def main() -> int:
    root = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else "stacks")
    if not root.exists():
        print(f"lint-sg-descriptions: no such directory: {root}", file=sys.stderr)
        return 1

    found = list(offenders(root))
    if not found:
        print("security group descriptions: OK")
        return 0

    print(f"{len(found)} security group description(s) AWS will reject:\n", file=sys.stderr)
    for path, lineno, name, bad, text in found:
        rendered = " ".join(f"{c!r} (U+{ord(c):04X})" for c in bad)
        print(f"  {path}:{lineno}", file=sys.stderr)
        print(f"    {name}", file=sys.stderr)
        print(f"    rejected: {rendered}", file=sys.stderr)
        print(f"    {text}", file=sys.stderr)
        print(file=sys.stderr)
    print(
        "Allowed set: a-zA-Z0-9 and . _ - : / ( ) # , @ [ ] + = & ; { } ! $ *\n"
        "Comments are unaffected - only the description argument is.",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    sys.exit(main())
