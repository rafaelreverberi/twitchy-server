#!/usr/bin/env python3
"""Validate responses against the unchanged OpenAPI 3.1 schema snapshot."""
import hashlib
import json
import pathlib
import sys
import jsonschema
root = pathlib.Path(__file__).resolve().parents[1]
raw = (root/'docs/contract/SERVER_API.json').read_bytes()
assert hashlib.sha256(raw).hexdigest() == 'afd46e988fc51ee07f64a08966497c2fdf0f5fb6fa89a40eff1a96e5d611dfde'
doc = json.loads(raw)
report = json.loads(pathlib.Path(sys.argv[1]).read_text())
count = 0
for result in report:
    route, method, status = result['route'], result['method'], str(result['status'])
    response = doc['paths'][route][method]['responses'][status]
    if 'content' in response:
        schema = dict(response['content']['application/json']['schema'])
        schema['components'] = doc['components']
        jsonschema.Draft202012Validator(schema, format_checker=jsonschema.FormatChecker()).validate(result['body'])
    else:
        assert result['body'] is None
    count += 1
assert {'/v1/health','/v1/capabilities','/v1/playback/sessions','/v1/playback/sessions/{sessionId}'} <= {r['route'] for r in report}
print(f'{count} recorded API responses validated against unchanged OpenAPI 3.1 snapshot')
