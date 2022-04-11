#! /usr/bin/env python3

import base64
import json
import sys


def fixup(obj):
    if isinstance(obj, list):
        new = []
        for elem in obj:
            if isinstance(elem, str):
                elem = convert(elem)
            else:
                elem = fixup(elem)
            new.append(elem)
        return new
    if isinstance(obj, dict):
        new = {}
        for key, val in obj.items():
            if isinstance(val, str):
                val = convert(val)
            else:
                val = fixup(val)
            new[key] = val
        return new
    if isinstance(obj, str):
        return convert(val)
    return obj


def convert(val):
    if val.endswith("="):
        return base64.b64decode(val).hex()
    return val


input = sys.stdin.read()
data = json.loads(input)
data = fixup(data)
print(json.dumps(data, indent=4))
