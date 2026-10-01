import pathlib
import subprocess
import sys

files = [str(p) for base in ("cmd", "internal") for p in pathlib.Path(base).rglob("*.go")]
for command in (["gofmt", "-l", *files], [".bin/goimports", "-l", *files]):
    result = subprocess.run(command, check=True, capture_output=True, text=True)
    if result.stdout:
        print(result.stdout)
        sys.exit("run make format")
result = subprocess.run(["go", "fix", "-diff", "./..."], check=True, capture_output=True, text=True)
if result.stdout or result.stderr:
    print(result.stdout, result.stderr)
    sys.exit("run make format")
