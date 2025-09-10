### Aperture IDOR Tester

Automates detection of low-hanging IDOR issues by generating cross-user access attempts from an OpenAPI spec and a YAML config of users.

### Features
- Parses OpenAPI spec to discover endpoints, params, and request bodies
- Builds control vs test requests across user pairs
- Supports header or cookie auth per user
- Text log by default; optional JSONL (-jsonl) with full request/response details + console summary

### Install / Build
- Go install
```sh
go install github.com/yansol0/aperture@latest
```

- Build from source
```bash
# in repo root
go mod tidy
go build
# or run directly
go run . --spec <openapi.(json|yaml)> --config config.yaml --base-url https://api.example.com --out aperture_log.jsonl --jsonl -v
```

### Usage
```bash
aperture --spec <path-or-url> --config config.yaml [--base-url https://api.example.com] [--out aperture_log.(txt|jsonl)] [--timeout 20] [--jsonl] [-v] [--list]
# short forms are also supported, e.g.:
aperture -s <path-or-url> -c config.yaml -b https://api.example.com -o aperture_log.jsonl -t 20 -j -v -l
```
- `-s, --spec`: OpenAPI 3 spec file path or URL (JSON or YAML)
- `-c, --config`: YAML config with users and fields
- `-b, --base-url`: Overrides spec servers[0].URL
- `-o, --out`: Output log file path (default `aperture_log.txt`). With `-j, --jsonl`, writes JSON Lines to this path.
- `-t, --timeout`: HTTP timeout seconds (default 20)
- `-j, --jsonl`: Write JSON Lines output instead of text
- `-v, --verbose`: Verbose
- `-l, --list`: List unique path parameter names from the provided spec and exit
- `-h, --help`: Show help

#### List path parameters
List all path parameters (de-duplicated), one per line:
```bash
aperture --spec /path/to/openapi.json --list
# or
aperture -s /path/to/openapi.json -l
```
Example output for a route like `https://console.neon.tech/api/v2/projects/{project_id}/endpoints/{endpoint_id}`:
```text
endpoint_id
project_id
```

### Config (YAML)
```yaml
users:
  - name: user1
    auth:
      type: header
      value: "Bearer token1"
      # header_name: Authorization  # optional; defaults to Authorization
    fields:
      user_id: "123"
      project_id: "abc"

  - name: user2
    auth:
      type: cookie
      value: "sessionid=xyz"
    fields:
      user_id: "456"
      project_id: "def"
```
- `fields` must map to parameter names and/or JSON body properties in the spec (e.g., path/query/header params, or body object properties for `application/json`).

### How it works
- For each endpoint and method:
  - Identify required path/query/header/body fields from the spec
  - If at least two users have the required fields: build two requests per pair
    - Control: creds=userA, identifiers=userA
    - Test: creds=userB, identifiers=userA
  - Send both, compare responses and flag potential IDOR when test succeeds (2xx) or mirrors control unexpectedly

### Output
- Console:
```text
[IDOR FOUND] GET /projects/{project_id}/users/{user_id}
  creds=user2, object=user1
Completed. N endpoints tested, M potential IDOR findings.
```
- JSONL log (`-out` with `-jsonl`): one line per test with request/response details and result label:
```json
{"endpoint":"/projects/{project_id}/users/{user_id}","method":"GET","control":{...},"test":{...},"result":"IDOR FOUND"}
```

### Notes
- Focuses on direct object reference checks; does not fuzz or do complex mutations
- Skips endpoints where required fields are missing from the config
- Treats `application/json` request bodies with object schemas; copies matching fields from `fields`

## Test environment (dockerized vulnerable API)

To run an end-to-end scan using Docker (requires Docker permissions):

```bash
chmod +x test/run_e2e.sh
./test/run_e2e.sh
```

This uses a Flask + SQLite API with two hardcoded API keys:
- X-API-Key: KEY_ALICE (user_id: alice; note_id: 1)
- X-API-Key: KEY_BOB (user_id: bob; note_id: 2)

The OpenAPI spec lives at `test/openapi.json` and the scanner config at `test/test_config.yml`. The script writes logs to `test/output.jsonl` and prints a summary to the console.
