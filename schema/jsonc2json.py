#!/usr/bin/env python3
"""Strip JSONC comments (// and /* */) from stdin, write JSON to stdout."""
import sys


def strip_comments(text):
    out = []
    i = 0
    in_string = False
    escape = False
    while i < len(text):
        c = text[i]
        if in_string:
            out.append(c)
            if escape:
                escape = False
            elif c == "\\":
                escape = True
            elif c == '"':
                in_string = False
        elif c == '"':
            in_string = True
            out.append(c)
        elif c == "/" and i + 1 < len(text):
            if text[i + 1] == "/":
                i += 2
                while i < len(text) and text[i] != "\n":
                    i += 1
                continue
            elif text[i + 1] == "*":
                i += 2
                while i + 1 < len(text) and not (text[i] == "*" and text[i + 1] == "/"):
                    i += 1
                i += 2
                continue
            else:
                out.append(c)
        else:
            out.append(c)
        i += 1
    return "".join(out)


sys.stdout.write(strip_comments(sys.stdin.read()))
