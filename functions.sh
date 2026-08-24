#!/bin/sh
check() {
eval $@
if [ $? -ne 0 ]; then
echo "Command $1 failed to execute."
exit 10
fi
}
git_version() {
version=$(git describe --tags "$(git rev-list --tags --max-count=1)" 2>/dev/null || true)
if [ -n "$version" ]; then
echo "$version"
else
git rev-parse --short HEAD
fi
}
gen_cert() {
check go run . -launch=false -log-level=-1 -conf-read=false -gen-cert-file $(pwd)/cert.pem
}
