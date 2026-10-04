#!/usr/bin/env python3
"""Minimal versioned subprocess provider. stdout is reserved for its result."""
import json
import sys

request = json.load(sys.stdin)
if request["version"] != "1":
    raise ValueError("unsupported provider protocol")
print("custom provider running", file=sys.stderr)
json.dump({"arguments": request["arguments"], "workspace": request["workspace"]}, sys.stdout)
print()
