#!/bin/bash

__dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

source "${__dir}/header.sh"

testground plan import --from ./plans --name testground

pushd $TEMPDIR

testground healthcheck --runner local:docker --fix

testground run single \
    --plan=testground/example-rust \
    --testcase=tcp-connect \
    --builder=docker:generic \
    --runner=local:docker \
    --instances=2 \
    --wait

echo "terminating remaining containers"
testground terminate --runner local:docker
